package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
)

type codexChatStreamState struct {
	ID, Model     string
	Input, Output int
	Started, Done bool
	HasToolCall   bool
}

func (h *Handler) handleChatToCodex(w http.ResponseWriter, r *http.Request, started time.Time, plan TransportPlan, model string, stream bool, body map[string]any) {
	round := archiveRoundFromContext(r.Context())
	if h.codexResponses == nil {
		h.writeCodexResponsesError(w, r, round, started, plan.RouteOwner, model, stream, codexresponses.NewFailure(codexresponses.KindProviderUnavailable, 0, fmt.Errorf("Codex Responses executor is unavailable")))
		return
	}
	raw, err := buildCodexResponsesFromChat(body, model)
	if err != nil {
		h.writeArchivedAPIError(w, round, r, started, plan.RouteOwner, model, stream, http.StatusBadRequest, conversionAPIError(plan, err))
		return
	}
	normalized, normalizedBody, ignored, err := normalizeCodexRequest(raw, false)
	if err != nil {
		h.writeArchivedError(w, round, r, started, plan.RouteOwner, model, stream, http.StatusBadRequest, err.Error())
		return
	}
	markConversionDegraded(round, ignored)
	h.archiveAndLogTransportPlan(round, r, plan, effectivecatalog.BuiltinProviderViewFor(plan.RouteOwner), stream)
	request := codexresponses.Request{Model: model, Body: normalized, SessionHash: codexSessionHash(r, model, normalizedBody)}
	if stream {
		h.streamChatFromCodex(w, r, started, plan, model, request)
		return
	}
	result, execErr := h.codexResponses.CompleteCodexResponses(r.Context(), request)
	if execErr != nil {
		h.writeCodexResponsesError(w, r, round, started, plan.RouteOwner, model, false, execErr)
		return
	}
	converted, usage, convertErr := convertCodexResponsesToChat(result.Body, model)
	if convertErr != nil {
		h.writeArchivedError(w, round, r, started, plan.RouteOwner, model, false, http.StatusBadGateway, "upstream_protocol_error: "+convertErr.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(converted)
	_ = h.writeArchiveResponse(round, "response.json", converted)
	duration := time.Since(started)
	h.recordAndPrint(round, r, plan.RouteOwner, model, false, http.StatusOK, duration, usage, "")
	h.writeArchiveMetadata(round, plan.RouteOwner, model, false, http.StatusOK, duration, usage, "response.json", "", "", "success")
}

func buildCodexResponsesFromChat(body map[string]any, model string) ([]byte, error) {
	if err := rejectConversionFields(body, map[string]struct{}{
		"model": {}, "messages": {}, "stream": {}, "tools": {}, "tool_choice": {},
		"functions": {}, "function_call": {}, "max_tokens": {}, "max_completion_tokens": {},
		"temperature": {}, "top_p": {}, "frequency_penalty": {}, "presence_penalty": {},
		"stop": {}, "stream_options": {}, "user": {}, "metadata": {},
	}); err != nil {
		return nil, err
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 || len(messages) > maxConversionContentBlocks {
		return nil, fmt.Errorf("messages")
	}
	input := make([]any, 0, len(messages))
	var instructions []string
	for i, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("messages[%d]", i)
		}
		if err := rejectConversionFields(message, map[string]struct{}{"role": {}, "content": {}, "name": {}, "tool_calls": {}, "tool_call_id": {}}); err != nil {
			return nil, fmt.Errorf("messages[%d].%w", i, err)
		}
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			text, err := extractTextContent(message["content"], "chat->responses")
			if err != nil {
				return nil, fmt.Errorf("messages[%d].content: %w", i, err)
			}
			if text != "" {
				instructions = append(instructions, text)
			}
		case "user", "assistant":
			if message["content"] != nil {
				text, err := extractTextContent(message["content"], "chat->responses")
				if err != nil {
					return nil, fmt.Errorf("messages[%d].content: %w", i, err)
				}
				if text != "" {
					input = append(input, map[string]any{"type": "message", "role": role, "content": text})
				}
			}
			if calls, exists := message["tool_calls"]; exists {
				mapped, err := chatToolCallsToResponses(calls)
				if err != nil {
					return nil, fmt.Errorf("messages[%d].tool_calls: %w", i, err)
				}
				input = append(input, mapped...)
			}
		case "tool":
			callID, _ := message["tool_call_id"].(string)
			content, err := stringifyToolOutput(message["content"])
			if err != nil || strings.TrimSpace(callID) == "" {
				return nil, fmt.Errorf("messages[%d].tool result", i)
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": content})
		default:
			return nil, fmt.Errorf("messages[%d].role", i)
		}
	}
	out := map[string]any{"model": model, "input": input, "stream": true}
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n\n")
	}
	if rawTools, exists := body["tools"]; exists {
		tools, err := chatToolsToResponses(rawTools)
		if err != nil {
			return nil, err
		}
		out["tools"] = tools
	}
	if rawChoice, exists := body["tool_choice"]; exists {
		choice, err := chatToolChoiceToResponses(rawChoice)
		if err != nil {
			return nil, err
		}
		out["tool_choice"] = choice
	}
	for _, key := range []string{"functions", "function_call", "max_tokens", "max_completion_tokens", "temperature", "top_p", "frequency_penalty", "presence_penalty", "user", "metadata", "stream_options"} {
		if value, exists := body[key]; exists {
			out[key] = value
		}
	}
	if stop, exists := body["stop"]; exists && hasNonEmptyConversionFeature(stop) {
		return nil, fmt.Errorf("stop")
	}
	return json.Marshal(out)
}

