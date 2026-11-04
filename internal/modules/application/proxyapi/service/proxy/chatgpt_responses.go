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
	completion "aetherrelay/internal/pkg/aetherrelaycompletion"
	"aetherrelay/internal/pkg/chatattachment"
	"aetherrelay/internal/pkg/chatgptimageinput"
)

// handleChatGPTWebResponses implements the deliberately small, stateless
// subset of /v1/responses which can be projected onto a ChatGPT Web text
// conversation. It never stores a Responses conversation ID and never claims
// support for function tools, structured output, background/realtime execution, or
// remote image references.
func (h *Handler) handleChatGPTWebResponses(w http.ResponseWriter, r *http.Request, started time.Time, provider, model string, stream bool, body map[string]any) {
	round := archiveRoundFromContext(r.Context())
	request, ignored, apiErr := chatGPTResponsesRequest(model, body)
	if apiErr == nil && request.ThinkingEffort != "" && !h.chatGPTWebReasoningSupported(model) {
		apiErr = unsupportedChatGPTWebFeature("reasoning")
	}
	if apiErr != nil {
		apiErr.Model = model
		fail := newStreamFailWithCode(streamKindError, apiErr.Code, apiErr.Code+": "+apiErr.Message, fmt.Errorf("%s", apiErr.Message), false)
		h.writeChatGPTWebAPIError(w, round, r, started, provider, model, stream, http.StatusBadRequest, *apiErr, fail, tokenUsage{})
		return
	}
	if round != nil {
		round.SetIgnoredFeatures(ignored)
	}
	searchInvocation, _ := chatGPTWebResponsesSearchInvocation(body)
	if searchInvocation.Enabled {
		h.handleChatGPTWebSearchResponses(w, r, started, provider, model, stream, searchInvocation, body)
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
	response := newChatGPTResponse(body)
	if !stream {
		result, execErr := executor.Complete(r.Context(), request)
		execErr = chatgpttext.ValidateCompletion(&result, execErr)
		recordTextCompletion(r.Context(), result.Completion, execErr)
		recordGeneration(r.Context(), result.Generation)
		billingModel := firstNonEmpty(result.ActualModel, model)
		tok := estimateChatGPTTextUsage(request, result.Text)
		if execErr != nil {
			fail := streamFailFromChatGPTErr(execErr)
			h.writeChatGPTWebAPIError(w, round, r, started, provider, billingModel, false, statusForChatGPTFailure(fail), APIError{
				Code: ErrorCodeUpstreamUnavailable, Message: "chatgpt web response failed: " + chatGPTFailureCode(fail), Model: billingModel,
			}, fail, tok)
			return
		}
		payload := response.payload(billingModel, []any{chatGPTResponseMessage(response.messageID(), result.Text, nil, "completed")}, tok, "completed")
		setChatGPTUsageHeaders(w.Header())
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

	itemID := response.messageID()
	actualModel := model
	var text strings.Builder
	events := chatGPTResponseStream{writer: w}
	textAdded := false
	startStream := func() error {
		if err := events.start(response, actualModel); err != nil {
			return err
		}
		if !textAdded {
			textAdded = true
			return events.addText(itemID, 0)
		}
		return nil
	}
	emit := func(delta chatgpttext.Delta) error {
		if delta.ActualModel != "" {
			actualModel = delta.ActualModel
		}
		if delta.Text == "" {
			return nil
		}
		if text.Len()+len(delta.Text) > completion.MaxTextBytes {
			return chatgptfail.New(chatgptfail.KindTextBudget, fmt.Errorf("text output limit exceeded"))
		}
		if err := startStream(); err != nil {
			return chatgptfail.New(chatgptfail.KindClientWrite, err)
		}
		text.WriteString(delta.Text)
		if err := events.delta(itemID, 0, delta.Text); err != nil {
			return chatgptfail.New(chatgptfail.KindClientWrite, err)
		}
		return nil
	}
	result, execErr := executor.Stream(r.Context(), request, emit)
	execErr = chatgpttext.ValidateCompletion(&result, execErr)
	if execErr == nil && text.String() != result.Text {
		result.Completion.Completed = false
		result.Completion.Reason = completion.Snapshot
		execErr = chatgptfail.New(chatgptfail.KindTextProtocol, fmt.Errorf("text stream differs from final result"))
	}
	result.Completion.ResponseStarted = events.started
	recordTextCompletion(r.Context(), result.Completion, execErr)
	recordGeneration(r.Context(), result.Generation)
	if result.ActualModel != "" {
		actualModel = result.ActualModel
	}

	billingModel := firstNonEmpty(actualModel, model)
	tok := estimateChatGPTTextUsage(request, text.String())
	if execErr != nil {
		fail := streamFailFromChatGPTErr(execErr)
		if !events.started {
			h.writeChatGPTWebAPIError(w, round, r, started, provider, billingModel, true, statusForChatGPTFailure(fail), APIError{
				Code: ErrorCodeUpstreamUnavailable, Message: "chatgpt web response stream failed: " + chatGPTFailureCode(fail), Model: billingModel,
			}, fail, tok)
			return
		}
		_ = events.write("response.failed", map[string]any{"response": response.failed(billingModel, []any{chatGPTResponseMessage(itemID, text.String(), nil, "incomplete")}, fail)})
		_ = h.writeArchiveResponse(round, "response.sse", []byte(events.archive.String()))
		h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, fail)
		return
	}
	if err := startStream(); err != nil {
		fail := newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", err, false)
		h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, fail)
		return
	}
	finalText := text.String()
	err := events.finishText(itemID, 0, finalText, nil, "completed")
	if err == nil {
		payload := response.payload(billingModel, []any{chatGPTResponseMessage(itemID, finalText, nil, "completed")}, tok, "completed")
		err = events.write("response.completed", map[string]any{"response": payload})
	}

	if err != nil {
		fail := newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", err, false)
		_ = h.writeArchiveResponse(round, "response.sse", []byte(events.archive.String()))
		h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, fail)
		return
	}
	_ = h.writeArchiveResponse(round, "response.sse", []byte(events.archive.String()))
	h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, nil)
}

