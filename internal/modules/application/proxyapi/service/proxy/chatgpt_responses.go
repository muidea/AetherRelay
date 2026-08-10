package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
	"aetherrelay/internal/pkg/chatattachment"
	"aetherrelay/internal/pkg/chatgptimageinput"
)

// handleChatGPTWebResponses implements the deliberately small, stateless
// subset of /v1/responses which can be projected onto a ChatGPT Web text
// conversation. It never stores a Responses conversation ID and never claims
// support for tools, structured output, background/realtime execution, or
// remote image references.
func (h *Handler) handleChatGPTWebResponses(w http.ResponseWriter, r *http.Request, started time.Time, provider, model string, stream bool, body map[string]any) {
	round := archiveRoundFromContext(r.Context())
	searchInvocation, searchErr := chatGPTWebResponsesSearchInvocation(body)
	if searchErr != nil {
		searchErr.Model = model
		fail := newStreamFailWithCode(streamKindError, searchErr.Code, searchErr.Code+": "+searchErr.Message, fmt.Errorf("%s", searchErr.Message), false)
		h.writeChatGPTWebAPIError(w, round, r, started, provider, model, stream, http.StatusBadRequest, *searchErr, fail, tokenUsage{})
		return
	}
	if searchInvocation.Enabled {
		h.handleChatGPTWebSearchResponses(w, r, started, provider, model, stream, searchInvocation)
		return
	}
	if _, present := body["reasoning"]; present && !h.chatGPTWebReasoningSupported(model) {
		apiErr := unsupportedChatGPTWebFeature("reasoning")
		apiErr.Model = model
		fail := newStreamFailWithCode(streamKindError, apiErr.Code, apiErr.Code+": "+apiErr.Message, fmt.Errorf("%s", apiErr.Message), false)
		h.writeChatGPTWebAPIError(w, round, r, started, provider, model, stream, http.StatusBadRequest, *apiErr, fail, tokenUsage{})
		return
	}
	h.cfgMu.RLock()
	executor := h.chatGPTText
	h.cfgMu.RUnlock()
	if executor == nil {
		apiErr := APIError{Code: ErrorCodeProviderUnavailable, Message: "chatgpt web executor is unavailable", Model: model}
		h.writeChatGPTWebAPIError(w, round, r, started, provider, model, stream, http.StatusServiceUnavailable, apiErr, streamFailFromKind(chatgptfail.KindProviderUnavailable, apiErr.Code+": "+apiErr.Message, nil), tokenUsage{})
		return
	}
	request, ignored, apiErr := chatGPTResponsesRequest(model, body)
	if apiErr != nil {
		apiErr.Model = model
		fail := newStreamFailWithCode(streamKindError, apiErr.Code, apiErr.Code+": "+apiErr.Message, fmt.Errorf("%s", apiErr.Message), false)
		h.writeChatGPTWebAPIError(w, round, r, started, provider, model, stream, http.StatusBadRequest, *apiErr, fail, tokenUsage{})
		return
	}
	if round != nil {
		round.SetIgnoredFeatures(ignored)
	}

	if !stream {
		result, execErr := executor.Complete(r.Context(), request)
		billingModel := firstNonEmpty(result.ActualModel, model)
		tok := estimateChatGPTTextUsage(request, result.Text)
		if execErr != nil {
			fail := streamFailFromChatGPTErr(execErr)
			h.writeChatGPTWebAPIError(w, round, r, started, provider, billingModel, false, statusForChatGPTFailure(fail), APIError{
				Code: ErrorCodeUpstreamUnavailable, Message: "chatgpt web response failed: " + chatGPTFailureCode(fail), Model: billingModel,
			}, fail, tok)
			return
		}
		payload := chatGPTResponsePayload(responseIdentifier(result.ConversationID), billingModel, result.Text, tok, "completed")
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			h.settleChatGPTWeb(round, r, provider, billingModel, false, http.StatusOK, time.Since(started), tok, newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", err, false))
			return
		}
		if archived, err := json.Marshal(payload); err == nil {
			_ = h.writeArchiveResponse(round, "response.json", append(archived, '\n'))
		}
		h.settleChatGPTWeb(round, r, provider, billingModel, false, http.StatusOK, time.Since(started), tok, nil)
		return
	}

	responseID := responseIdentifier("")
	itemID := "msg_" + strings.TrimPrefix(responseID, "resp_")
	actualModel := model
	var text strings.Builder
	var archiveSSE strings.Builder
	streamStarted := false
	writeEvent := func(eventType string, payload any) error {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, encoded)
		archiveSSE.WriteString(line)
		if _, err := fmt.Fprint(w, line); err != nil {
			return err
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	}
	startStream := func() error {
		if streamStarted {
			return nil
		}
		prepareSSEHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		streamStarted = true
		created := chatGPTResponsePayload(responseID, actualModel, "", tokenUsage{}, "in_progress")
		created["output"] = []any{}
		return writeEvent("response.created", map[string]any{"type": "response.created", "response": created})
	}
	emit := func(delta chatgpttext.Delta) error {
		if delta.ActualModel != "" {
			actualModel = delta.ActualModel
		}
		if delta.Text == "" {
			return nil
		}
		if err := startStream(); err != nil {
			return chatgptfail.New(chatgptfail.KindClientWrite, err)
		}
		text.WriteString(delta.Text)
		if err := writeEvent("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": itemID, "output_index": 0, "content_index": 0, "delta": delta.Text,
		}); err != nil {
			return chatgptfail.New(chatgptfail.KindClientWrite, err)
		}
		return nil
	}
	result, execErr := executor.Stream(r.Context(), request, emit)
	if result.ActualModel != "" {
		actualModel = result.ActualModel
	}
	if result.Text != "" {
		text.Reset()
		text.WriteString(result.Text)
	}
	billingModel := firstNonEmpty(actualModel, model)
	tok := estimateChatGPTTextUsage(request, text.String())
	if execErr != nil {
		fail := streamFailFromChatGPTErr(execErr)
		if !streamStarted {
			h.writeChatGPTWebAPIError(w, round, r, started, provider, billingModel, true, statusForChatGPTFailure(fail), APIError{
				Code: ErrorCodeUpstreamUnavailable, Message: "chatgpt web response stream failed: " + chatGPTFailureCode(fail), Model: billingModel,
			}, fail, tok)
			return
		}
		_ = writeEvent("response.failed", map[string]any{"type": "response.failed", "response": chatGPTFailedResponse(responseID, billingModel, fail)})
		_ = h.writeArchiveResponse(round, "response.sse", []byte(archiveSSE.String()))
		h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, fail)
		return
	}
	if err := startStream(); err != nil {
		fail := newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", err, false)
		h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, fail)
		return
	}
	finalText := text.String()
	item := chatGPTResponseOutputItem(itemID, finalText)
	err := writeEvent("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": itemID, "output_index": 0, "content_index": 0, "text": finalText,
	})
	if err == nil {
		err = writeEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	}
	if err == nil {
		completed := chatGPTResponsePayload(responseID, billingModel, finalText, tok, "completed")
		err = writeEvent("response.completed", map[string]any{"type": "response.completed", "response": completed})
	}
	if err != nil {
		fail := newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", err, false)
		_ = h.writeArchiveResponse(round, "response.sse", []byte(archiveSSE.String()))
		h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, fail)
		return
	}
	_ = h.writeArchiveResponse(round, "response.sse", []byte(archiveSSE.String()))
	h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, nil)
}