func chatToolsToResponses(raw any) ([]any, error) {
	tools, ok := raw.([]any)
	if !ok || len(tools) > maxConversionTools {
		return nil, fmt.Errorf("tools")
	}
	out := make([]any, 0, len(tools))
	for i, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tools[%d]", i)
		}
		if err := rejectConversionFields(tool, map[string]struct{}{"type": {}, "function": {}}); err != nil {
			return nil, fmt.Errorf("tools[%d].%w", i, err)
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tools[%d].function", i)
		}
		if err := rejectConversionFields(function, map[string]struct{}{"name": {}, "description": {}, "parameters": {}, "strict": {}}); err != nil {
			return nil, fmt.Errorf("tools[%d].function.%w", i, err)
		}
		name, _ := function["name"].(string)
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("tools[%d].function.name", i)
		}
		mapped := map[string]any{"type": "function", "name": name}
		for _, key := range []string{"description", "parameters", "strict"} {
			if value, exists := function[key]; exists {
				mapped[key] = value
			}
		}
		out = append(out, mapped)
	}
	return out, nil
}

func chatToolChoiceToResponses(raw any) (any, error) {
	if value, ok := raw.(string); ok {
		switch value {
		case "none", "auto", "required":
			return value, nil
		default:
			return nil, fmt.Errorf("tool_choice")
		}
	}
	choice, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tool_choice")
	}
	function, _ := choice["function"].(map[string]any)
	name, _ := function["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("tool_choice.function.name")
	}
	return map[string]any{"type": "function", "name": name}, nil
}

func chatToolCallsToResponses(raw any) ([]any, error) {
	calls, ok := raw.([]any)
	if !ok || len(calls) > maxConversionTools {
		return nil, fmt.Errorf("must be an array")
	}
	out := make([]any, 0, len(calls))
	for i, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("[%d]", i)
		}
		if err := rejectConversionFields(call, map[string]struct{}{"id": {}, "type": {}, "function": {}}); err != nil {
			return nil, err
		}
		function, _ := call["function"].(map[string]any)
		id, _ := call["id"].(string)
		name, _ := function["name"].(string)
		arguments, _ := function["arguments"].(string)
		if id == "" || name == "" || len(arguments) > maxConversionToolArgumentBytes || !json.Valid([]byte(arguments)) {
			return nil, fmt.Errorf("[%d].function", i)
		}
		out = append(out, map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": arguments})
	}
	return out, nil
}

