package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptfail"
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptsearch"
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgpttext"
	archive "ai-proxy/internal/pkg/aiproxyarchive"
)

var chatGPTWebSearchToolTypes = map[string]struct{}{
	"web_search": {}, "web_search_preview": {}, "web_search_preview_2025_03_11": {},
}

type chatGPTWebSearchInvocation struct {
	Enabled bool
	Query   string
	Ignored []string
}

type openAIURLCitation struct {
	Type        string `json:"type"`
	URLCitation struct {
		StartIndex int    `json:"start_index"`
		EndIndex   int    `json:"end_index"`
		URL        string `json:"url"`
		Title      string `json:"title"`
	} `json:"url_citation"`
}

// chatGPTWebChatSearchInvocation recognises the one tool semantic the Web
// search upstream can faithfully perform. Mixed tools are rejected instead of
// accepting a request and silently dropping function/plugin behaviour.
func chatGPTWebChatSearchInvocation(body map[string]any) (chatGPTWebSearchInvocation, *APIError) {
	invocation, apiErr := chatGPTWebSearchToolInvocation(body, true)
	if apiErr != nil || !invocation.Enabled {
		return invocation, apiErr
	}
	query, apiErr := lastChatGPTWebUserText(body["messages"], "messages")
	if apiErr != nil {
		return chatGPTWebSearchInvocation{}, apiErr
	}
	invocation.Query = query
	return invocation, nil
}

func chatGPTWebResponsesSearchInvocation(body map[string]any) (chatGPTWebSearchInvocation, *APIError) {
	invocation, apiErr := chatGPTWebSearchToolInvocation(body, false)
	if apiErr != nil || !invocation.Enabled {
		return invocation, apiErr
	}
	query, apiErr := lastChatGPTWebResponseUserText(body["input"])
	if apiErr != nil {
		return chatGPTWebSearchInvocation{}, apiErr
	}
	invocation.Query = query
	return invocation, nil
}

func chatGPTWebSearchToolInvocation(body map[string]any, allowOptions bool) (chatGPTWebSearchInvocation, *APIError) {
	if body == nil {
		return chatGPTWebSearchInvocation{}, &APIError{Code: ErrorCodeInvalidRequest, Message: "request body is required"}
	}
	tools, hasTools := body["tools"]
	_, hasOptions := body["web_search_options"]
	if !hasTools && !hasOptions {
		return chatGPTWebSearchInvocation{}, nil
	}
	if hasOptions && !allowOptions {
		return chatGPTWebSearchInvocation{}, unsupportedChatGPTWebFeature("web_search_options")
	}
	if hasTools {
		items, ok := tools.([]any)
		if !ok || len(items) != 1 {
			return chatGPTWebSearchInvocation{}, unsupportedChatGPTWebFeature("tools")
		}
		tool, ok := items[0].(map[string]any)
		if !ok || !isChatGPTWebSearchTool(tool) {
			return chatGPTWebSearchInvocation{}, unsupportedChatGPTWebFeature("tools")
		}
		if choice, present := body["tool_choice"]; present && !isChatGPTWebSearchToolChoice(choice) {
			return chatGPTWebSearchInvocation{}, unsupportedChatGPTWebFeature("tool_choice")
		}
	} else if choice, present := body["tool_choice"]; present && !isChatGPTWebSearchToolChoice(choice) {
		return chatGPTWebSearchInvocation{}, unsupportedChatGPTWebFeature("tool_choice")
	}
	if _, present := body["parallel_tool_calls"]; present {
		return chatGPTWebSearchInvocation{}, unsupportedChatGPTWebFeature("parallel_tool_calls")
	}
	invocation := chatGPTWebSearchInvocation{Enabled: true}
	if hasOptions {
		// The options opt a Chat request into search, but upstream does not offer
		// a stable mapping for its tuning knobs. Keep that limitation observable
		// in archive metadata rather than claiming the fields were honoured.
		invocation.Ignored = []string{"web_search_options.parameters"}
	}
	return invocation, nil
}

func isChatGPTWebSearchTool(tool map[string]any) bool {
	typ, _ := tool["type"].(string)
	_, ok := chatGPTWebSearchToolTypes[strings.TrimSpace(typ)]
	return ok
}

func isChatGPTWebSearchToolChoice(value any) bool {
	tool, ok := value.(map[string]any)
	return ok && isChatGPTWebSearchTool(tool)
}

