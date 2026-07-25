package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgpttext"
)

func (h *Handler) handleChatGPTWebChatCompletions(w http.ResponseWriter, r *http.Request, started time.Time, provider, model string, stream bool, body map[string]any) {
	h.cfgMu.RLock()
	executor := h.chatGPTText
	h.cfgMu.RUnlock()
	if executor == nil {
		writeAPIError(w, http.StatusServiceUnavailable, APIError{Code: ErrorCodeProviderUnavailable, Message: "chatgpt web executor is unavailable", Model: model})
		return
	}
	request, err := chatGPTTextRequest(model, body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: ErrorCodeInvalidRequest, Message: err.Error(), Model: model})
		return
	}
	if !stream {
		result, err := executor.Complete(r.Context(), request)
		if err != nil {
			writeAPIError(w, http.StatusBadGateway, APIError{Code: ErrorCodeUpstreamUnavailable, Message: "chatgpt web completion failed: " + chatGPTFailureClass(err), Model: model})
			return
		}
		_ = json.NewEncoder(w).Encode(openAIChatCompletion{ID: "chatcmpl-" + result.ConversationID, Object: "chat.completion", Created: time.Now().Unix(), Model: model, Choices: []openAIChatChoice{{Index: 0, Message: openAIChatMessage{Role: "assistant", Content: result.Text}, FinishReason: "stop"}}})
		return
	}
	streamStarted := false
	startSSE := func() http.Flusher {
		if !streamStarted {
			prepareSSEHeaders(w.Header())
			w.WriteHeader(http.StatusOK)
			streamStarted = true
		}
		flusher, _ := w.(http.Flusher)
		return flusher
	}
	_, err = executor.Stream(r.Context(), request, func(delta chatgpttext.Delta) error {
		if delta.Text == "" {
			return nil
		}
		flusher := startSSE()
		payload, err := json.Marshal(openAIChatChunk{ID: "chatcmpl-chatgptweb", Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: model, Choices: []openAIChatChunkChoice{{Index: 0, Delta: openAIChatMessage{Content: delta.Text}}}})
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		if !streamStarted {
			writeAPIError(w, http.StatusBadGateway, APIError{Code: ErrorCodeUpstreamUnavailable, Message: "chatgpt web stream failed: " + chatGPTFailureClass(err), Model: model})
		}
		return
	}
	flusher := startSSE()
	if err == nil {
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func chatGPTFailureClass(err error) string {
	message := err.Error()
	for _, class := range []string{"invalid_token", "rate_limit", "timeout", "tls", "upstream"} {
		if strings.Contains(message, class) {
			return class
		}
	}
	return "unclassified"
}

func chatGPTTextRequest(model string, body map[string]any) (chatgpttext.Request, error) {
	raw, ok := body["messages"].([]any)
	if !ok || len(raw) == 0 {
		return chatgpttext.Request{}, fmt.Errorf("messages is required")
	}
	request := chatgpttext.Request{Model: model}
	if effort, ok := body["reasoning_effort"].(string); ok {
		request.ThinkingEffort = effort
	}
	for index, item := range raw {
		message, ok := item.(map[string]any)
		if !ok {
			return chatgpttext.Request{}, fmt.Errorf("messages[%d] is invalid", index)
		}
		role, _ := message["role"].(string)
		content, _ := message["content"].(string)
		if role = strings.TrimSpace(role); role == "" || strings.TrimSpace(content) == "" {
			return chatgpttext.Request{}, fmt.Errorf("messages[%d] requires text role and content", index)
		}
		request.Messages = append(request.Messages, chatgpttext.Message{Role: role, Content: content})
	}
	return request, nil
}

type openAIChatMessage struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content"`
}
type openAIChatChoice struct {
	Index        int               `json:"index"`
	Message      openAIChatMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}
type openAIChatCompletion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []openAIChatChoice `json:"choices"`
}
type openAIChatChunkChoice struct {
	Index int               `json:"index"`
	Delta openAIChatMessage `json:"delta"`
}
type openAIChatChunk struct {
	ID      string                  `json:"id"`
	Object  string                  `json:"object"`
	Created int64                   `json:"created"`
	Model   string                  `json:"model"`
	Choices []openAIChatChunkChoice `json:"choices"`
}