func convertCodexResponsesToChat(body []byte, fallbackModel string) ([]byte, tokenUsage, error) {
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, tokenUsage{}, err
	}
	model, _ := response["model"].(string)
	if model == "" {
		model = fallbackModel
	}
	id, _ := response["id"].(string)
	if id == "" {
		id = "chatcmpl-codex"
	}
	message := map[string]any{"role": "assistant", "content": ""}
	var text strings.Builder
	var calls []any
	output, _ := response["output"].([]any)
	for i, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, tokenUsage{}, fmt.Errorf("output[%d]", i)
		}
		switch typ, _ := item["type"].(string); typ {
		case "message":
			parts, _ := item["content"].([]any)
			for _, rawPart := range parts {
				if part, ok := rawPart.(map[string]any); ok {
					if typ, _ := part["type"].(string); typ == "output_text" {
						value, ok := part["text"].(string)
						if !ok {
							return nil, tokenUsage{}, fmt.Errorf("output text")
						}
						text.WriteString(value)
					}
				}
			}
		case "function_call":
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			arguments, _ := item["arguments"].(string)
			if callID == "" || name == "" || !json.Valid([]byte(arguments)) {
				return nil, tokenUsage{}, fmt.Errorf("output function_call")
			}
			calls = append(calls, map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": arguments}})
		case "reasoning":
			continue
		default:
			return nil, tokenUsage{}, fmt.Errorf("output.%s", typ)
		}
	}
	message["content"] = text.String()
	finish := "stop"
	if len(calls) > 0 {
		message["tool_calls"] = calls
		finish = "tool_calls"
	}
	status, _ := response["status"].(string)
	if status == "incomplete" {
		finish = codexChatIncompleteFinishReason(response)
	} else if status != "" && status != "completed" {
		return nil, tokenUsage{}, fmt.Errorf("response.status.%s", status)
	}
	usage := tokenUsage{}
	if rawUsage, ok := response["usage"].(map[string]any); ok {
		usage.PromptTokens = intNumber(rawUsage["input_tokens"])
		usage.CompletionTokens = intNumber(rawUsage["output_tokens"])
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		usage.Known = true
	}
	out := map[string]any{"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": openAIUsagePayload(usage)}
	encoded, err := json.Marshal(out)
	return encoded, usage, err
}

func (h *Handler) streamChatFromCodex(w http.ResponseWriter, r *http.Request, started time.Time, plan TransportPlan, model string, request codexresponses.Request) {
	round := archiveRoundFromContext(r.Context())
	state := &codexChatStreamState{Model: model}
	var archive bytes.Buffer
	streamStarted := false
	emit := func(line []byte) error {
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(line)), "data:"))
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		chunks, err := codexResponsesEventToChat([]byte(payload), state)
		if err != nil {
			return err
		}
		if len(chunks) == 0 {
			return nil
		}
		if !streamStarted {
			prepareSSEHeaders(w.Header())
			w.WriteHeader(http.StatusOK)
			streamStarted = true
		}
		for _, chunk := range chunks {
			encoded := sseData(chunk)
			if archive.Len()+len(encoded) > maxConversionSSEBytes {
				return fmt.Errorf("conversion SSE exceeds %d bytes", maxConversionSSEBytes)
			}
			if _, err := w.Write(encoded); err != nil {
				return err
			}
			archive.Write(encoded)
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	}
	err := h.codexResponses.StreamCodexResponses(r.Context(), request, func(codexresponses.StreamStart) error { return nil }, emit)
	usage := tokenUsage{PromptTokens: state.Input, CompletionTokens: state.Output, TotalTokens: state.Input + state.Output, Known: state.Input > 0 || state.Output > 0}
	duration := time.Since(started)
	_ = h.writeArchiveResponse(round, "response.sse", archive.Bytes())
	if err != nil || !state.Done {
		if err == nil {
			err = fmt.Errorf("conversion SSE ended without terminal event")
		}
		if !streamStarted {
			h.writeCodexResponsesError(w, r, round, started, plan.RouteOwner, model, true, err)
			return
		}
		fail := conversionStreamFailure(err)
		h.recordAndPrintFail(round, r, plan.RouteOwner, model, true, http.StatusOK, duration, usage, fail)
		h.writeArchiveMetadata(round, plan.RouteOwner, model, true, http.StatusOK, duration, usage, "response.sse", err.Error(), "", outcomeFromStreamFail(fail, http.StatusOK))
		return
	}
	h.recordAndPrint(round, r, plan.RouteOwner, model, true, http.StatusOK, duration, usage, "")
	h.writeArchiveMetadata(round, plan.RouteOwner, model, true, http.StatusOK, duration, usage, "response.sse", "", "", "success")
}

