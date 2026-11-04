package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// chatGPTResponse owns one identity and creation timestamp across all events.
type chatGPTResponse struct {
	ID        string
	CreatedAt int64
	Body      map[string]any
}

func newChatGPTResponse(body map[string]any) chatGPTResponse {
	return chatGPTResponse{ID: responseIdentifier(""), CreatedAt: time.Now().Unix(), Body: body}
}

func (c chatGPTResponse) messageID() string { return "msg_" + strings.TrimPrefix(c.ID, "resp_") }

func (c chatGPTResponse) payload(model string, output []any, tok tokenUsage, status string) map[string]any {
	if output == nil {
		output = []any{}
	}
	metadata := c.Body["metadata"]
	if metadata == nil {
		metadata = map[string]any{}
	}
	tools := c.Body["tools"]
	if tools == nil {
		tools = []any{}
	}
	result := map[string]any{
		"id": c.ID, "object": "response", "created_at": c.CreatedAt, "completed_at": nil, "status": status, "model": model, "output": output,
		"error": nil, "incomplete_details": nil, "usage": nil, "instructions": c.Body["instructions"], "metadata": metadata,
		"background": false, "parallel_tool_calls": false, "store": false, "previous_response_id": nil, "tools": tools,
		"text": map[string]any{"format": map[string]any{"type": "text"}}, "truncation": "disabled",
	}
	if choice := c.Body["tool_choice"]; choice != nil {
		result["tool_choice"] = choice
	}
	if reasoning := c.Body["reasoning"]; reasoning != nil {
		result["reasoning"] = reasoning
	}
	if status == "completed" || status == "incomplete" {
		result["usage"] = map[string]any{"input_tokens": tok.PromptTokens, "output_tokens": tok.CompletionTokens, "total_tokens": tok.TotalTokens}
	}
	if status == "completed" {
		result["completed_at"] = time.Now().Unix()
	}
	if status == "incomplete" {
		result["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return result
}

func (c chatGPTResponse) failed(model string, output []any, fail *streamFail) map[string]any {
	result := c.payload(model, output, tokenUsage{}, "failed")
	result["error"] = map[string]any{"code": chatGPTFailureCode(fail), "message": "chatgpt web response failed: " + chatGPTFailureCode(fail)}
	return result
}

func chatGPTResponsePart(text string, annotations []openAIURLCitation) map[string]any {
	return map[string]any{"type": "output_text", "text": text, "annotations": searchResponseAnnotations(annotations), "logprobs": []any{}}
}

func chatGPTResponseMessage(id, text string, annotations []openAIURLCitation, status string) map[string]any {
	return map[string]any{"id": id, "type": "message", "status": status, "role": "assistant", "content": []any{chatGPTResponsePart(text, annotations)}}
}

// One writer implements sequencing, headers and terminal exclusion for both paths.
type chatGPTResponseStream struct {
	writer   http.ResponseWriter
	archive  strings.Builder
	sequence int
	started  bool
	terminal bool
}

func (s *chatGPTResponseStream) write(kind string, payload map[string]any) error {
	if s.terminal {
		return fmt.Errorf("response stream is already terminal")
	}
	payload["type"] = kind
	payload["sequence_number"] = s.sequence
	s.sequence++
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("event: %s\ndata: %s\n\n", kind, encoded)
	s.archive.WriteString(line)
	if kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed" {
		s.terminal = true
	}
	n, err := s.writer.Write([]byte(line))
	if err != nil {
		return err
	}
	if n != len(line) {
		return io.ErrShortWrite
	}
	if f, ok := s.writer.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (s *chatGPTResponseStream) start(response chatGPTResponse, model string) error {
	if s.started {
		return nil
	}
	prepareSSEHeaders(s.writer.Header())
	setChatGPTUsageHeaders(s.writer.Header())
	s.writer.WriteHeader(http.StatusOK)
	s.started = true
	for _, kind := range []string{"response.created", "response.in_progress"} {
		if err := s.write(kind, map[string]any{"response": response.payload(model, nil, tokenUsage{}, "in_progress")}); err != nil {
			return err
		}
	}
	return nil
}

func (s *chatGPTResponseStream) addText(itemID string, index int) error {
	item := chatGPTResponseMessage(itemID, "", nil, "in_progress")
	item["content"] = []any{}
	if err := s.write("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
		return err
	}
	return s.write("response.content_part.added", map[string]any{"item_id": itemID, "output_index": index, "content_index": 0, "part": chatGPTResponsePart("", nil)})
}

func (s *chatGPTResponseStream) delta(itemID string, index int, text string) error {
	return s.write("response.output_text.delta", map[string]any{"item_id": itemID, "output_index": index, "content_index": 0, "delta": text, "logprobs": []any{}})
}

func (s *chatGPTResponseStream) finishText(itemID string, index int, text string, annotations []openAIURLCitation, status string) error {
	for i, annotation := range annotations {
		if err := s.write("response.output_text.annotation.added", map[string]any{"item_id": itemID, "output_index": index, "content_index": 0, "annotation_index": i, "annotation": annotation}); err != nil {
			return err
		}
	}
	if err := s.write("response.output_text.done", map[string]any{"item_id": itemID, "output_index": index, "content_index": 0, "text": text, "logprobs": []any{}}); err != nil {
		return err
	}
	if err := s.write("response.content_part.done", map[string]any{"item_id": itemID, "output_index": index, "content_index": 0, "part": chatGPTResponsePart(text, annotations)}); err != nil {
		return err
	}
	return s.write("response.output_item.done", map[string]any{"output_index": index, "item": chatGPTResponseMessage(itemID, text, annotations, status)})
}

func setChatGPTUsageHeaders(header http.Header) {
	header.Set("X-AetherRelay-Usage-Estimated", "true")
	header.Set("X-AetherRelay-Usage-Scope", "text-only")
}

func (h *Handler) writeChatGPTResponseJSON(w http.ResponseWriter, r *http.Request, started time.Time, provider, model string, payload map[string]any, status int, tok tokenUsage, fail *streamFail) {
	round := archiveRoundFromContext(r.Context())
	setChatGPTUsageHeaders(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		fail = newStreamFailWithCode(streamKindClientWrite, "client_write", "client write failed", err, false)
	} else if raw, err := json.Marshal(payload); err == nil {
		_ = h.writeArchiveResponse(round, "response.json", append(raw, '\n'))
	}
	h.settleChatGPTWeb(round, r, provider, model, false, status, time.Since(started), tok, fail)
}
