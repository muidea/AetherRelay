package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Stops are local output controls, never fields of the upstream Responses body.
const maxLocalStopSequences = 16
const maxLocalStopBytes = 256

type anthropicStopsContextKey struct{}

func parseAnthropicStops(body map[string]any) ([]string, error) {
	raw, exists := body["stop_sequences"]
	if !exists {
		return nil, nil
	}
	values, ok := raw.([]any)
	if !ok || len(values) > maxLocalStopSequences {
		return nil, &conversionLocationError{Path: "stop_sequences", Err: fmt.Errorf("stop_sequences must be an array of at most %d strings", maxLocalStopSequences)}
	}
	stops := make([]string, 0, len(values))
	for i, value := range values {
		stop, ok := value.(string)
		if !ok || stop == "" || len(stop) > maxLocalStopBytes || !utf8.ValidString(stop) {
			return nil, &conversionLocationError{Path: fmt.Sprintf("stop_sequences[%d]", i), Err: fmt.Errorf("stop_sequences requires nonempty UTF-8 strings of at most %d bytes", maxLocalStopBytes)}
		}
		stops = append(stops, stop)
	}
	return stops, nil
}

// Called only after request conversion has validated the field.
func withAnthropicStops(r *http.Request, body map[string]any) *http.Request {
	stops, _ := parseAnthropicStops(body)
	return r.WithContext(context.WithValue(r.Context(), anthropicStopsContextKey{}, stops))
}

func anthropicStops(r *http.Request) []string {
	stops, _ := r.Context().Value(anthropicStopsContextKey{}).([]string)
	return stops
}

type localStopMatcher struct {
	stops   []string
	pending string
	matched string
}

// Select the first completed match (earliest end, then request order), independent
// of SSE chunk boundaries. Retain only a possible stop prefix between chunks.
func (m *localStopMatcher) push(text string) string {
	if m.matched != "" {
		return ""
	}
	text = m.pending + text
	m.pending = ""
	start, end := -1, len(text)+1
	for _, stop := range m.stops {
		if i := strings.Index(text, stop); i >= 0 && i+len(stop) < end {
			start, end, m.matched = i, i+len(stop), stop
		}
	}
	if start >= 0 {
		return text[:start]
	}
	keep := 0
	for _, stop := range m.stops {
		for n := min(len(text), len(stop)-1); n > keep; n-- {
			if strings.HasSuffix(text, stop[:n]) {
				keep = n
				break
			}
		}
	}
	m.pending = text[len(text)-keep:]
	return text[:len(text)-keep]
}

func (m *localStopMatcher) flush() string {
	text := m.pending
	m.pending = ""
	return text
}

func applyAnthropicStops(body []byte, stops []string) ([]byte, error) {
	if len(stops) == 0 {
		return body, nil
	}
	// Keep usage and all other fields as raw JSON, including integer precision.
	var message map[string]json.RawMessage
	if err := json.Unmarshal(body, &message); err != nil {
		return nil, err
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(message["content"], &blocks); err != nil {
		return nil, err
	}
	matcher := localStopMatcher{stops: stops}
	for i, block := range blocks {
		var typ, text string
		_ = json.Unmarshal(block["type"], &typ)
		if typ != "text" {
			continue
		}
		if err := json.Unmarshal(block["text"], &text); err != nil {
			return nil, err
		}
		text = matcher.push(text) + matcher.flush()
		block["text"], _ = json.Marshal(text)
		if matcher.matched != "" {
			blocks = blocks[:i+1]
			message["stop_reason"], _ = json.Marshal("stop_sequence")
			message["stop_sequence"], _ = json.Marshal(matcher.matched)
			break
		}
	}
	message["content"], _ = json.Marshal(blocks)
	return json.Marshal(message)
}

// Drain and validate the upstream terminal event even after a local stop. This
// preserves actual billable usage and does not turn a later upstream fault into
// success. Subsequent tool blocks must not become actionable client tool calls.
func withAnthropicStopMapper(mapper conversionSSEMapper, stops []string) conversionSSEMapper {
	if len(stops) == 0 {
		return mapper
	}
	matcher := localStopMatcher{stops: stops}
	opened := map[int]bool{}
	inputBytes := 0
	return func(payload []byte, state *textConversionStreamState) ([]map[string]any, error) {
		inputBytes += len(payload)
		if inputBytes > maxConversionSSEBytes {
			return nil, fmt.Errorf("stop_sequences upstream SSE exceeds %d bytes", maxConversionSSEBytes)
		}
		events, err := mapper(payload, state)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(events))
		for _, event := range events {
			typ, _ := event["type"].(string)
			index, _ := event["index"].(int)
			switch typ {
			case "content_block_start":
				if matcher.matched != "" {
					continue
				}
				opened[index] = true
			case "content_block_delta":
				if matcher.matched != "" {
					continue
				}
				delta, _ := event["delta"].(map[string]any)
				if delta["type"] == "text_delta" {
					text, _ := delta["text"].(string)
					text = matcher.push(text)
					if text == "" {
						continue
					}
					delta["text"] = text
				}
			case "content_block_stop":
				if !opened[index] {
					continue
				}
				if text := matcher.flush(); text != "" {
					out = append(out, map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": text}})
				}
				delete(opened, index)
			case "message_delta":
				if matcher.matched != "" {
					delta, _ := event["delta"].(map[string]any)
					delta["stop_reason"], delta["stop_sequence"] = "stop_sequence", matcher.matched
				}
			}
			out = append(out, event)
		}
		// The Responses mapper coalesces visible text into downstream block 0.
		// Do not let that coalescing invent a match across source text parts.
		var source struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &source); err != nil {
			return nil, err
		}
		if source.Type == "response.output_text.done" || source.Type == "response.content_part.done" {
			if text := matcher.flush(); text != "" {
				out = append(out, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
			}
		}
		return out, nil
	}
}
