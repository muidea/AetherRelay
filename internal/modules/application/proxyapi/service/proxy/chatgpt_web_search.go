package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptsearch"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
)

var chatGPTWebSearchToolTypes = map[string]struct{}{
	"web_search": {}, "web_search_preview": {}, "web_search_preview_2025_03_11": {},
}

type chatGPTWebSearchInvocation struct {
	Enabled bool
	Query   string
}

type openAIURLCitation struct {
	Type       string `json:"type"`
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
	URL        string `json:"url"`
	Title      string `json:"title"`
}

func chatGPTWebResponsesSearchInvocation(body map[string]any) (chatGPTWebSearchInvocation, *APIError) {
	invocation, apiErr := chatGPTWebSearchToolInvocation(body)
	if apiErr != nil || !invocation.Enabled {
		return invocation, apiErr
	}
	query, apiErr := lastChatGPTWebResponseUserText(body["input"])
	if apiErr != nil {
		return chatGPTWebSearchInvocation{}, apiErr
	}
	if len(query) > chatgptsearch.MaxQueryBytes {
		return chatGPTWebSearchInvocation{}, invalidChatGPTResponseField("input", "web search query exceeds 64 KiB")
	}
	invocation.Query = query
	return invocation, nil
}

func chatGPTWebSearchToolInvocation(body map[string]any) (chatGPTWebSearchInvocation, *APIError) {
	if body == nil {
		return chatGPTWebSearchInvocation{}, invalidChatGPTResponseField("", "request body is required")
	}
	if _, present := body["web_search_options"]; present {
		return chatGPTWebSearchInvocation{}, unsupportedChatGPTWebFeature("web_search_options")
	}
	var tools []any
	if raw := body["tools"]; raw != nil {
		var ok bool
		tools, ok = raw.([]any)
		if !ok {
			return chatGPTWebSearchInvocation{}, invalidChatGPTResponseField("tools", "tools must be an array")
		}
	}
	if len(tools) == 0 {
		if choice := body["tool_choice"]; choice != nil && choice != "none" && choice != "auto" {
			return chatGPTWebSearchInvocation{}, unsupportedChatGPTWebFeature("tool_choice")
		}
		return chatGPTWebSearchInvocation{}, nil
	}
	if len(tools) != 1 {
		return chatGPTWebSearchInvocation{}, unsupportedChatGPTWebFeature("tools")
	}
	tool, ok := tools[0].(map[string]any)
	if !ok || !isChatGPTWebSearchTool(tool) {
		return chatGPTWebSearchInvocation{}, unsupportedChatGPTWebFeature("tools")
	}
	if err := rejectChatGPTFields(tool, "tools[0]", "type"); err != nil {
		return chatGPTWebSearchInvocation{}, err
	}
	if choice := body["tool_choice"]; choice != nil {
		if text, ok := choice.(string); ok {
			// A forced Web search cannot implement the model's optional auto choice.
			if text != "required" {
				return chatGPTWebSearchInvocation{}, unsupportedChatGPTWebFeature("tool_choice")
			}
		} else {
			selected, ok := choice.(map[string]any)
			if !ok || selected["type"] != tool["type"] {
				return chatGPTWebSearchInvocation{}, unsupportedChatGPTWebFeature("tool_choice")
			}
			if err := rejectChatGPTFields(selected, "tool_choice", "type"); err != nil {
				return chatGPTWebSearchInvocation{}, err
			}
		}
	}
	return chatGPTWebSearchInvocation{Enabled: true}, nil
}

func isChatGPTWebSearchTool(tool map[string]any) bool {
	typ, _ := tool["type"].(string)
	_, ok := chatGPTWebSearchToolTypes[typ]
	return ok
}