func lastChatGPTWebUserText(raw any, feature string) (string, *APIError) {
	messages, ok := raw.([]any)
	if !ok || len(messages) == 0 {
		return "", &APIError{Code: ErrorCodeInvalidRequest, Message: "messages is required", Feature: feature}
	}
	for index := len(messages) - 1; index >= 0; index-- {
		message, ok := messages[index].(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if strings.ToLower(strings.TrimSpace(role)) != "user" {
			continue
		}
		return chatGPTWebSearchTextContent(message["content"], fmt.Sprintf("messages[%d].content", index), "text")
	}
	return "", &APIError{Code: ErrorCodeInvalidRequest, Message: "a non-empty user text message is required for web search", Feature: feature}
}

func lastChatGPTWebResponseUserText(raw any) (string, *APIError) {
	if text, ok := raw.(string); ok {
		if strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text), nil
		}
		return "", &APIError{Code: ErrorCodeInvalidRequest, Message: "input text is required for web search", Feature: "input"}
	}
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return "", &APIError{Code: ErrorCodeInvalidRequest, Message: "input is required", Feature: "input"}
	}
	for index := len(items) - 1; index >= 0; index-- {
		item, ok := items[index].(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := item["type"].(string); typ != "" && strings.TrimSpace(typ) != "message" {
			continue
		}
		role, _ := item["role"].(string)
		if role != "" && strings.ToLower(strings.TrimSpace(role)) != "user" {
			continue
		}
		return chatGPTWebSearchTextContent(item["content"], fmt.Sprintf("input[%d].content", index), "input_text")
	}
	return "", &APIError{Code: ErrorCodeInvalidRequest, Message: "a non-empty user text message is required for web search", Feature: "input"}
}

func chatGPTWebSearchTextContent(raw any, feature, textType string) (string, *APIError) {
	if text, ok := raw.(string); ok {
		if strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text), nil
		}
		return "", &APIError{Code: ErrorCodeInvalidRequest, Message: "web search text is required", Feature: feature}
	}
	parts, ok := raw.([]any)
	if !ok || len(parts) == 0 {
		return "", &APIError{Code: ErrorCodeConversionUnsupported, Message: "chatgpt web search only supports pure text user content", Feature: feature}
	}
	var text strings.Builder
	for index, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return "", &APIError{Code: ErrorCodeInvalidRequest, Message: "web search content part is invalid", Feature: fmt.Sprintf("%s[%d]", feature, index)}
		}
		typ, _ := part["type"].(string)
		if strings.TrimSpace(typ) != textType {
			return "", &APIError{Code: ErrorCodeConversionUnsupported, Message: "chatgpt web search only supports pure text user content", Feature: fmt.Sprintf("%s[%d].type", feature, index)}
		}
		value, ok := part["text"].(string)
		if !ok {
			return "", &APIError{Code: ErrorCodeInvalidRequest, Message: "web search text is required", Feature: fmt.Sprintf("%s[%d].text", feature, index)}
		}
		text.WriteString(value)
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", &APIError{Code: ErrorCodeInvalidRequest, Message: "web search text is required", Feature: feature}
	}
	return strings.TrimSpace(text.String()), nil
}