func codexResponsesEventToChat(payload []byte, state *codexChatStreamState) ([][]byte, error) {
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, err
	}
	typ, _ := event["type"].(string)
	created := time.Now().Unix()
	chunk := func(delta map[string]any, finish any, usage tokenUsage) []byte {
		return openAIStreamChunk(state.ID, state.Model, created, []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}, usage)
	}
	switch typ {
	case "response.created", "response.in_progress":
		if state.Started {
			return nil, nil
		}
		response, _ := event["response"].(map[string]any)
		state.ID, _ = response["id"].(string)
		if value, _ := response["model"].(string); value != "" {
			state.Model = value
		}
		state.Started = true
		return [][]byte{chunk(map[string]any{"role": "assistant"}, nil, tokenUsage{})}, nil
	case "response.output_text.delta":
		value, ok := event["delta"].(string)
		if !ok {
			return nil, fmt.Errorf("response.output_text.delta")
		}
		return [][]byte{chunk(map[string]any{"content": value}, nil, tokenUsage{})}, nil
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		if itemType, _ := item["type"].(string); itemType == "function_call" {
			state.HasToolCall = true
			id, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			return [][]byte{chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": ""}}}}, nil, tokenUsage{})}, nil
		}
		return nil, nil
	case "response.function_call_arguments.delta":
		value, ok := event["delta"].(string)
		if !ok {
			return nil, fmt.Errorf("response.function_call_arguments.delta")
		}
		return [][]byte{chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": value}}}}, nil, tokenUsage{})}, nil
	case "response.output_text.done", "response.function_call_arguments.done", "response.output_item.done", "response.content_part.added", "response.content_part.done":
		return nil, nil
	case "response.reasoning.delta", "response.reasoning.done", "response.reasoning_text.delta", "response.reasoning_text.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		return nil, nil
	case "response.completed", "response.incomplete":
		response, _ := event["response"].(map[string]any)
		if rawUsage, ok := response["usage"].(map[string]any); ok {
			state.Input = intNumber(rawUsage["input_tokens"])
			state.Output = intNumber(rawUsage["output_tokens"])
		}
		finish := "stop"
		if state.HasToolCall {
			finish = "tool_calls"
		}
		if typ == "response.incomplete" {
			finish = codexChatIncompleteFinishReason(response)
		}
		state.Done = true
		usage := tokenUsage{PromptTokens: state.Input, CompletionTokens: state.Output, TotalTokens: state.Input + state.Output, Known: true}
		return [][]byte{chunk(map[string]any{}, finish, usage), []byte("[DONE]")}, nil
	default:
		return nil, fmt.Errorf("responses stream event %q", typ)
	}
}

func codexChatIncompleteFinishReason(response map[string]any) string {
	details, _ := response["incomplete_details"].(map[string]any)
	reason, _ := details["reason"].(string)
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "max_tokens", "max_output_tokens":
		return "length"
	case "content_filter":
		return "content_filter"
	default:
		return "stop"
	}
}
