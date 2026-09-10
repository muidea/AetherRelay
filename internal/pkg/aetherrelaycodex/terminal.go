// Package aetherrelaycodex contains owner-neutral Codex protocol rules shared
// by the upstream block and proxy adapters.
package aetherrelaycodex

import (
	"bytes"
	"encoding/json"
	"strings"
)

// MeaningfulOutputEvent reports whether a Responses event carries generated
// output. Prelude, empty skeleton and zero usage events are not output.
func MeaningfulOutputEvent(payload []byte) bool {
	var event struct {
		Type      string          `json:"type"`
		Delta     json.RawMessage `json:"delta"`
		Text      json.RawMessage `json:"text"`
		Summary   json.RawMessage `json:"summary"`
		Arguments json.RawMessage `json:"arguments"`
		Part      json.RawMessage `json:"part"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return false
	}
	switch event.Type {
	case "response.output_text.delta", "response.reasoning.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta":
		return rawSemantic(event.Delta)
	case "response.output_text.done", "response.reasoning_text.done", "response.reasoning_summary_text.done":
		return rawSemantic(event.Text)
	case "response.reasoning.done":
		return rawSemantic(event.Text) || rawSemantic(event.Summary)
	case "response.function_call_arguments.done":
		return rawSemantic(event.Arguments)
	case "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return contentPartHasText(event.Part)
	default:
		return false
	}
}

func contentPartHasText(raw json.RawMessage) bool {
	var part struct {
		Type string          `json:"type"`
		Text json.RawMessage `json:"text"`
	}
	if json.Unmarshal(raw, &part) != nil {
		return false
	}
	switch part.Type {
	case "output_text", "reasoning_text", "reasoning_summary", "summary_text":
		return rawSemantic(part.Text)
	default:
		return false
	}
}

// EmptyIncomplete reports the narrow silent-failure shape documented by
// CP-STREAM-014. Missing usage is deliberately not classified as empty.
func EmptyIncomplete(payload []byte, outputItems int, sawOutput bool) bool {
	if sawOutput || outputItems > 0 {
		return false
	}
	var event struct {
		Type     string `json:"type"`
		Response struct {
			Output []json.RawMessage `json:"output"`
			Usage  struct {
				OutputTokens json.RawMessage `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil || event.Type != "response.incomplete" || len(event.Response.Output) > 0 {
		return false
	}
	return exactZero(event.Response.Usage.OutputTokens)
}

// EmptyIncompleteResponse applies CP-STREAM-014 to a native, non-event
// Response object.
func EmptyIncompleteResponse(payload []byte) bool {
	var response struct {
		Status string            `json:"status"`
		Output []json.RawMessage `json:"output"`
		Usage  struct {
			OutputTokens json.RawMessage `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &response) != nil || response.Status != "incomplete" || len(response.Output) > 0 {
		return false
	}
	return exactZero(response.Usage.OutputTokens)
}

func exactZero(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("0"))
}

func rawSemantic(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`)) || bytes.Equal(trimmed, []byte("[]")) || bytes.Equal(trimmed, []byte("{}")) {
		return false
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		return strings.TrimSpace(text) != ""
	}
	return true
}
