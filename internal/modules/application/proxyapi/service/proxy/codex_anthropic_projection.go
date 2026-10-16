package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"

	archive "aetherrelay/internal/pkg/aetherrelayarchive"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
)

type responseProjectionError struct {
	Path, EventType, Phase string
	Err                    error
}

func (e *responseProjectionError) Error() string { return e.Path + ": " + e.Err.Error() }
func (e *responseProjectionError) Unwrap() error { return e.Err }

func equivalentResponsesToolArguments(a, b string) bool {
	if a == b {
		return true
	}
	var left, right any
	return decodeCodexJSON([]byte(a), &left) == nil && decodeCodexJSON([]byte(b), &right) == nil && reflect.DeepEqual(left, right)
}

func responsesCompletionUsage(body []byte) tokenUsage {
	var envelope struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return tokenUsage{}
	}
	u, _ := usageFromRaw(envelope.Usage)
	return u
}

func recordCodexProjectionError(round *archive.Round, stage string, err error) {
	if round == nil {
		return
	}
	round.ConversionErrorStage = stage
	round.ConversionErrorDetail = projectionDiagnostic(err.Error())
	var projection *responseProjectionError
	if errors.As(err, &projection) {
		round.ConversionErrorPath = projectionDiagnostic(projection.Path)
		round.ConversionErrorEventType = projectionDiagnostic(projection.EventType)
		round.ConversionErrorPhase = projectionDiagnostic(projection.Phase)
	}
}

// This is the adapter's input, not the transport's raw wire representation.
func (h *Handler) archiveCodexProjection(round *archive.Round, body []byte, stream, truncated bool) {
	if round == nil || !round.FullContent() {
		return
	}
	index := max(1, len(round.UpstreamAttempts))
	name, format := fmt.Sprintf("upstream_response_%03d.body.json", index), "aggregated_response_json"
	if stream {
		name, format = fmt.Sprintf("upstream_response_%03d.projection.sse", index), "responses_sse"
	}
	if err := h.writeArchiveResponse(round, name, body); err == nil {
		round.UpstreamProjectionPath, round.UpstreamProjectionFormat = name, format
		round.UpstreamProjectionTruncated = truncated
	} else {
		log.Printf("archive Codex response projection: %v", err)
	}
}

type codexProjectionChunk struct {
	text    string
	tool    *responsesAnthropicToolState
	textEnd bool
}

type codexProjectionArchive struct {
	body      bytes.Buffer
	truncated bool
}

func (a *codexProjectionArchive) capture(payload string, limit int) {
	if a.truncated {
		return
	}
	if a.body.Len()+len(payload)+8 > limit {
		a.truncated = true
		return
	}
	a.body.WriteString("data: ")
	a.body.WriteString(payload)
	a.body.WriteString("\n\n")
}