func (h *Handler) chatGPTWebReasoningSupported(model string) bool {
	snapshot := h.EffectiveCatalog()
	metadata, ok := snapshot.ModelMetadata[strings.TrimSpace(model)]
	return ok && metadata.ReasoningDeclared && metadata.ReasoningSupported
}

func chatGPTResponsesRequest(model string, body map[string]any) (chatgpttext.Request, []string, *APIError) {
	if body == nil {
		return chatgpttext.Request{}, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: "request body is required"}
	}
	search, searchErr := chatGPTWebResponsesSearchInvocation(body)
	if searchErr != nil {
		return chatgpttext.Request{}, nil, searchErr
	}
	for _, feature := range []string{
		"tool_choice", "parallel_tool_calls", "previous_response_id", "background", "conversation", "prompt", "include", "truncation",
	} {
		if _, present := body[feature]; present {
			return chatgpttext.Request{}, nil, unsupportedChatGPTWebFeature(feature)
		}
	}
	if !search.Enabled {
		if _, present := body["tools"]; present {
			return chatgpttext.Request{}, nil, unsupportedChatGPTWebFeature("tools")
		}
	}
	if text, present := body["text"].(map[string]any); present {
		if _, hasFormat := text["format"]; hasFormat {
			return chatgpttext.Request{}, nil, unsupportedChatGPTWebFeature("text.format")
		}
	}
	request := chatgpttext.Request{Model: model}
	if instructions, present := body["instructions"]; present {
		value, ok := instructions.(string)
		if !ok {
			return chatgpttext.Request{}, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: "instructions must be a string", Feature: "instructions"}
		}
		if strings.TrimSpace(value) != "" {
			request.Messages = append(request.Messages, chatgpttext.Message{Role: "system", Content: value})
		}
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			request.ThinkingEffort = effort
		}
	}
	input, present := body["input"]
	if !present {
		return chatgpttext.Request{}, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: "input is required", Feature: "input"}
	}
	images, imageBytes := 0, 0
	appendMessage := func(role, content string, attachments [][]byte, files []chatattachment.File) *APIError {
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "developer" {
			role = "system"
		}
		if role != "system" && role != "user" && role != "assistant" {
			return &APIError{Code: ErrorCodeInvalidRequest, Message: "input message role is unsupported", Feature: "input.role"}
		}
		if (len(attachments) > 0 || len(files) > 0) && role != "user" {
			return &APIError{Code: ErrorCodeInvalidRequest, Message: "input attachments are only supported for user messages", Feature: "input.content"}
		}
		for _, attachment := range attachments {
			images++
			imageBytes += len(attachment)
			if images > imageinput.MaxChatImageCount {
				return &APIError{Code: ErrorCodeInvalidRequest, Message: fmt.Sprintf("at most %d images are supported per request", imageinput.MaxChatImageCount), Feature: "input.content"}
			}
			if imageBytes > imageinput.MaxChatImageBytes {
				return &APIError{Code: ErrorCodeInvalidRequest, Message: fmt.Sprintf("images exceed %d MiB per request", imageinput.MaxChatImageBytes>>20), Feature: "input.content"}
			}
		}
		if strings.TrimSpace(content) == "" && len(attachments) == 0 && len(files) == 0 {
			return &APIError{Code: ErrorCodeInvalidRequest, Message: "input message requires content", Feature: "input.content"}
		}
		request.Messages = append(request.Messages, chatgpttext.Message{Role: role, Content: content, Images: attachments, Files: files})
		return nil
	}
	switch value := input.(type) {
	case string:
		if err := appendMessage("user", value, nil, nil); err != nil {
			return chatgpttext.Request{}, nil, err
		}
	case []any:
		for index, rawItem := range value {
			item, ok := rawItem.(map[string]any)
			if !ok {
				return chatgpttext.Request{}, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: fmt.Sprintf("input[%d] is invalid", index), Feature: "input"}
			}
			if typ, _ := item["type"].(string); strings.TrimSpace(typ) != "" && typ != "message" {
				return chatgpttext.Request{}, nil, unsupportedChatGPTWebFeature(fmt.Sprintf("input[%d].type", index))
			}
			role, _ := item["role"].(string)
			content, attachments, files, err := chatGPTResponsesContent(index, item["content"])
			if err != nil {
				return chatgpttext.Request{}, nil, err
			}
			if err := appendMessage(role, content, attachments, files); err != nil {
				return chatgpttext.Request{}, nil, err
			}
		}
	default:
		return chatgpttext.Request{}, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: "input must be a string or message array", Feature: "input"}
	}
	if len(request.Messages) == 0 {
		return chatgpttext.Request{}, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: "input is required", Feature: "input"}
	}
	known := map[string]struct{}{
		"model": {}, "input": {}, "instructions": {}, "stream": {}, "reasoning": {}, "text": {},
		"tools": {}, "tool_choice": {}, "parallel_tool_calls": {}, "previous_response_id": {}, "background": {}, "conversation": {}, "prompt": {}, "include": {}, "truncation": {},
	}
	ignored := make([]string, 0, len(body))
	for field := range body {
		if _, handled := known[field]; !handled {
			ignored = append(ignored, field)
		}
	}
	for _, field := range []string{"temperature", "top_p", "max_output_tokens", "user", "metadata", "store", "service_tier", "safety_identifier"} {
		if _, present := body[field]; present {
			ignored = append(ignored, field)
		}
	}
	return request, uniqueSortedFeatures(ignored), nil
}

