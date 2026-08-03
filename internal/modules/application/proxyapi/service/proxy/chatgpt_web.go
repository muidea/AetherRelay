package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptfail"
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgpttext"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	archive "ai-proxy/internal/pkg/aiproxyarchive"
	"ai-proxy/internal/pkg/chatgptimageinput"
	"ai-proxy/internal/pkg/chatgpttokenusage"
)

func (h *Handler) handleChatGPTWebChatCompletions(w http.ResponseWriter, r *http.Request, started time.Time, provider, model string, stream bool, body map[string]any) {
	round := archiveRoundFromContext(r.Context())
	ignored, compatibilityErr := chatGPTWebChatCompatibility(body)
	if compatibilityErr != nil {
		compatibilityErr.Model = model
		fail := newStreamFailWithCode(streamKindError, compatibilityErr.Code, compatibilityErr.Code+": "+compatibilityErr.Message, fmt.Errorf("%s", compatibilityErr.Message), false)
		h.writeChatGPTWebAPIError(w, round, r, started, provider, model, stream, http.StatusBadRequest, *compatibilityErr, fail, tokenUsage{})
		return
	}
	searchInvocation, searchErr := chatGPTWebChatSearchInvocation(body)
	if searchErr != nil {
		searchErr.Model = model
		fail := newStreamFailWithCode(streamKindError, searchErr.Code, searchErr.Code+": "+searchErr.Message, fmt.Errorf("%s", searchErr.Message), false)
		h.writeChatGPTWebAPIError(w, round, r, started, provider, model, stream, http.StatusBadRequest, *searchErr, fail, tokenUsage{})
		return
	}
	if searchInvocation.Enabled {
		searchInvocation.Ignored = uniqueSortedFeatures(append(searchInvocation.Ignored, ignored...))
		h.handleChatGPTWebSearchChat(w, r, started, provider, model, stream, searchInvocation)
		return
	}
	settleModel := strings.TrimSpace(model)

	h.cfgMu.RLock()
	executor := h.chatGPTText
	h.cfgMu.RUnlock()
	if executor == nil {
		apiErr := APIError{Code: ErrorCodeProviderUnavailable, Message: "chatgpt web executor is unavailable", Model: model}
		h.writeChatGPTWebAPIError(w, round, r, started, provider, model, stream, http.StatusServiceUnavailable, apiErr, streamFailFromKind(chatgptfail.KindProviderUnavailable, apiErr.Code+": "+apiErr.Message, nil), tokenUsage{})
		return
	}
	if round != nil {
		round.SetIgnoredFeatures(ignored)
	}
	request, err := chatGPTTextRequest(model, body)
	if err != nil {
		apiErr := APIError{Code: ErrorCodeInvalidRequest, Message: err.Error(), Model: model}
		// Parameter errors are local invalid_request, not proxy_internal_error.
		fail := newStreamFailWithCode(streamKindError, ErrorCodeInvalidRequest, apiErr.Code+": "+apiErr.Message, err, false)
		h.writeChatGPTWebAPIError(w, round, r, started, provider, model, stream, http.StatusBadRequest, apiErr, fail, tokenUsage{})
		return
	}

	if !stream {
		result, execErr := executor.Complete(r.Context(), request)
		billingModel := firstNonEmpty(result.ActualModel, settleModel)
		tok := estimateChatGPTTextUsage(request, result.Text)
		if execErr != nil {
			fail := streamFailFromChatGPTErr(execErr)
			status := statusForChatGPTFailure(fail)
			h.writeChatGPTWebAPIError(w, round, r, started, provider, billingModel, false, status, APIError{
				Code:    ErrorCodeUpstreamUnavailable,
				Message: "chatgpt web completion failed: " + chatGPTFailureCode(fail),
				Model:   billingModel,
			}, fail, tok)
			return
		}
		payload := openAIChatCompletion{
			ID:      "chatcmpl-" + firstNonEmpty(result.ConversationID, "chatgptweb"),
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   billingModel,
			Choices: []openAIChatChoice{{Index: 0, Message: openAIChatMessage{Role: "assistant", Content: result.Text}, FinishReason: "stop"}},
		}
		tok = estimateChatGPTTextUsage(request, result.Text)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			h.settleChatGPTWeb(round, r, provider, billingModel, false, http.StatusOK, time.Since(started), tok, newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", err, false))
			return
		}
		bodyBytes, _ := json.Marshal(payload)
		bodyBytes = append(bodyBytes, '\n')
		_ = h.writeArchiveResponse(round, "response.json", bodyBytes)
		h.settleChatGPTWeb(round, r, provider, billingModel, false, http.StatusOK, time.Since(started), tok, nil)
		return
	}

	// Streaming path.
	streamStarted := false
	var builder strings.Builder
	actualModel := settleModel
	startSSE := func() http.Flusher {
		if !streamStarted {
			prepareSSEHeaders(w.Header())
			w.WriteHeader(http.StatusOK)
			streamStarted = true
		}
		flusher, _ := w.(http.Flusher)
		return flusher
	}
	result, execErr := executor.Stream(r.Context(), request, func(delta chatgpttext.Delta) error {
		if delta.ActualModel != "" {
			actualModel = delta.ActualModel
		}
		if delta.Text == "" {
			return nil
		}
		builder.WriteString(delta.Text)
		flusher := startSSE()
		payload, err := json.Marshal(openAIChatChunk{
			ID:      "chatcmpl-chatgptweb",
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   firstNonEmpty(actualModel, settleModel),
			Choices: []openAIChatChunkChoice{{Index: 0, Delta: openAIChatMessage{Content: delta.Text}}},
		})
		if err != nil {
			return chatgptfail.New(chatgptfail.KindInternal, err)
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return chatgptfail.New(chatgptfail.KindClientWrite, err)
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if result.ActualModel != "" {
		actualModel = result.ActualModel
	}
	if result.Text != "" {
		builder.Reset()
		builder.WriteString(result.Text)
	}
	billingModel := firstNonEmpty(actualModel, settleModel)
	tok := estimateChatGPTTextUsage(request, builder.String())
	duration := time.Since(started)

	if execErr != nil {
		fail := streamFailFromChatGPTErr(execErr)
		if !streamStarted {
			status := statusForChatGPTFailure(fail)
			h.writeChatGPTWebAPIError(w, round, r, started, provider, billingModel, true, status, APIError{
				Code:    ErrorCodeUpstreamUnavailable,
				Message: "chatgpt web stream failed: " + chatGPTFailureCode(fail),
				Model:   billingModel,
			}, fail, tok)
			return
		}
		// Headers already sent: keep HTTP 200, settle real outcome.
		h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, duration, tok, fail)
		return
	}

	flusher := startSSE()
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, duration, tok, newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", err, false))
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
	h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, duration, tok, nil)
}

func (h *Handler) writeChatGPTWebAPIError(w http.ResponseWriter, round *archive.Round, r *http.Request, start time.Time, provider, model string, stream bool, status int, apiErr APIError, fail *streamFail, tok tokenUsage) {
	if apiErr.ClientProtocol == "" {
		apiErr.ClientProtocol = clientProtocolFromRequest(r)
	}
	if apiErr.ClientEndpoint == "" && r != nil && r.URL != nil {
		apiErr.ClientEndpoint = NormalizeClientEndpoint(r.URL.Path)
	}
	if apiErr.Operation == "" && apiErr.ClientEndpoint != "" {
		apiErr.Operation = OperationForPath(apiErr.ClientEndpoint)
	}
	if apiErr.Model == "" {
		apiErr.Model = model
	}
	writeClientProtocolError(w, status, apiErr.ClientProtocol, apiErr)
	if apiErr.Type == "" {
		apiErr.Type = openAIErrorType(apiErr.Code)
	}
	body, _ := json.Marshal(APIErrorResponse{Error: apiErr})
	body = append(body, '\n')
	_ = h.writeArchiveResponse(round, "response.json", body)
	if fail == nil {
		fail = newStreamFailWithCode(streamKindError, apiErr.Code, apiErr.Code+": "+apiErr.Message, nil, false)
	}
	h.settleChatGPTWeb(round, r, provider, model, stream, status, time.Since(start), tok, fail)
}

func (h *Handler) settleChatGPTWeb(round *archive.Round, r *http.Request, provider, model string, stream bool, status int, duration time.Duration, tok tokenUsage, fail *streamFail) {
	if provider == "" {
		provider = effectivecatalog.BuiltinProviderID
	}
	if round != nil && round.UpstreamProtocol == "" {
		path := pathOrEmpty(r)
		round.SetTransportPlan(
			OperationForPath(NormalizeClientEndpoint(path)),
			NormalizeClientEndpoint(path),
			ClientProtocolForPath(path),
			effectivecatalog.BuiltinProviderID,
			"chatgptweb",
			TransportModeNative,
		)
	}
	outcome := outcomeFromStreamFail(fail, status)
	h.recordAndPrintFail(round, r, provider, model, stream, status, duration, tok, fail)
	msg := ""
	if fail != nil {
		msg = fail.Error()
	}
	h.writeArchiveMetadata(round, provider, model, stream, status, duration, tok, "response.json", msg, "", outcome)
}

func estimateChatGPTTextUsage(request chatgpttext.Request, completion string) tokenUsage {
	parts := make([]string, 0, len(request.Messages))
	for _, message := range request.Messages {
		parts = append(parts, message.Content)
	}
	estimated := tokenusage.EstimateChatTextUsage(parts, completion)
	if estimated == nil {
		return tokenUsage{Estimated: true, Known: true}
	}
	return tokenUsage{
		PromptTokens:     estimated.PromptTokens,
		CompletionTokens: estimated.CompletionTokens,
		TotalTokens:      estimated.TotalTokens,
		Estimated:        true,
		Known:            true,
	}
}

func streamFailFromChatGPTErr(err error) *streamFail {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return streamFailFromKind(chatgptfail.KindClientCanceled, "client canceled", err)
	}
	if f, ok := chatgpttext.AsFailure(err); ok && f != nil {
		return streamFailFromKind(f.Kind, f.Error(), f)
	}
	return streamFailFromKind(chatgptfail.KindUpstream, err.Error(), err)
}