func (h *Handler) chatGPTWebReasoningSupported(model string) bool {
	snapshot := h.EffectiveCatalog()
	metadata, ok := snapshot.ModelMetadata[strings.TrimSpace(model)]
	return ok && metadata.ReasoningDeclared && metadata.ReasoningSupported
}

func chatGPTResponsesRequest(model string, body map[string]any) (chatgpttext.Request, []string, *APIError) {
	ignored, apiErr := validateChatGPTResponsesFields(body)
	if apiErr != nil {
		return chatgpttext.Request{}, nil, apiErr
	}
	search, apiErr := chatGPTWebResponsesSearchInvocation(body)
	if apiErr != nil {
		return chatgpttext.Request{}, nil, apiErr
	}
	request := chatgpttext.Request{Model: model}
	if instructions := body["instructions"]; instructions != nil {
		value, ok := instructions.(string)
		if !ok {
			return chatgpttext.Request{}, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: "instructions must be a string", Feature: "instructions"}
		}
		if strings.TrimSpace(value) != "" {
			if len(value) > completion.MaxTextBytes {
				return chatgpttext.Request{}, nil, invalidChatGPTResponseField("instructions", "instructions exceed request limits")
			}
			request.Messages = append(request.Messages, chatgpttext.Message{Role: "system", Content: value})
		}
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			request.ThinkingEffort = strings.ToLower(strings.TrimSpace(effort))
		}
	}
	input, present := body["input"]
	if !present {
		return chatgpttext.Request{}, nil, &APIError{Code: ErrorCodeInvalidRequest, Message: "input is required", Feature: "input"}
	}
	images, imageBytes, fileCount, fileBytes := 0, 0, 0, 0
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
		for _, file := range files {
			fileCount++
			fileBytes += len(file.Bytes)
		}
		if fileCount > chatattachment.MaxFileCount || fileBytes > chatattachment.MaxFileBytes {
			return invalidChatGPTResponseField("input.content", "file attachments exceed request limits")
		}
		if len(request.Messages) >= 64 || len(content) > completion.MaxTextBytes {
			return invalidChatGPTResponseField("input", "message count or content exceeds request limits")
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
			if rawType, present := item["type"]; present {
				if typ, ok := rawType.(string); !ok || typ != "message" {
					return chatgpttext.Request{}, nil, unsupportedChatGPTWebFeature(fmt.Sprintf("input[%d].type", index))
				}
			}
			if err := rejectChatGPTFields(item, fmt.Sprintf("input[%d]", index), "type", "role", "content", "id", "status"); err != nil {
				return chatgpttext.Request{}, nil, err
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
	if search.Enabled {
		if instructions, _ := body["instructions"].(string); strings.TrimSpace(instructions) != "" {
			return chatgpttext.Request{}, nil, unsupportedChatGPTWebFeature("instructions")
		}
		if request.ThinkingEffort != "" {
			return chatgpttext.Request{}, nil, unsupportedChatGPTWebFeature("reasoning.effort")
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
		prefix := fmt.Sprintf("input[%d].content[%d]", messageIndex, partIndex)
		var allowed []string
		switch typ {
		case "input_text":
			allowed = []string{"type", "text"}
		case "output_text":
			allowed = []string{"type", "text", "annotations", "logprobs"}
			for _, field := range []string{"annotations", "logprobs"} {
				if val := part[field]; val != nil {
					items, ok := val.([]any)
					if !ok || len(items) != 0 {
						return "", nil, nil, unsupportedChatGPTWebFeature(prefix + "." + field)
					}
				}
			}
		case "input_image":
			allowed = []string{"type", "image_url", "detail"}
			if detail := part["detail"]; detail != nil && detail != "auto" {
				return "", nil, nil, unsupportedChatGPTWebFeature(prefix + ".detail")
			}
		case "input_file":
			allowed = []string{"type", "filename", "file_data"}
		}
		if len(allowed) > 0 {
			if err := rejectChatGPTFields(part, prefix, allowed...); err != nil {
				return "", nil, nil, err
			}
		}
		switch typ {
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
			if comma < 0 || !strings.HasPrefix(value, "data:") || !strings.HasSuffix(value[:comma], ";base64") {
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