func (h *Handler) handleChatGPTWebSearchChat(w http.ResponseWriter, r *http.Request, started time.Time, provider, model string, stream bool, invocation chatGPTWebSearchInvocation) {
	round := archiveRoundFromContext(r.Context())
	h.cfgMu.RLock()
	executor := h.chatGPTSearch
	h.cfgMu.RUnlock()
	if executor == nil {
		apiErr := APIError{Code: ErrorCodeProviderUnavailable, Message: "chatgpt web search executor is unavailable", Model: model}
		h.writeChatGPTWebAPIError(w, round, r, started, provider, model, stream, http.StatusServiceUnavailable, apiErr, streamFailFromKind(chatgptfail.KindProviderUnavailable, apiErr.Code+": "+apiErr.Message, nil), tokenUsage{})
		return
	}
	if round != nil {
		round.SetIgnoredFeatures(invocation.Ignored)
	}
	request := chatgptsearch.Request{Model: model, Query: invocation.Query}
	result, err := executor.Search(r.Context(), request)
	billingModel := firstNonEmpty(result.ActualModel, model)
	usageRequest := chatgpttext.Request{Model: model, Messages: []chatgpttext.Message{{Role: "user", Content: invocation.Query}}}
	tok := estimateChatGPTTextUsage(usageRequest, result.Text)
	if err != nil {
		fail := streamFailFromChatGPTSearchErr(err)
		h.writeChatGPTWebAPIError(w, round, r, started, provider, billingModel, stream, statusForChatGPTFailure(fail), APIError{Code: ErrorCodeUpstreamUnavailable, Message: "chatgpt web search failed: " + chatGPTFailureCode(fail), Model: billingModel}, fail, tok)
		return
	}
	text, annotations := presentChatGPTWebSearch(result)
	tok = estimateChatGPTTextUsage(usageRequest, text)
	if !stream {
		payload := openAIChatCompletion{ID: "chatcmpl-" + firstNonEmpty(result.ConversationID, "chatgptweb-search"), Object: "chat.completion", Created: time.Now().Unix(), Model: billingModel, Choices: []openAIChatChoice{{Index: 0, Message: openAIChatMessage{Role: "assistant", Content: text, Annotations: annotations}, FinishReason: "stop"}}}
		w.Header().Set("Content-Type", "application/json")
		if encErr := json.NewEncoder(w).Encode(payload); encErr != nil {
			h.settleChatGPTWeb(round, r, provider, billingModel, false, http.StatusOK, time.Since(started), tok, newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", encErr, false))
			return
		}
		if archived, marshalErr := json.Marshal(payload); marshalErr == nil {
			_ = h.writeArchiveResponse(round, "response.json", append(archived, '\n'))
		}
		h.settleChatGPTWeb(round, r, provider, billingModel, false, http.StatusOK, time.Since(started), tok, nil)
		return
	}
	prepareSSEHeaders(w.Header())
	w.WriteHeader(http.StatusOK)
	payload, marshalErr := json.Marshal(openAIChatChunk{ID: "chatcmpl-" + firstNonEmpty(result.ConversationID, "chatgptweb-search"), Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: billingModel, Choices: []openAIChatChunkChoice{{Index: 0, Delta: openAIChatMessage{Role: "assistant", Content: text, Annotations: annotations}, FinishReason: nil}}})
	if marshalErr == nil {
		_, marshalErr = fmt.Fprintf(w, "data: %s\n\n", payload)
	}
	if marshalErr == nil {
		_, marshalErr = fmt.Fprint(w, "data: [DONE]\n\n")
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if marshalErr != nil {
		h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", marshalErr, false))
		return
	}
	// This is intentionally buffered: ChatGPT Web only exposes reliable search
	// sources after its document poll completes, so the SSE is compatible but
	// not token-incremental upstream streaming.
	h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, nil)
}

func (h *Handler) handleChatGPTWebSearchResponses(w http.ResponseWriter, r *http.Request, started time.Time, provider, model string, stream bool, invocation chatGPTWebSearchInvocation) {
	round := archiveRoundFromContext(r.Context())
	h.cfgMu.RLock()
	executor := h.chatGPTSearch
	h.cfgMu.RUnlock()
	if executor == nil {
		apiErr := APIError{Code: ErrorCodeProviderUnavailable, Message: "chatgpt web search executor is unavailable", Model: model}
		h.writeChatGPTWebAPIError(w, round, r, started, provider, model, stream, http.StatusServiceUnavailable, apiErr, streamFailFromKind(chatgptfail.KindProviderUnavailable, apiErr.Code+": "+apiErr.Message, nil), tokenUsage{})
		return
	}
	if round != nil {
		round.SetIgnoredFeatures(invocation.Ignored)
	}
	usageRequest := chatgpttext.Request{Model: model, Messages: []chatgpttext.Message{{Role: "user", Content: invocation.Query}}}
	if !stream {
		result, err := executor.Search(r.Context(), chatgptsearch.Request{Model: model, Query: invocation.Query})
		billingModel := firstNonEmpty(result.ActualModel, model)
		tok := estimateChatGPTTextUsage(usageRequest, result.Text)
		if err != nil {
			h.writeChatGPTWebSearchResponseError(w, round, r, started, provider, billingModel, false, err, tok)
			return
		}
		text, annotations := presentChatGPTWebSearch(result)
		tok = estimateChatGPTTextUsage(usageRequest, text)
		responseID := responseIdentifier(result.ConversationID)
		searchItem := chatGPTWebSearchCallItem("ws_"+strings.TrimPrefix(responseID, "resp_"), invocation.Query, "completed", result.Sources)
		messageItem := chatGPTSearchResponseOutputItem("msg_"+strings.TrimPrefix(responseID, "resp_"), text, annotations, "completed")
		payload := chatGPTSearchResponsePayload(responseID, billingModel, []any{searchItem, messageItem}, tok, "completed")
		w.Header().Set("Content-Type", "application/json")
		if encErr := json.NewEncoder(w).Encode(payload); encErr != nil {
			h.settleChatGPTWeb(round, r, provider, billingModel, false, http.StatusOK, time.Since(started), tok, newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", encErr, false))
			return
		}
		if archived, marshalErr := json.Marshal(payload); marshalErr == nil {
			_ = h.writeArchiveResponse(round, "response.json", append(archived, '\n'))
		}
		h.settleChatGPTWeb(round, r, provider, billingModel, false, http.StatusOK, time.Since(started), tok, nil)
		return
	}

	responseID := responseIdentifier("")
	searchID := "ws_" + strings.TrimPrefix(responseID, "resp_")
	messageID := "msg_" + strings.TrimPrefix(responseID, "resp_")
	actualModel := model
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
		created := chatGPTSearchResponsePayload(responseID, actualModel, []any{}, tokenUsage{}, "in_progress")
		return writeEvent("response.created", map[string]any{"type": "response.created", "response": created})
	}
	if err := startStream(); err != nil {
		fail := newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", err, false)
		h.settleChatGPTWeb(round, r, provider, model, true, http.StatusOK, time.Since(started), tokenUsage{}, fail)
		return
	}
	searchingItem := chatGPTWebSearchCallItem(searchID, invocation.Query, "in_progress", nil)
	err := writeEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": searchingItem})
	if err == nil {
		err = writeEvent("response.web_search_call.in_progress", map[string]any{"type": "response.web_search_call.in_progress", "output_index": 0, "item_id": searchID})
	}
	if err == nil {
		err = writeEvent("response.web_search_call.searching", map[string]any{"type": "response.web_search_call.searching", "output_index": 0, "item_id": searchID})
	}
	if err != nil {
		fail := newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", err, false)
		_ = h.writeArchiveResponse(round, "response.sse", []byte(archiveSSE.String()))
		h.settleChatGPTWeb(round, r, provider, model, true, http.StatusOK, time.Since(started), tokenUsage{}, fail)
		return
	}
	result, searchErr := executor.Search(r.Context(), chatgptsearch.Request{Model: model, Query: invocation.Query})
	if result.ActualModel != "" {
		actualModel = result.ActualModel
	}
	billingModel := firstNonEmpty(actualModel, model)
	tok := estimateChatGPTTextUsage(usageRequest, result.Text)
	if searchErr != nil {
		fail := streamFailFromChatGPTSearchErr(searchErr)
		_ = writeEvent("response.failed", map[string]any{"type": "response.failed", "response": chatGPTFailedResponse(responseID, billingModel, fail)})
		_ = h.writeArchiveResponse(round, "response.sse", []byte(archiveSSE.String()))
		h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, fail)
		return
	}
	text, annotations := presentChatGPTWebSearch(result)
	tok = estimateChatGPTTextUsage(usageRequest, text)
	searchItem := chatGPTWebSearchCallItem(searchID, invocation.Query, "completed", result.Sources)
	messageItem := chatGPTSearchResponseOutputItem(messageID, text, annotations, "completed")
	err = writeEvent("response.web_search_call.completed", map[string]any{"type": "response.web_search_call.completed", "output_index": 0, "item_id": searchID})
	if err == nil {
		err = writeEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": searchItem})
	}
	if err == nil {
		err = writeEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 1, "item": chatGPTSearchResponseOutputItem(messageID, "", annotations, "in_progress")})
	}
	if err == nil && text != "" {
		err = writeEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": messageID, "output_index": 1, "content_index": 0, "delta": text})
	}
	if err == nil {
		err = writeEvent("response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": messageID, "output_index": 1, "content_index": 0, "text": text})
	}
	if err == nil {
		err = writeEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 1, "item": messageItem})
	}
	if err == nil {
		completed := chatGPTSearchResponsePayload(responseID, billingModel, []any{searchItem, messageItem}, tok, "completed")
		err = writeEvent("response.completed", map[string]any{"type": "response.completed", "response": completed})
	}
	_ = h.writeArchiveResponse(round, "response.sse", []byte(archiveSSE.String()))
	if err != nil {
		fail := newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", err, false)
		h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, fail)
		return
	}
	h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, nil)
}