// Share validation/usage with the bounded mapper, but emit tools in their
// original position. A parallel tool that completes early waits for its peers
// ahead of it; following text waits as well. Text continues to stream promptly.
func codexResponsesEventToAnthropic(capability config.ConversionCapability) conversionSSEMapper {
	var pending []codexProjectionChunk
	var emitted []any
	var emittedText strings.Builder
	flushEmittedText := func() {
		if emittedText.Len() > 0 {
			emitted = append(emitted, map[string]any{"type": "text", "text": emittedText.String()})
			emittedText.Reset()
		}
	}
	nextBlock, textBlock, inputBytes := 0, -1, 0
	closeText := func(out []map[string]any) []map[string]any {
		if textBlock >= 0 {
			out = append(out, map[string]any{"type": "content_block_stop", "index": textBlock})
			textBlock = -1
		}
		return out
	}
	drain := func() ([]map[string]any, error) {
		var out []map[string]any
		for len(pending) > 0 {
			chunk := pending[0]
			if chunk.textEnd {
				out = closeText(out)
			} else if chunk.tool != nil {
				out = closeText(out)
				if !chunk.tool.Completed {
					break
				}
				events, err := appendResponsesAnthropicToolBlocks(nil, map[int]*responsesAnthropicToolState{0: chunk.tool})
				if err != nil {
					return nil, err
				}
				for _, event := range events {
					event["index"] = nextBlock
				}
				nextBlock++
				out = append(out, events...)
				var input any = map[string]any{}
				if chunk.tool.Arguments != "" {
					if err := decodeCodexJSON([]byte(chunk.tool.Arguments), &input); err != nil {
						return nil, err
					}
				}
				flushEmittedText()
				emitted = append(emitted, map[string]any{"type": "tool_use", "id": chunk.tool.CallID, "name": chunk.tool.Name, "input": input})
			} else {
				if textBlock < 0 {
					textBlock = nextBlock
					nextBlock++
					out = append(out, map[string]any{"type": "content_block_start", "index": textBlock, "content_block": map[string]any{"type": "text", "text": ""}})
				}
				out = append(out, map[string]any{"type": "content_block_delta", "index": textBlock, "delta": map[string]any{"type": "text_delta", "text": chunk.text}})
				emittedText.WriteString(chunk.text)
			}
			pending[0] = codexProjectionChunk{}
			pending = pending[1:]
		}
		return out, nil
	}
	return func(payload []byte, state *textConversionStreamState) (out []map[string]any, resultErr error) {
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, &responseProjectionError{Path: "event", Err: err}
		}
		typ, _ := event["type"].(string)
		item, _ := event["item"].(map[string]any)
		phase, _ := item["phase"].(string)
		path := "event"
		defer func() {
			if resultErr != nil {
				var projection *responseProjectionError
				if !errors.As(resultErr, &projection) {
					resultErr = &responseProjectionError{Path: path, EventType: typ, Phase: phase, Err: resultErr}
				} else if projection.EventType == "" {
					projection.EventType = typ
				}
			}
		}()
		inputBytes += len(payload)
		if inputBytes > maxConversionSSEBytes {
			return nil, fmt.Errorf("conversion SSE input exceeds limit")
		}
		if state == nil || state.Completed {
			return nil, fmt.Errorf("stream event received outside active response")
		}
		if typ == "response.metadata" || typ == "codex.response.metadata" {
			state.markIgnored("response_metadata")
			return nil, nil
		}
		if typ == "codex.rate_limits" {
			state.markIgnored("codex_rate_limits")
			return nil, nil
		}
		if typ == "response.content_part.added" {
			state.ResponseTextEmitted = false
		}
		if typ == "response.output_item.added" || typ == "response.output_item.done" {
			var ignored []string
			item, ignored = normalizeResponsesOutputItem(item)
			for _, feature := range ignored {
				state.markIgnored(feature)
			}
			path = "item"
			if err := validateResponsesOutputItemFields(item); err != nil {
				var location *conversionLocationError
				if errors.As(err, &location) {
					path += "." + location.Path
				}
				return nil, err
			}
			switch item["type"] {
			case "message", "function_call", "reasoning":
			default:
				path = "item.type"
				return nil, fmt.Errorf("unsupported response output item type")
			}
			event["item"] = item
			// The generic state machine receives the same normalized projection.
			// The archive has already captured the original payload.
			payload, _ = json.Marshal(event)
		}
		if (typ == "response.output_item.added" || typ == "response.output_item.done") && item["type"] == "function_call" {
			if value, exists := item["arguments"]; exists {
				arguments, ok := value.(string)
				if !ok || len(arguments) > maxConversionToolArgumentBytes {
					path = "item.arguments"
					return nil, fmt.Errorf("invalid function arguments")
				}
			}
		}
		if typ == "response.function_call_arguments.done" || (typ == "response.output_item.done" && item["type"] == "function_call") {
			index, err := responsesAnthropicToolOutputIndex(event, state.ResponsesTools)
			if err != nil {
				return nil, err
			}
			arguments, exists := event["arguments"]
			path = "event.arguments"
			if typ == "response.output_item.done" {
				arguments, exists = item["arguments"]
				path = "item.arguments"
			}
			if exists {
				value, ok := arguments.(string)
				if !ok || len(value) > maxConversionToolArgumentBytes {
					return nil, fmt.Errorf("invalid function arguments")
				}
				if value != "" && !json.Valid([]byte(value)) {
					return nil, fmt.Errorf("invalid completed function arguments")
				}
				if tool := state.ResponsesTools[index]; tool != nil && tool.ArgumentsEmitted && !equivalentResponsesToolArguments(tool.Arguments, value) {
					return nil, fmt.Errorf("completed function arguments differ from streamed arguments")
				}
			}
		}
		if (typ == "response.output_item.added" || typ == "response.output_item.done") && item["type"] == "message" {
			if value, exists := item["phase"]; exists && value != "commentary" && value != "final_answer" {
				path = "item.phase"
				return nil, fmt.Errorf("unsupported message phase")
			}
			if phase == "commentary" {
				state.markIgnored("message_phase")
			}
			if typ == "response.output_item.added" {
				state.ResponseTextEmitted = false
			}
		}
		// The terminal object is also authoritative for unsupported response
		// fields and for streams that only contain a complete terminal payload.
		var projected map[string]any
		if typ == "response.completed" || typ == "response.incomplete" {
			response, _ := event["response"].(map[string]any)
			if u, ok := usageFromMap(response["usage"]); ok {
				state.CacheUsage, state.InputTokens, state.OutputTokens = u, u.PromptTokens, u.CompletionTokens
			}
			if output, ok := response["output"].([]any); ok && len(output) > 0 {
				raw, _ := json.Marshal(response)
				converted, _, ignored, err := convertCodexResponsesToAnthropic(raw, state.Model, capability)
				if err != nil {
					return nil, err
				}
				for _, feature := range ignored {
					state.markIgnored(feature)
				}
				_ = decodeCodexJSON(converted, &projected)
			}
		}
		events, err := responsesEventToAnthropicWithCapabilityState(payload, state, capability)
		if err != nil {
			return nil, err
		}
		if typ == "response.output_item.added" && item["type"] == "function_call" {
			index, err := responsesAnthropicToolOutputIndex(event, state.ResponsesTools)
			if err != nil {
				return nil, err
			}
			pending = append(pending, codexProjectionChunk{tool: state.ResponsesTools[index]})
		}
		for _, value := range events {
			switch value["type"] {
			case "message_start":
				out = append(out, value)
			case "content_block_delta":
				delta, _ := value["delta"].(map[string]any)
				if delta["type"] == "text_delta" {
					text, _ := delta["text"].(string)
					pending = append(pending, codexProjectionChunk{text: text})
				}
			}
		}
		// Preserve source text boundaries even when a preceding unfinished
		// tool delays text emission. Local stops must not match across parts.
		if typ == "response.output_text.done" || typ == "response.content_part.done" {
			pending = append(pending, codexProjectionChunk{textEnd: true})
		}
		drained, err := drain()
		if err != nil {
			return nil, err
		}
		out = append(out, drained...)
		if state.Completed {
			flushEmittedText()
			if len(pending) > 0 {
				return nil, fmt.Errorf("response completed with pending output")
			}
			if nextBlock > 0 && projected != nil && !reflect.DeepEqual(normalizeProjectionContent(emitted), normalizeProjectionContent(projected["content"].([]any))) {
				path = "response.output"
				return nil, fmt.Errorf("terminal content differs from streamed output")
			}
			// No incremental content: project the terminal object's content once.
			if nextBlock == 0 && projected != nil {
				for _, raw := range projected["content"].([]any) {
					block := raw.(map[string]any)
					out = append(out, map[string]any{"type": "content_block_start", "index": nextBlock, "content_block": block}, map[string]any{"type": "content_block_stop", "index": nextBlock})
					nextBlock++
				}
			}
			if nextBlock == 0 {
				pending = append(pending, codexProjectionChunk{})
				drained, _ = drain()
				out = append(out, drained...)
			}
			out = closeText(out)
			for _, value := range events {
				if value["type"] == "message_delta" || value["type"] == "message_stop" {
					if projected != nil && value["type"] == "message_delta" {
						value["delta"].(map[string]any)["stop_reason"] = projected["stop_reason"]
					}
					out = append(out, value)
				}
			}
		}
		return out, nil
	}
}

func appendProjectionText(content []any, text string) []any {
	if text == "" {
		return content
	}
	if len(content) > 0 {
		last := content[len(content)-1].(map[string]any)
		if last["type"] == "text" {
			last["text"] = last["text"].(string) + text
			return content
		}
	}
	return append(content, map[string]any{"type": "text", "text": text})
}

func normalizeProjectionContent(content []any) []any {
	var normalized []any
	for _, raw := range content {
		block := raw.(map[string]any)
		if block["type"] == "text" {
			normalized = appendProjectionText(normalized, block["text"].(string))
		} else {
			normalized = append(normalized, block)
		}
	}
	return normalized
}

// Keep diagnostics bounded independently of archived response bodies.
func projectionDiagnostic(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, value)
	if len(value) > 512 {
		return value[:512]
	}
	return value
}