func chatGPTResponsesContent(messageIndex int, raw any) (string, [][]byte, []chatattachment.File, *APIError) {
	if content, ok := raw.(string); ok {
		return content, nil, nil, nil
	}
	parts, ok := raw.([]any)
	if !ok {
		return "", nil, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: fmt.Sprintf("input[%d].content must be a string or content-part array", messageIndex), Feature: "input.content"}
	}
	var text strings.Builder
	images := make([][]byte, 0)
	files := make([]chatattachment.File, 0)
	for partIndex, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return "", nil, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: fmt.Sprintf("input[%d].content[%d] is invalid", messageIndex, partIndex), Feature: "input.content"}
		}
		typ, _ := part["type"].(string)
		switch strings.TrimSpace(typ) {
		case "input_text", "output_text":
			value, ok := part["text"].(string)
			if !ok {
				return "", nil, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: fmt.Sprintf("input[%d].content[%d].text is required", messageIndex, partIndex), Feature: "input.content"}
			}
			text.WriteString(value)
		case "input_image":
			value, ok := part["image_url"].(string)
			if !ok {
				return "", nil, nil, unsupportedChatGPTWebFeature(fmt.Sprintf("input[%d].content[%d].image_url", messageIndex, partIndex))
			}
			image, err := imageinput.DecodeDataURLImage(value)
			if err != nil {
				return "", nil, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: fmt.Sprintf("input[%d].content[%d]: %v", messageIndex, partIndex, err), Feature: "input.content"}
			}
			images = append(images, image.Bytes)
		case "input_file":
			name, _ := part["filename"].(string)
			value, _ := part["file_data"].(string)
			comma := strings.IndexByte(value, ',')
			if comma < 0 || !strings.Contains(value[:comma], ";base64") {
				return "", nil, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: fmt.Sprintf("input[%d].content[%d].file_data must be a base64 data URL", messageIndex, partIndex), Feature: "input.content"}
			}
			data, err := base64.StdEncoding.DecodeString(value[comma+1:])
			if err != nil {
				return "", nil, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: fmt.Sprintf("input[%d].content[%d].file_data is invalid", messageIndex, partIndex), Feature: "input.content"}
			}
			declared := strings.TrimPrefix(strings.Split(value[:comma], ";")[0], "data:")
			file, err := chatattachment.Validate(data, name, declared)
			if err != nil {
				return "", nil, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: fmt.Sprintf("input[%d].content[%d]: %v", messageIndex, partIndex, err), Feature: "input.content"}
			}
			files = append(files, file)
			if len(files) > chatattachment.MaxFileCount {
				return "", nil, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: fmt.Sprintf("at most %d files are supported per request", chatattachment.MaxFileCount), Feature: "input.content"}
			}
			fileBytes := 0
			for _, item := range files {
				fileBytes += len(item.Bytes)
			}
			if fileBytes > chatattachment.MaxFileBytes {
				return "", nil, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: fmt.Sprintf("files exceed %d MiB per request", chatattachment.MaxFileBytes>>20), Feature: "input.content"}
			}
		default:
			return "", nil, nil, unsupportedChatGPTWebFeature(fmt.Sprintf("input[%d].content[%d].type", messageIndex, partIndex))
		}
	}
	return text.String(), images, files, nil
}