func streamFailFromChatGPTSearchErr(err error) *streamFail {
	if f, ok := chatgptsearch.AsFailure(err); ok && f != nil {
		return streamFailFromKind(f.Kind, f.Error(), f)
	}
	if err == context.Canceled {
		return streamFailFromKind(chatgptfail.KindClientCanceled, "client canceled", err)
	}
	return streamFailFromKind(chatgptfail.KindUpstream, err.Error(), err)
}

func presentChatGPTWebSearch(result chatgptsearch.Result) (string, []openAIURLCitation) {
	text := cleanChatGPTWebSearchText(result.Text)
	annotations := make([]openAIURLCitation, 0, len(result.Sources))
	if len(result.Sources) == 0 {
		return text, annotations
	}
	if text != "" {
		text += "\n\n"
	}
	text += "Sources:\n"
	for index, source := range result.Sources {
		title := strings.TrimSpace(source.Title)
		if title == "" {
			title = source.URL
		}
		line := fmt.Sprintf("%d. %s", index+1, title)
		if source.URL != "" {
			start := utf8.RuneCountInString(text) + utf8.RuneCountInString(line)
			if title == source.URL {
				start = utf8.RuneCountInString(text) + utf8.RuneCountInString(fmt.Sprintf("%d. ", index+1))
			} else {
				line += " - "
				start = utf8.RuneCountInString(text) + utf8.RuneCountInString(line)
				line += source.URL
			}
			citation := openAIURLCitation{Type: "url_citation"}
			citation.URLCitation.StartIndex = start
			citation.URLCitation.EndIndex = start + utf8.RuneCountInString(source.URL)
			citation.URLCitation.URL, citation.URLCitation.Title = source.URL, title
			annotations = append(annotations, citation)
		}
		text += line + "\n"
	}
	return strings.TrimSpace(text), annotations
}