func streamFailFromKind(kind chatgptfail.Kind, message string, err error) *streamFail {
	var outcomeKind streamKind
	switch chatgptfail.Outcome(kind) {
	case "success":
		return nil
	case "client_canceled":
		outcomeKind = streamKindClientCanceled
	case "client_write":
		outcomeKind = streamKindClientWrite
	case "upstream_failed":
		outcomeKind = streamKindUpstreamFailed
	case "error":
		outcomeKind = streamKindError
	default:
		outcomeKind = streamKindError
	}
	return newStreamFailWithCode(outcomeKind, chatgptfail.ErrorCode(kind), message, err, chatgptfail.CountUpstream(kind))
}

func statusForChatGPTFailure(fail *streamFail) int {
	if fail == nil {
		return http.StatusOK
	}
	switch fail.ErrorCode {
	case chatgptfail.ErrorCode(chatgptfail.KindProviderUnavailable):
		return http.StatusServiceUnavailable
	case chatgptfail.ErrorCode(chatgptfail.KindClientCanceled):
		return 499
	case ErrorCodeInvalidRequest:
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

func chatGPTFailureCode(fail *streamFail) string {
	if fail == nil {
		return "unclassified"
	}
	if fail.ErrorCode != "" {
		return fail.ErrorCode
	}
	return string(fail.Kind)
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
	imageCount, imageBytes := 0, 0
	for index, item := range raw {
		message, ok := item.(map[string]any)
		if !ok {
			return chatgpttext.Request{}, fmt.Errorf("messages[%d] is invalid", index)
		}
		role, _ := message["role"].(string)
		role = strings.ToLower(strings.TrimSpace(role))
		if role != "system" && role != "user" && role != "assistant" {
			return chatgpttext.Request{}, fmt.Errorf("messages[%d].role is unsupported", index)
		}
		content, images, err := chatGPTTextContent(index, message["content"])
		if err != nil {
			return chatgpttext.Request{}, err
		}
		if len(images) > 0 && role != "user" {
			return chatgpttext.Request{}, fmt.Errorf("messages[%d] image_url content is only supported for user messages", index)
		}
		for _, image := range images {
			imageCount++
			imageBytes += len(image)
			if imageCount > imageinput.MaxChatImageCount {
				return chatgpttext.Request{}, fmt.Errorf("at most %d images are supported per request", imageinput.MaxChatImageCount)
			}
			if imageBytes > imageinput.MaxChatImageBytes {
				return chatgpttext.Request{}, fmt.Errorf("images exceed %d MiB per request", imageinput.MaxChatImageBytes>>20)
			}
		}
		if strings.TrimSpace(content) == "" && len(images) == 0 {
			return chatgpttext.Request{}, fmt.Errorf("messages[%d] requires content", index)
		}
		request.Messages = append(request.Messages, chatgpttext.Message{Role: role, Content: content, Images: images})
	}
	return request, nil
}

// chatGPTTextContent accepts the bounded OpenAI Chat Completions content
// subset implemented by ChatGPT Web: legacy text strings and text/image_url
// content parts. Remote image URLs are intentionally rejected rather than
// fetched by the proxy, so this adapter has no SSRF behavior.
func chatGPTTextContent(messageIndex int, raw any) (string, [][]byte, error) {
	if content, ok := raw.(string); ok {
		return content, nil, nil
	}
	parts, ok := raw.([]any)
	if !ok {
		return "", nil, fmt.Errorf("messages[%d].content must be a string or content-part array", messageIndex)
	}
	var text strings.Builder
	images := make([][]byte, 0)
	for partIndex, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("messages[%d].content[%d] is invalid", messageIndex, partIndex)
		}
		typ, _ := part["type"].(string)
		switch strings.TrimSpace(typ) {
		case "text":
			value, ok := part["text"].(string)
			if !ok {
				return "", nil, fmt.Errorf("messages[%d].content[%d].text is required", messageIndex, partIndex)
			}
			text.WriteString(value)
		case "image_url":
			imageURL, ok := part["image_url"].(map[string]any)
			if !ok {
				return "", nil, fmt.Errorf("messages[%d].content[%d].image_url is required", messageIndex, partIndex)
			}
			url, ok := imageURL["url"].(string)
			if !ok {
				return "", nil, fmt.Errorf("messages[%d].content[%d].image_url.url is required", messageIndex, partIndex)
			}
			image, err := imageinput.DecodeDataURLImage(url)
			if err != nil {
				return "", nil, fmt.Errorf("messages[%d].content[%d]: %w", messageIndex, partIndex, err)
			}
			images = append(images, image.Bytes)
		default:
			return "", nil, fmt.Errorf("messages[%d].content[%d].type %q is not supported", messageIndex, partIndex, strings.TrimSpace(typ))
		}
	}
	return text.String(), images, nil
}

func pathOrEmpty(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return r.URL.Path
}

type openAIChatMessage struct {
	Role        string              `json:"role,omitempty"`
	Content     string              `json:"content"`
	Annotations []openAIURLCitation `json:"annotations,omitempty"`
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
	Index        int               `json:"index"`
	Delta        openAIChatMessage `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}
type openAIChatChunk struct {
	ID      string                  `json:"id"`
	Object  string                  `json:"object"`
	Created int64                   `json:"created"`
	Model   string                  `json:"model"`
	Choices []openAIChatChunkChoice `json:"choices"`
}