func responseIdentifier(conversationID string) string {
	value := strings.TrimSpace(conversationID)
	if value == "" {
		value = newRequestID()
	}
	return "resp_" + value
}

func chatGPTResponseOutputItem(itemID, text string) map[string]any {
	return map[string]any{
		"id": itemID, "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
}

func chatGPTResponsePayload(responseID, model, text string, tok tokenUsage, status string) map[string]any {
	payload := map[string]any{
		"id": responseID, "object": "response", "created_at": time.Now().Unix(), "status": status, "model": model,
		"output": []any{chatGPTResponseOutputItem("msg_"+strings.TrimPrefix(responseID, "resp_"), text)},
	}
	if status == "completed" {
		payload["completed_at"] = time.Now().Unix()
		payload["usage"] = map[string]any{"input_tokens": tok.PromptTokens, "output_tokens": tok.CompletionTokens, "total_tokens": tok.TotalTokens}
	}
	return payload
}

func chatGPTFailedResponse(responseID, model string, fail *streamFail) map[string]any {
	return map[string]any{
		"id": responseID, "object": "response", "created_at": time.Now().Unix(), "status": "failed", "model": model,
		"error": map[string]any{"code": chatGPTFailureCode(fail), "message": firstNonEmpty(fail.Error(), "chatgpt web response failed")},
	}
}