func cleanChatGPTWebSearchText(text string) string {
	for {
		start := strings.IndexRune(text, '\uE200')
		if start < 0 {
			break
		}
		end := strings.IndexRune(text[start:], '\uE201')
		if end < 0 {
			text = text[:start]
			break
		}
		text = text[:start] + text[start+end+len(string('\uE201')):]
	}
	return strings.TrimSpace(text)
}

func searchResponseAnnotations(annotations []openAIURLCitation) []any {
	if len(annotations) == 0 {
		return []any{}
	}
	result := make([]any, 0, len(annotations))
	for _, item := range annotations {
		result = append(result, item)
	}
	return result
}

func chatGPTWebSearchCallItem(itemID, query, status string, sources []chatgptsearch.Source) map[string]any {
	action := map[string]any{"type": "search", "query": query, "queries": []string{query}}
	if len(sources) > 0 {
		urls := make([]any, 0, len(sources))
		for _, source := range sources {
			if source.URL != "" {
				urls = append(urls, map[string]any{"type": "url", "url": source.URL})
			}
		}
		action["sources"] = urls
	}
	return map[string]any{"id": itemID, "type": "web_search_call", "status": status, "action": action}
}

func chatGPTSearchResponseOutputItem(itemID, text string, annotations []openAIURLCitation, status string) map[string]any {
	return map[string]any{"id": itemID, "type": "message", "status": status, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": searchResponseAnnotations(annotations)}}}
}

func chatGPTSearchResponsePayload(responseID, model string, output []any, tok tokenUsage, status string) map[string]any {
	payload := map[string]any{"id": responseID, "object": "response", "created_at": time.Now().Unix(), "status": status, "model": model, "output": output}
	if status == "completed" {
		payload["completed_at"] = time.Now().Unix()
		payload["usage"] = map[string]any{"input_tokens": tok.PromptTokens, "output_tokens": tok.CompletionTokens, "total_tokens": tok.TotalTokens}
	}
	return payload
}

func (h *Handler) writeChatGPTWebSearchResponseError(w http.ResponseWriter, round *archive.Round, r *http.Request, started time.Time, provider, model string, stream bool, err error, tok tokenUsage) {
	fail := streamFailFromChatGPTSearchErr(err)
	h.writeChatGPTWebAPIError(w, round, r, started, provider, model, stream, statusForChatGPTFailure(fail), APIError{Code: ErrorCodeUpstreamUnavailable, Message: "chatgpt web search failed: " + chatGPTFailureCode(fail), Model: model}, fail, tok)
}