func lastChatGPTWebResponseUserText(raw any) (string, *APIError) {
	if text, ok := raw.(string); ok {
		if strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text), nil
		}
		return "", invalidChatGPTResponseField("input", "input text is required for web search")
	}
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return "", invalidChatGPTResponseField("input", "input is required")
	}
	if len(items) != 1 {
		return "", unsupportedChatGPTWebFeature("input.history")
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		return "", invalidChatGPTResponseField("input", "input message is invalid")
	}
	if err := rejectChatGPTFields(item, "input[0]", "type", "role", "content"); err != nil {
		return "", err
	}
	if rawType, present := item["type"]; present {
		if typ, ok := rawType.(string); !ok || typ != "message" {
			return "", unsupportedChatGPTWebFeature("input[0].type")
		}
	}
	if role, _ := item["role"].(string); role != "user" {
		return "", unsupportedChatGPTWebFeature("input[0].role")
	}
	return chatGPTWebSearchTextContent(item["content"], "input[0].content", "input_text")
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
		if err := rejectChatGPTFields(part, fmt.Sprintf("%s[%d]", feature, index), "type", "text"); err != nil {
			return "", err
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

func (h *Handler) handleChatGPTWebSearchResponses(w http.ResponseWriter, r *http.Request, started time.Time, provider, model string, stream bool, invocation chatGPTWebSearchInvocation, body map[string]any) {
	round := archiveRoundFromContext(r.Context())
	h.cfgMu.RLock()
	executor := h.chatGPTSearch
	h.cfgMu.RUnlock()
	if executor == nil {
		apiErr := APIError{Code: ErrorCodeProviderUnavailable, Message: "chatgpt web search executor is unavailable", Model: model}
		h.writeChatGPTWebAPIError(w, round, r, started, provider, model, stream, http.StatusServiceUnavailable, apiErr, streamFailFromKind(chatgptfail.KindProviderUnavailable, apiErr.Code+": "+apiErr.Message, nil), tokenUsage{})
		return
	}
	response := newChatGPTResponse(body)
	messageID := response.messageID()
	searchID := "ws_" + strings.TrimPrefix(response.ID, "resp_")
	usageRequest := chatgpttext.Request{Model: model, Messages: []chatgpttext.Message{{Role: "user", Content: invocation.Query}}}
	events := chatGPTResponseStream{writer: w}
	settleWriteFailure := func(err error, tok tokenUsage, billingModel string) {
		fail := newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", err, false)
		_ = h.writeArchiveResponse(round, "response.sse", []byte(events.archive.String()))
		h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, fail)
	}
	if stream {
		err := events.start(response, model)
		if err == nil {
			err = events.write("response.output_item.added", map[string]any{"output_index": 0, "item": chatGPTWebSearchCallItem(searchID, invocation.Query, "in_progress", nil)})
		}
		for _, kind := range []string{"response.web_search_call.in_progress", "response.web_search_call.searching"} {
			if err == nil {
				err = events.write(kind, map[string]any{"output_index": 0, "item_id": searchID})
			}
		}
		if err != nil {
			settleWriteFailure(err, tokenUsage{}, model)
			return
		}
	}
	result, execErr := executor.Search(r.Context(), chatgptsearch.Request{Model: model, Query: invocation.Query})
	execErr = chatgptsearch.ValidateCompletion(&result, execErr)
	billingModel := firstNonEmpty(result.ActualModel, model)
	text, annotations := presentChatGPTWebSearch(result)
	if execErr == nil && text == "" {
		execErr = chatgptfail.New(chatgptfail.KindTextIncomplete, fmt.Errorf("search answer has no visible text"))
	}
	tok := estimateChatGPTTextUsage(usageRequest, text)
	var fail *streamFail
	status := "completed"
	if execErr != nil {
		fail = streamFailFromChatGPTSearchErr(execErr)
		status = "failed"
	} else if result.Completion.Incomplete(result.Text) {
		status = "incomplete"
		fail = streamFailFromKind(chatgptfail.KindTextModelLength, "chatgpt web search reached model length limit", nil)
	}
	recordSearchCompletion(r.Context(), result.Completion, events.started, fail)
	itemStatus := status
	if status == "failed" {
		itemStatus = "incomplete"
	}
	searchItem := chatGPTWebSearchCallItem(searchID, invocation.Query, "completed", result.Sources)
	messageItem := chatGPTResponseMessage(messageID, text, annotations, itemStatus)
	output := []any{searchItem, messageItem}
	payload := response.payload(billingModel, output, tok, status)
	if status == "failed" {
		searchItem = chatGPTWebSearchCallItem(searchID, invocation.Query, "failed", result.Sources)
		output = []any{searchItem}
		if text != "" {
			output = append(output, messageItem)
		}
		payload = response.failed(billingModel, output, fail)
	}
	if !stream {
		httpStatus := http.StatusOK
		if status == "failed" {
			httpStatus = statusForChatGPTFailure(fail)
		}
		h.writeChatGPTResponseJSON(w, r, started, provider, billingModel, payload, httpStatus, tok, fail)
		return
	}
	if status == "failed" {
		err := events.write("response.output_item.done", map[string]any{"output_index": 0, "item": searchItem})
		if err == nil && text != "" {
			err = events.addText(messageID, 1)
			if err == nil {
				err = events.delta(messageID, 1, text)
			}
		}
		if err == nil {
			err = events.write("response.failed", map[string]any{"response": payload})
		}
		if err != nil {
			settleWriteFailure(err, tok, billingModel)
			return
		}
	} else {
		err := events.write("response.web_search_call.completed", map[string]any{"output_index": 0, "item_id": searchID})
		if err == nil {
			err = events.write("response.output_item.done", map[string]any{"output_index": 0, "item": searchItem})
		}
		if err == nil {
			err = events.addText(messageID, 1)
		}
		if err == nil && text != "" {
			err = events.delta(messageID, 1, text)
		}
		if err == nil {
			err = events.finishText(messageID, 1, text, annotations, itemStatus)
		}
		if err == nil {
			err = events.write("response."+status, map[string]any{"response": payload})
		}
		if err != nil {
			settleWriteFailure(err, tok, billingModel)
			return
		}
	}
	_ = h.writeArchiveResponse(round, "response.sse", []byte(events.archive.String()))
	h.settleChatGPTWeb(round, r, provider, billingModel, true, http.StatusOK, time.Since(started), tok, fail)
}

func streamFailFromChatGPTSearchErr(err error) *streamFail {
	return streamFailFromChatGPTErr(err)
}

func presentChatGPTWebSearch(result chatgptsearch.Result) (string, []openAIURLCitation) {
	text := cleanChatGPTWebSearchText(result.Text)
	annotations := make([]openAIURLCitation, 0, len(result.Sources))
	if text == "" || len(result.Sources) == 0 {
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
			var start int
			if title == source.URL {
				start = utf8.RuneCountInString(text) + utf8.RuneCountInString(fmt.Sprintf("%d. ", index+1))
			} else {
				line += " - "
				start = utf8.RuneCountInString(text) + utf8.RuneCountInString(line)
				line += source.URL
			}
			citation := openAIURLCitation{Type: "url_citation"}
			citation.StartIndex = start
			citation.EndIndex = start + utf8.RuneCountInString(source.URL)
			citation.URL, citation.Title = source.URL, title
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
