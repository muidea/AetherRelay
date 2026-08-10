package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	archive "aetherrelay/internal/pkg/aetherrelayarchive"
	"aetherrelay/internal/pkg/aetherrelayconfig"
)

type toolCallRegistry struct {
	mu    sync.Mutex
	calls map[string]struct{}
}

func newToolCallRegistry() *toolCallRegistry { return &toolCallRegistry{calls: map[string]struct{}{}} }
func (r *toolCallRegistry) Add(id string) error {
	if r == nil || id == "" {
		return fmt.Errorf("tool call id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.calls[id]; ok {
		return fmt.Errorf("duplicate tool call id %q", id)
	}
	if len(r.calls) >= 128 {
		return fmt.Errorf("too many active tool calls")
	}
	r.calls[id] = struct{}{}
	return nil
}
func (r *toolCallRegistry) Resolve(id string) error {
	if r == nil || id == "" {
		return fmt.Errorf("tool result call id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.calls[id]; !ok {
		return fmt.Errorf("unknown tool call id %q", id)
	}
	delete(r.calls, id)
	return nil
}

func (r *toolCallRegistry) EnsureResolved() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 0 {
		return fmt.Errorf("unresolved tool calls")
	}
	return nil
}

type conversionSSEMapper func([]byte, *textConversionStreamState) ([]map[string]any, error)

const maxConversionSSEBytes = 64 << 20

const (
	maxConversionToolSchemaBytes   = 256 << 10
	maxConversionToolArgumentBytes = 1 << 20
	maxConversionSchemaDepth       = 32
	maxConversionContentBlocks     = 256
	maxConversionTools             = 128
)

func serveConvertedSSE(ctx context.Context, w http.ResponseWriter, input io.Reader, mapper conversionSSEMapper, state *textConversionStreamState, includeEventName bool) error {
	return serveConvertedSSEWithTimeouts(ctx, w, input, mapper, state, includeEventName, 0, 0)
}

// serveConvertedSSEWithTimeouts converts one upstream SSE stream while
// enforcing first-event and inter-event deadlines.  The input is closed when
// the client cancels or a deadline fires so a blocked upstream read cannot
// keep the request alive indefinitely. Headers are delayed until the first
// converted event, allowing a malformed first event to return a normal HTTP
// error instead of a misleading 200 SSE response.
func serveConvertedSSEWithTimeouts(ctx context.Context, w http.ResponseWriter, input io.Reader, mapper conversionSSEMapper, state *textConversionStreamState, includeEventName bool, firstEventTimeout, idleTimeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if input == nil || mapper == nil || state == nil {
		return fmt.Errorf("conversion SSE service requires input, mapper and state")
	}
	if firstEventTimeout <= 0 {
		firstEventTimeout = 30 * time.Second
	}
	if idleTimeout <= 0 {
		idleTimeout = firstEventTimeout
	}
	flusher, _ := w.(http.Flusher)
	lineCh := make(chan conversionSSELine, 1)
	stopCh := make(chan struct{})
	reader := bufio.NewScanner(input)
	reader.Buffer(make([]byte, 4096), 1<<20)
	go func() {
		for reader.Scan() {
			select {
			case lineCh <- conversionSSELine{line: reader.Text()}:
			case <-stopCh:
				return
			}
		}
		select {
		case lineCh <- conversionSSELine{done: true, err: reader.Err()}:
		case <-stopCh:
		}
	}()
	closeInput := func() {
		if closer, ok := input.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	defer func() {
		close(stopCh)
		closeInput()
	}()
	timer := time.NewTimer(firstEventTimeout)
	defer timer.Stop()
	resetTimer := func(d time.Duration) {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
	}
	headersWritten := false
	for {
		select {
		case <-ctx.Done():
			closeInput()
			return ctx.Err()
		case <-timer.C:
			closeInput()
			if !headersWritten {
				return fmt.Errorf("upstream SSE first/next event timeout after %s", firstEventTimeout.Truncate(time.Millisecond))
			}
			return fmt.Errorf("upstream SSE idle timeout after %s", idleTimeout.Truncate(time.Millisecond))
		case result := <-lineCh:
			if result.done {
				if result.err != nil {
					return result.err
				}
				if state.Completed {
					return nil
				}
				return fmt.Errorf("conversion SSE ended without terminal event")
			}
			line := strings.TrimSpace(result.line)
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			events, err := mapper([]byte(payload), state)
			if err != nil {
				return err
			}
			// Only a syntactically valid protocol event proves upstream progress.
			// Blank lines, comments and malformed data cannot extend deadlines.
			resetTimer(idleTimeout)
			encoded, err := encodeConversionSSE(events, includeEventName)
			if err != nil {
				return err
			}
			if encoded == nil {
				if state.Completed {
					return nil
				}
				continue
			}
			if state.Output.Len()+len(encoded) > maxConversionSSEBytes {
				return fmt.Errorf("conversion SSE exceeds %d bytes", maxConversionSSEBytes)
			}
			if !headersWritten {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				w.WriteHeader(http.StatusOK)
				headersWritten = true
				state.HeadersWritten = true
			}
			if _, err := w.Write(encoded); err != nil {
				return fmt.Errorf("client write: %w", err)
			}
			state.Output.Write(encoded)
			state.BytesWritten += int64(len(encoded))
			if flusher != nil {
				flusher.Flush()
			}
			if state.Completed {
				return nil
			}
		}
	}
}

type conversionSSELine struct {
	line string
	err  error
	done bool
}

func convertSSEReader(input io.Reader, mapper conversionSSEMapper, state *textConversionStreamState, includeEventName bool) ([]byte, error) {
	return convertSSEReaderContext(context.Background(), input, mapper, state, includeEventName)
}

func convertSSEReaderContext(ctx context.Context, input io.Reader, mapper conversionSSEMapper, state *textConversionStreamState, includeEventName bool) ([]byte, error) {
	if input == nil || mapper == nil || state == nil {
		return nil, fmt.Errorf("conversion SSE reader requires input, mapper and state")
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var output bytes.Buffer
	sawTerminal := false
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		events, err := mapper([]byte(payload), state)
		if err != nil {
			return nil, err
		}
		encoded, err := encodeConversionSSE(events, includeEventName)
		if err != nil {
			return nil, err
		}
		if output.Len()+len(encoded) > maxConversionSSEBytes {
			return nil, fmt.Errorf("conversion SSE exceeds %d bytes", maxConversionSSEBytes)
		}
		output.Write(encoded)
		if state.Completed {
			sawTerminal = true
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !sawTerminal {
		return nil, fmt.Errorf("conversion SSE ended without terminal event")
	}
	return output.Bytes(), nil
}

func encodeConversionSSE(events []map[string]any, includeEventName bool) ([]byte, error) {
	var out bytes.Buffer
	for _, event := range events {
		if len(event) == 0 {
			continue
		}
		typ, _ := event["type"].(string)
		if typ == "" {
			return nil, fmt.Errorf("conversion SSE event type is required")
		}
		payload, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		if includeEventName {
			out.WriteString("event: ")
			out.WriteString(typ)
			out.WriteByte('\n')
		}
		out.WriteString("data: ")
		out.Write(payload)
		out.WriteString("\n\n")
	}
	return out.Bytes(), nil
}

func (h *Handler) handleResponsesToAnthropic(w http.ResponseWriter, r *http.Request, resp *http.Response, round *archive.Round, start time.Time, provider, model string, capability config.ConversionCapability) {
	if resp != nil && isEventStreamContentType(resp.Header.Get("Content-Type")) {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		h.writeArchivedError(w, round, r, start, provider, model, false, http.StatusBadGateway, "upstream_protocol_error: stream=false request returned event-stream")
		return
	}
	body, err := h.readLimitedUpstreamContext(r.Context(), resp.Body, h.currentConfig().UpstreamBodyIdleTimeout)
	if err != nil {
		h.writeArchivedError(w, round, r, start, provider, model, false, http.StatusBadGateway, err.Error())
		return
	}
	conversionStart := time.Now()
	converted, usage, ignored, err := convertAnthropicToResponsesWithCapability(body, model, capability)
	if round != nil {
		round.SetConversionDuration(time.Since(conversionStart))
	}
	if err != nil {
		h.writeArchivedError(w, round, r, start, provider, model, false, http.StatusBadGateway, "upstream_protocol_error: "+err.Error())
		return
	}
	markConversionDegraded(round, ignored)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(converted)
	_ = h.writeArchiveResponse(round, "response.json", converted)
	d := time.Since(start)
	h.recordAndPrint(round, r, provider, model, false, resp.StatusCode, d, usage, "")
	h.writeArchiveMetadata(round, provider, model, false, resp.StatusCode, d, usage, "response.json", "", "", "success")
}

func (h *Handler) handleResponsesToAnthropicStream(w http.ResponseWriter, r *http.Request, resp *http.Response, round *archive.Round, start time.Time, provider, model string, capability config.ConversionCapability) error {
	return h.handleConvertedSSE(w, r, resp, round, start, provider, model, anthropicEventToResponsesWithCapability(capability))
}

func (h *Handler) handleAnthropicToResponses(w http.ResponseWriter, r *http.Request, resp *http.Response, round *archive.Round, start time.Time, provider, model string, capability config.ConversionCapability) {
	if resp != nil && isEventStreamContentType(resp.Header.Get("Content-Type")) {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		h.writeArchivedError(w, round, r, start, provider, model, false, http.StatusBadGateway, "upstream_protocol_error: stream=false request returned event-stream")
		return
	}
	body, err := h.readLimitedUpstreamContext(r.Context(), resp.Body, h.currentConfig().UpstreamBodyIdleTimeout)
	if err != nil {
		h.writeArchivedError(w, round, r, start, provider, model, false, http.StatusBadGateway, err.Error())
		return
	}
	conversionStart := time.Now()
	converted, usage, ignored, err := convertOpenAIResponsesToAnthropicWithCapability(body, model, capability)
	if round != nil {
		round.SetConversionDuration(time.Since(conversionStart))
	}
	if err != nil {
		h.writeArchivedError(w, round, r, start, provider, model, false, http.StatusBadGateway, "upstream_protocol_error: "+err.Error())
		return
	}
	markConversionDegraded(round, ignored)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(converted)
	_ = h.writeArchiveResponse(round, "response.json", converted)
	d := time.Since(start)
	h.recordAndPrint(round, r, provider, model, false, resp.StatusCode, d, usage, "")
	h.writeArchiveMetadata(round, provider, model, false, resp.StatusCode, d, usage, "response.json", "", "", "success")
}

func (h *Handler) handleAnthropicToResponsesStream(w http.ResponseWriter, r *http.Request, resp *http.Response, round *archive.Round, start time.Time, provider, model string, capability config.ConversionCapability) error {
	return h.handleConvertedSSE(w, r, resp, round, start, provider, model, responsesEventToAnthropicWithCapability(capability))
}

func (h *Handler) handleConvertedSSE(w http.ResponseWriter, r *http.Request, resp *http.Response, round *archive.Round, start time.Time, provider, model string, mapper conversionSSEMapper) error {
	if resp == nil || !isEventStreamContentType(resp.Header.Get("Content-Type")) {
		err := fmt.Errorf("upstream_protocol_error: conversion stream requires text/event-stream")
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		h.writeArchivedError(w, round, r, start, provider, model, true, http.StatusBadGateway, err.Error())
		return err
	}
	state := &textConversionStreamState{}
	conversionStart := time.Now()
	cfg := h.currentConfig()
	firstTimeout := cfg.StreamFirstEventTimeout
	if firstTimeout <= 0 {
		firstTimeout = 30 * time.Second
	}
	idleTimeout := cfg.StreamIdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = firstTimeout
	}
	err := serveConvertedSSEWithTimeouts(r.Context(), w, resp.Body, mapper, state, true, firstTimeout, idleTimeout)
	if round != nil {
		round.SetConversionDuration(time.Since(conversionStart))
	}
	if state.Output.Len() > 0 {
		if archiveErr := h.writeArchiveResponse(round, "response.sse", state.Output.Bytes()); archiveErr != nil {
			log.Printf("archive converted SSE: %v", archiveErr)
		}
	}
	markConversionDegraded(round, state.IgnoredFeatures)
	usage := tokenUsage{PromptTokens: state.InputTokens, CompletionTokens: state.OutputTokens, TotalTokens: state.InputTokens + state.OutputTokens, Known: state.InputTokens > 0 || state.OutputTokens > 0}
	duration := time.Since(start)
	if err != nil {
		if !state.HeadersWritten {
			h.writeArchivedError(w, round, r, start, provider, model, true, http.StatusBadGateway, "upstream_protocol_error: "+err.Error())
			return err
		}
		failure := conversionStreamFailure(err)
		h.recordAndPrintFail(round, r, provider, model, true, http.StatusOK, duration, usage, failure)
		h.writeArchiveMetadata(round, provider, model, true, http.StatusOK, duration, usage, "response.sse", err.Error(), "", outcomeFromStreamFail(failure, http.StatusOK))
		return err
	}
	h.recordAndPrint(round, r, provider, model, true, http.StatusOK, duration, usage, "")
	h.writeArchiveMetadata(round, provider, model, true, http.StatusOK, duration, usage, "response.sse", "", "", "success")
	return nil
}

func conversionStreamFailure(err error) *streamFail {
	if err == nil {
		return nil
	}
	message := "converted SSE: " + err.Error()
	lower := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, context.Canceled) || strings.Contains(lower, "context canceled"):
		return newStreamFail(streamKindClientCanceled, message, err, false)
	case strings.Contains(lower, "client write") || strings.Contains(lower, "broken pipe"):
		return newStreamFail(streamKindClientWrite, message, err, false)
	case strings.Contains(lower, "idle timeout") || strings.Contains(lower, "first event timeout") || strings.Contains(lower, "first/next event timeout"):
		return newStreamFail(streamKindIdleTimeout, message, err, true)
	case strings.Contains(lower, "exceeds") || strings.Contains(lower, "limit"):
		return newStreamFail(streamKindLimitExceeded, message, err, false)
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(lower, "without terminal") || strings.Contains(lower, "closed before"):
		return newStreamFail(streamKindUpstreamTrunc, message, err, true)
	default:
		return newStreamFail(streamKindProtocol, message, err, true)
	}
}

func convertOpenAIResponsesToAnthropic(body []byte, fallbackModel string) ([]byte, tokenUsage, error) {
	encoded, usage, _, err := convertOpenAIResponsesToAnthropicWithCapability(body, fallbackModel, config.ConversionCapability{})
	return encoded, usage, err
}

func convertOpenAIResponsesToAnthropicWithCapability(body []byte, fallbackModel string, capability config.ConversionCapability) ([]byte, tokenUsage, []string, error) {
	var in struct {
		ID                string `json:"id"`
		Model             string `json:"model"`
		Status            string `json:"status"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []map[string]any `json:"output"`
		Usage  struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, tokenUsage{}, nil, err
	}
	var content []any
	var ignored []string
	registry := newToolCallRegistry()
	toolStop := false
	for _, item := range in.Output {
		if _, exists := item["internal_chat_message_metadata_passthrough"]; exists {
			// Krill/Codex-compatible Responses relays may attach opaque
			// internal message metadata to output items. It has no Anthropic
			// semantic equivalent and must never be exposed as content.
			delete(item, "internal_chat_message_metadata_passthrough")
			ignored = append(ignored, "internal_chat_message_metadata_passthrough")
		}
		if _, exists := item["metadata"]; exists {
			delete(item, "metadata")
			ignored = append(ignored, "output_metadata")
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "message":
			if err := rejectConversionFields(item, map[string]struct{}{"type": {}, "id": {}, "role": {}, "status": {}, "content": {}, "phase": {}}); err != nil {
				return nil, tokenUsage{}, nil, fmt.Errorf("output.message.%w", err)
			}
			if phase, exists := item["phase"]; exists {
				value, ok := phase.(string)
				if !ok || value != "final_answer" {
					return nil, tokenUsage{}, nil, fmt.Errorf("output.message.phase")
				}
			}
			blocks, ok := item["content"].([]any)
			if !ok {
				return nil, tokenUsage{}, nil, fmt.Errorf("output.message.content")
			}
			for _, rawBlock := range blocks {
				block, ok := rawBlock.(map[string]any)
				if !ok {
					return nil, tokenUsage{}, nil, fmt.Errorf("output.message.content")
				}
				blockType, _ := block["type"].(string)
				if blockType != "output_text" {
					return nil, tokenUsage{}, nil, fmt.Errorf("output.%s", blockType)
				}
				if err := rejectConversionFields(block, map[string]struct{}{"type": {}, "text": {}, "annotations": {}, "logprobs": {}}); err != nil {
					return nil, tokenUsage{}, nil, fmt.Errorf("output.output_text.%w", err)
				}
				if hasNonEmptyConversionFeature(block["annotations"]) {
					return nil, tokenUsage{}, nil, fmt.Errorf("output.output_text.annotations")
				}
				if hasNonEmptyConversionFeature(block["logprobs"]) {
					return nil, tokenUsage{}, nil, fmt.Errorf("output.output_text.logprobs")
				}
				text, ok := block["text"].(string)
				if !ok {
					return nil, tokenUsage{}, nil, fmt.Errorf("output.output_text.text")
				}
				content = append(content, map[string]any{"type": "text", "text": text})
			}
		case "function_call":
			if err := rejectConversionFields(item, map[string]struct{}{"type": {}, "id": {}, "call_id": {}, "name": {}, "arguments": {}, "status": {}}); err != nil {
				return nil, tokenUsage{}, nil, fmt.Errorf("output.function_call.%w", err)
			}
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			arguments, _ := item["arguments"].(string)
			if callID == "" || name == "" || len(arguments) > maxConversionToolArgumentBytes || !json.Valid([]byte(arguments)) {
				return nil, tokenUsage{}, nil, fmt.Errorf("output.function_call")
			}
			var input any
			if err := json.Unmarshal([]byte(arguments), &input); err != nil {
				return nil, tokenUsage{}, nil, err
			}
			if err := registry.Add(callID); err != nil {
				return nil, tokenUsage{}, nil, err
			}
			content = append(content, map[string]any{"type": "tool_use", "id": callID, "name": name, "input": input})
			toolStop = true
		case "function_call_output":
			if err := rejectConversionFields(item, map[string]struct{}{"type": {}, "id": {}, "call_id": {}, "output": {}, "status": {}}); err != nil {
				return nil, tokenUsage{}, nil, fmt.Errorf("output.function_call_output.%w", err)
			}
			callID, _ := item["call_id"].(string)
			output, err := stringifyToolOutput(item["output"])
			if err != nil {
				return nil, tokenUsage{}, nil, err
			}
			if err := registry.Resolve(callID); err != nil {
				return nil, tokenUsage{}, nil, err
			}
			content = append(content, map[string]any{"type": "tool_result", "tool_use_id": callID, "content": output})
		case "reasoning":
			if !capability.Reasoning {
				return nil, tokenUsage{}, nil, fmt.Errorf("output.reasoning")
			}
			ignored = append(ignored, "reasoning_output")
		default:
			return nil, tokenUsage{}, nil, fmt.Errorf("output.%s", typ)
		}
	}
	model := in.Model
	if model == "" {
		model = fallbackModel
	}
	usage := tokenUsage{PromptTokens: in.Usage.Input, CompletionTokens: in.Usage.Output, TotalTokens: in.Usage.Input + in.Usage.Output}
	if len(content) == 0 {
		content = []any{map[string]any{"type": "text", "text": ""}}
	}
	stopReason, err := responsesTerminationToAnthropic(in.Status, in.IncompleteDetails.Reason, toolStop)
	if err != nil {
		return nil, tokenUsage{}, nil, err
	}
	out := map[string]any{"id": in.ID, "type": "message", "role": "assistant", "model": model, "content": content, "stop_reason": stopReason, "stop_sequence": nil, "usage": map[string]any{"input_tokens": usage.PromptTokens, "output_tokens": usage.CompletionTokens}}
	if out["id"] == "" {
		out["id"] = "msg-responses"
	}
	encoded, err := json.Marshal(out)
	return encoded, usage, uniqueSortedFeatures(ignored), err
}

// buildAnthropicFromResponses keeps the default direct-call contract
// fail-closed. Handler routing passes an explicit capability to the internal
// builder when a model has opted into the bounded reasoning adapter.
func buildAnthropicFromResponses(body map[string]any, model string, stream bool) ([]byte, error) {
	encoded, _, err := buildAnthropicFromResponsesWithCapability(body, model, stream, config.ConversionCapability{})
	return encoded, err
}

// buildAnthropicFromResponsesWithCapability implements the Responses request
// subset that is safe to project to Messages. Reasoning is accepted only when
// a configured adapter maps its request control to Anthropic adaptive
// thinking; source effort is intentionally not translated.
func buildAnthropicFromResponsesWithCapability(body map[string]any, model string, stream bool, capability config.ConversionCapability) ([]byte, []string, error) {
	if err := rejectConversionFields(body, map[string]struct{}{
		"model": {}, "input": {}, "instructions": {}, "max_output_tokens": {}, "stream": {},
		"tools": {}, "tool_choice": {}, "temperature": {}, "top_p": {}, "text": {}, "reasoning": {},
	}); err != nil {
		return nil, nil, err
	}
	if text, ok := body["text"]; ok && text != nil {
		obj, ok := text.(map[string]any)
		if !ok || len(obj) > 0 {
			if ok && obj["format"] != nil {
				return nil, nil, fmt.Errorf("text.format")
			}
			return nil, nil, fmt.Errorf("text")
		}
	}
	maxTokens := 4096
	if raw, exists := body["max_output_tokens"]; exists {
		n, ok := numberAsInt(raw)
		if !ok || n <= 0 {
			return nil, nil, fmt.Errorf("max_output_tokens")
		}
		maxTokens = n
	}
	messages, system, err := responsesInputMessages(body["input"])
	if err != nil {
		return nil, nil, err
	}
	if raw, exists := body["instructions"]; exists {
		instructions, ok := raw.(string)
		if !ok {
			return nil, nil, fmt.Errorf("instructions")
		}
		if strings.TrimSpace(instructions) != "" {
			if system != "" {
				system += "\n"
			}
			system += instructions
		}
	}
	request := map[string]any{"model": model, "max_tokens": maxTokens, "messages": messages, "stream": stream}
	if system != "" {
		request["system"] = system
	}
	if tools, ok := body["tools"]; ok {
		if stream {
			return nil, nil, fmt.Errorf("streaming tools")
		}
		mapped, err := responsesToolsToAnthropic(tools)
		if err != nil {
			return nil, nil, err
		}
		request["tools"] = mapped
	}
	if choice, ok := body["tool_choice"]; ok {
		if stream {
			return nil, nil, fmt.Errorf("streaming tool_choice")
		}
		mapped, err := responsesToolChoiceToAnthropic(choice)
		if err != nil {
			return nil, nil, err
		}
		request["tool_choice"] = mapped
	}
	for _, key := range []string{"temperature", "top_p"} {
		if value, ok := body[key]; ok {
			request[key] = value
		}
	}
	reasoning, hasReasoning := body["reasoning"]
	ignored, err := applyResponsesReasoningAdapter(request, reasoning, hasReasoning, capability)
	if err != nil {
		return nil, nil, err
	}
	encoded, err := json.Marshal(request)
	return encoded, ignored, err
}

func applyResponsesReasoningAdapter(request map[string]any, raw any, present bool, capability config.ConversionCapability) ([]string, error) {
	if !present {
		return nil, nil
	}
	if !capability.Reasoning || capability.ReasoningAdapter != config.ReasoningAdapterResponsesToAnthropicAdaptive || capability.ReasoningTargetEffort == "" {
		return nil, fmt.Errorf("reasoning")
	}
	reasoning, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("reasoning")
	}
	if err := rejectConversionFields(reasoning, map[string]struct{}{"effort": {}}); err != nil {
		return nil, fmt.Errorf("reasoning.%w", err)
	}
	effortValue, exists := reasoning["effort"]
	if exists {
		value, ok := effortValue.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("reasoning.effort")
		}
		if strings.EqualFold(strings.TrimSpace(value), "none") {
			// An explicit Responses effort=none is the only cross-protocol
			// signal that disables Anthropic thinking. In particular, do not
			// emit output_config: DeepSeek rejects named tool_choice while
			// thinking is enabled.
			request["thinking"] = map[string]any{"type": "disabled"}
			delete(request, "output_config")
			return []string{"reasoning"}, nil
		}
	}
	request["thinking"] = map[string]any{"type": "adaptive"}
	request["output_config"] = map[string]any{"effort": capability.ReasoningTargetEffort}
	return []string{"reasoning"}, nil
}

func responsesInputMessages(input any) ([]map[string]any, string, error) {
	if text, ok := input.(string); ok && strings.TrimSpace(text) != "" {
		return []map[string]any{{"role": "user", "content": text}}, "", nil
	}
	items, ok := input.([]any)
	if !ok || len(items) == 0 {
		return nil, "", fmt.Errorf("input")
	}
	if len(items) > maxConversionContentBlocks {
		return nil, "", fmt.Errorf("input exceeds %d content blocks", maxConversionContentBlocks)
	}
	if err := validateConversionTree(items, "input"); err != nil {
		return nil, "", err
	}
	var system strings.Builder
	messages := make([]map[string]any, 0, len(items))
	registry := newToolCallRegistry()
	for i, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("input[%d]", i)
		}
		typ, _ := item["type"].(string)
		if typ == "function_call" || typ == "function_call_output" {
			block, err := responsesToolItemToAnthropic(item)
			if err != nil {
				return nil, "", err
			}
			if typ == "function_call" {
				if err := registry.Add(stringValue(block["id"])); err != nil {
					return nil, "", err
				}
				messages = append(messages, map[string]any{"role": "assistant", "content": []any{block}})
			} else {
				if err := registry.Resolve(stringValue(block["tool_use_id"])); err != nil {
					return nil, "", err
				}
				messages = append(messages, map[string]any{"role": "user", "content": []any{block}})
			}
			continue
		}
		if typ != "" && typ != "message" {
			return nil, "", fmt.Errorf("input[%d].type", i)
		}
		if err := rejectConversionFields(item, map[string]struct{}{"type": {}, "id": {}, "role": {}, "status": {}, "content": {}}); err != nil {
			return nil, "", fmt.Errorf("input[%d].%w", i, err)
		}
		role, _ := item["role"].(string)
		content, err := responsesMessageContentToAnthropic(item["content"], registry)
		if err != nil || strings.TrimSpace(content) == "" {
			if err != nil {
				return nil, "", fmt.Errorf("input[%d].content: %w", i, err)
			}
			return nil, "", fmt.Errorf("input[%d].content", i)
		}
		if role == "system" || role == "developer" {
			if system.Len() > 0 {
				system.WriteString("\n")
			}
			system.WriteString(content)
			continue
		}
		if role != "user" && role != "assistant" {
			return nil, "", fmt.Errorf("input[%d].role", i)
		}
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	if len(messages) == 0 {
		return nil, "", fmt.Errorf("input")
	}
	if err := registry.EnsureResolved(); err != nil {
		return nil, "", err
	}
	return messages, system.String(), nil
}

func stringValue(value any) string {
	s, _ := value.(string)
	return s
}

func responsesMessageContentToAnthropic(raw any, registry *toolCallRegistry) (string, error) {
	if text, ok := raw.(string); ok {
		return text, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return "", fmt.Errorf("content must be text")
	}
	var text strings.Builder
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return "", fmt.Errorf("content block must be object")
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "input_text", "output_text", "text":
			if err := rejectConversionFields(item, map[string]struct{}{"type": {}, "text": {}}); err != nil {
				return "", err
			}
			value, _ := item["text"].(string)
			if _, ok := item["text"].(string); !ok {
				return "", fmt.Errorf("content.text")
			}
			text.WriteString(value)
		case "function_call":
			block, err := responsesToolItemToAnthropic(item)
			if err != nil {
				return "", err
			}
			if err := registry.Add(stringValue(block["id"])); err != nil {
				return "", err
			}
			return "", fmt.Errorf("tool blocks require an assistant message")
		case "function_call_output":
			return "", fmt.Errorf("tool result blocks require a user message")
		default:
			return "", fmt.Errorf("content.%s", typ)
		}
	}
	return text.String(), nil
}

func responsesToolChoiceToAnthropic(raw any) (map[string]any, error) {
	if choice, ok := raw.(string); ok {
		switch choice {
		case "auto":
			return map[string]any{"type": "auto"}, nil
		case "required":
			return map[string]any{"type": "any"}, nil
		case "none":
			return map[string]any{"type": "none"}, nil
		default:
			return nil, fmt.Errorf("tool_choice.%s", choice)
		}
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tool_choice")
	}
	if err := rejectConversionFields(obj, map[string]struct{}{"type": {}, "name": {}, "function": {}}); err != nil {
		return nil, err
	}
	name, _ := obj["name"].(string)
	choiceType, _ := obj["type"].(string)
	if choiceType != "" && choiceType != "function" {
		return nil, fmt.Errorf("tool_choice.type")
	}
	if name == "" {
		if fn, ok := obj["function"].(map[string]any); ok {
			if err := rejectConversionFields(fn, map[string]struct{}{"name": {}}); err != nil {
				return nil, err
			}
			name, _ = fn["name"].(string)
		}
	}
	if name == "" {
		return nil, fmt.Errorf("tool_choice.name")
	}
	return map[string]any{"type": "tool", "name": name}, nil
}

func convertAnthropicToResponses(body []byte, fallbackModel string) ([]byte, tokenUsage, error) {
	encoded, usage, _, err := convertAnthropicToResponsesWithCapability(body, fallbackModel, config.ConversionCapability{})
	return encoded, usage, err
}

func convertAnthropicToResponsesWithCapability(body []byte, fallbackModel string, capability config.ConversionCapability) ([]byte, tokenUsage, []string, error) {
	var in struct {
		ID           string           `json:"id"`
		Model        string           `json:"model"`
		Role         string           `json:"role"`
		StopReason   string           `json:"stop_reason"`
		StopSequence string           `json:"stop_sequence"`
		Content      []map[string]any `json:"content"`
		Usage        struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, tokenUsage{}, nil, err
	}
	var output []any
	var ignored []string
	registry := newToolCallRegistry()
	messageContent := make([]any, 0)
	flushMessage := func() {
		if len(messageContent) == 0 {
			return
		}
		output = append(output, map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": append([]any(nil), messageContent...)})
		messageContent = messageContent[:0]
	}
	for _, block := range in.Content {
		typ, _ := block["type"].(string)
		switch typ {
		case "text":
			if err := rejectConversionFields(block, map[string]struct{}{"type": {}, "text": {}}); err != nil {
				return nil, tokenUsage{}, nil, fmt.Errorf("content.text.%w", err)
			}
			text, ok := block["text"].(string)
			if !ok {
				return nil, tokenUsage{}, nil, fmt.Errorf("content.text.text")
			}
			messageContent = append(messageContent, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
		case "tool_use":
			if err := rejectConversionFields(block, map[string]struct{}{"type": {}, "id": {}, "name": {}, "input": {}}); err != nil {
				return nil, tokenUsage{}, nil, fmt.Errorf("content.tool_use.%w", err)
			}
			flushMessage()
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			input, ok := block["input"]
			if id == "" || name == "" || !ok {
				return nil, tokenUsage{}, nil, fmt.Errorf("content.tool_use")
			}
			if err := registry.Add(id); err != nil {
				return nil, tokenUsage{}, nil, err
			}
			arguments, err := json.Marshal(input)
			if err != nil {
				return nil, tokenUsage{}, nil, err
			}
			if len(arguments) > maxConversionToolArgumentBytes {
				return nil, tokenUsage{}, nil, fmt.Errorf("content.tool_use.input exceeds %d bytes", maxConversionToolArgumentBytes)
			}
			output = append(output, map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": string(arguments)})
		case "tool_result":
			if err := rejectConversionFields(block, map[string]struct{}{"type": {}, "tool_use_id": {}, "content": {}}); err != nil {
				return nil, tokenUsage{}, nil, fmt.Errorf("content.tool_result.%w", err)
			}
			flushMessage()
			id, _ := block["tool_use_id"].(string)
			if err := registry.Resolve(id); err != nil {
				return nil, tokenUsage{}, nil, err
			}
			content, err := stringifyToolOutput(block["content"])
			if err != nil {
				return nil, tokenUsage{}, nil, fmt.Errorf("content.tool_result.content: %w", err)
			}
			output = append(output, map[string]any{"type": "function_call_output", "call_id": id, "output": content})
		case "thinking", "redacted_thinking":
			if !capability.Reasoning {
				return nil, tokenUsage{}, nil, fmt.Errorf("content.%s", typ)
			}
			ignored = append(ignored, "thinking_output")
		default:
			return nil, tokenUsage{}, nil, fmt.Errorf("content.%s", typ)
		}
	}
	flushMessage()
	if len(output) == 0 {
		output = append(output, map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}})
	}
	model := in.Model
	if model == "" {
		model = fallbackModel
	}
	usage := tokenUsage{PromptTokens: in.Usage.Input, CompletionTokens: in.Usage.Output, TotalTokens: in.Usage.Input + in.Usage.Output}
	status, incompleteDetails, err := anthropicTerminationToResponses(in.StopReason)
	if err != nil {
		return nil, tokenUsage{}, nil, err
	}
	out := map[string]any{"id": in.ID, "object": "response", "model": model, "status": status, "output": output}
	if incompleteDetails != nil {
		out["incomplete_details"] = incompleteDetails
	}
	if out["id"] == "" {
		out["id"] = "resp-anthropic"
	}
	out["usage"] = map[string]any{"input_tokens": usage.PromptTokens, "output_tokens": usage.CompletionTokens, "total_tokens": usage.TotalTokens}
	encoded, err := json.Marshal(out)
	return encoded, usage, uniqueSortedFeatures(ignored), err
}

func buildResponsesFromAnthropic(body map[string]any, model string, stream bool) ([]byte, error) {
	encoded, _, err := buildResponsesFromAnthropicWithCapability(body, model, stream, config.ConversionCapability{})
	return encoded, err
}

func buildResponsesFromAnthropicWithCapability(body map[string]any, model string, stream bool, capability config.ConversionCapability) ([]byte, []string, error) {
	if err := rejectConversionFields(body, map[string]struct{}{
		"model": {}, "messages": {}, "max_tokens": {}, "system": {}, "stream": {},
		"temperature": {}, "top_p": {}, "tools": {}, "tool_choice": {}, "thinking": {}, "output_config": {},
	}); err != nil {
		return nil, nil, err
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return nil, nil, fmt.Errorf("messages")
	}
	if len(msgs) > maxConversionContentBlocks {
		return nil, nil, fmt.Errorf("messages exceeds %d content blocks", maxConversionContentBlocks)
	}
	if err := validateConversionTree(msgs, "messages"); err != nil {
		return nil, nil, err
	}
	input := make([]map[string]any, 0, len(msgs))
	registry := newToolCallRegistry()
	instructions, err := anthropicSystemText(body["system"])
	if err != nil {
		return nil, nil, err
	}
	for i, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("messages[%d]", i)
		}
		if err := rejectConversionFields(m, map[string]struct{}{"role": {}, "content": {}}); err != nil {
			return nil, nil, fmt.Errorf("messages[%d].%w", i, err)
		}
		role, _ := m["role"].(string)
		contentRaw := m["content"]
		content, blocks, err := anthropicMessageContentToResponses(contentRaw, role, registry)
		if err != nil {
			return nil, nil, fmt.Errorf("messages[%d].content: %w", i, err)
		}
		if role == "system" {
			instructions += content
			continue
		}
		if role != "user" && role != "assistant" {
			return nil, nil, fmt.Errorf("messages[%d].role", i)
		}
		if content != "" {
			input = append(input, map[string]any{"type": "message", "role": role, "content": content})
		}
		input = append(input, blocks...)
	}
	if err := registry.EnsureResolved(); err != nil {
		return nil, nil, err
	}
	maxTokens := 4096
	if raw, exists := body["max_tokens"]; exists {
		n, ok := numberAsInt(raw)
		if !ok || n <= 0 {
			return nil, nil, fmt.Errorf("max_tokens")
		}
		maxTokens = n
	}
	out := map[string]any{"model": model, "input": input, "max_output_tokens": maxTokens, "stream": stream}
	if instructions != "" {
		out["instructions"] = instructions
	}
	if rawTools, ok := body["tools"]; ok {
		mapped, err := anthropicToolsToResponses(rawTools)
		if err != nil {
			return nil, nil, err
		}
		out["tools"] = mapped
	}
	if choice, ok := body["tool_choice"]; ok {
		mapped, err := anthropicToolChoiceToResponses(choice)
		if err != nil {
			return nil, nil, err
		}
		out["tool_choice"] = mapped
	}
	for _, key := range []string{"temperature", "top_p"} {
		if value, ok := body[key]; ok {
			out[key] = value
		}
	}
	thinking, hasThinking := body["thinking"]
	outputConfig, hasOutputConfig := body["output_config"]
	ignored, err := applyAnthropicThinkingAdapter(out, thinking, hasThinking, outputConfig, hasOutputConfig, capability)
	if err != nil {
		return nil, nil, err
	}
	encoded, err := json.Marshal(out)
	return encoded, ignored, err
}

// disableResponsesReasoningForOmittedAnthropicThinking preserves the source
// protocol's opt-in thinking semantics when a Responses model defaults to
// reasoning. It is applied only when the exact model explicitly supports none.
func disableResponsesReasoningForOmittedAnthropicThinking(source map[string]any, encoded []byte, metadata config.ModelMetadata) ([]byte, error) {
	if _, present := source["thinking"]; present || !metadata.ReasoningDeclared || !metadata.ReasoningSupported {
		return encoded, nil
	}
	supportsNone := false
	for _, effort := range metadata.ReasoningEfforts {
		if strings.EqualFold(strings.TrimSpace(effort), "none") {
			supportsNone = true
			break
		}
	}
	if !supportsNone {
		return encoded, nil
	}
	var target map[string]any
	if err := json.Unmarshal(encoded, &target); err != nil {
		return nil, err
	}
	if _, present := target["reasoning"]; present {
		return encoded, nil
	}
	target["reasoning"] = map[string]any{"effort": "none"}
	return json.Marshal(target)
}

func applyAnthropicThinkingAdapter(request map[string]any, rawThinking any, thinkingPresent bool, rawOutputConfig any, outputConfigPresent bool, capability config.ConversionCapability) ([]string, error) {
	if !thinkingPresent {
		if outputConfigPresent {
			return nil, fmt.Errorf("output_config")
		}
		return nil, nil
	}
	if !capability.Reasoning || capability.ReasoningAdapter != config.ReasoningAdapterAnthropicToResponsesEffort || capability.ReasoningTargetEffort == "" {
		return nil, fmt.Errorf("thinking")
	}
	thinking, ok := rawThinking.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("thinking")
	}
	if err := rejectConversionFields(thinking, map[string]struct{}{"type": {}}); err != nil {
		return nil, fmt.Errorf("thinking.%w", err)
	}
	if thinkingType, _ := thinking["type"].(string); thinkingType != "adaptive" {
		return nil, fmt.Errorf("thinking.type")
	}
	if rawOutputConfig != nil {
		outputConfig, ok := rawOutputConfig.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("output_config")
		}
		if err := rejectConversionFields(outputConfig, map[string]struct{}{"effort": {}}); err != nil {
			return nil, fmt.Errorf("output_config.%w", err)
		}
		if effort, exists := outputConfig["effort"]; exists {
			if value, ok := effort.(string); !ok || strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("output_config.effort")
			}
		}
	}
	request["reasoning"] = map[string]any{"effort": capability.ReasoningTargetEffort}
	return []string{"thinking"}, nil
}

func anthropicMessageContentToResponses(raw any, role string, registry *toolCallRegistry) (string, []map[string]any, error) {
	if text, ok := raw.(string); ok {
		return text, nil, nil
	}
	blocks, ok := raw.([]any)
	if !ok {
		return "", nil, fmt.Errorf("content must be text or blocks")
	}
	var text strings.Builder
	items := make([]map[string]any, 0)
	for _, rawBlock := range blocks {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("content block must be object")
		}
		typ, _ := block["type"].(string)
		switch typ {
		case "text":
			if err := rejectConversionFields(block, map[string]struct{}{"type": {}, "text": {}}); err != nil {
				return "", nil, err
			}
			value, ok := block["text"].(string)
			if !ok {
				return "", nil, fmt.Errorf("content.text")
			}
			text.WriteString(value)
		case "tool_use":
			if role != "assistant" {
				return "", nil, fmt.Errorf("tool_use requires assistant role")
			}
			mapped, err := anthropicToolBlockToResponses(block)
			if err != nil {
				return "", nil, err
			}
			if err := registry.Add(stringValue(mapped["call_id"])); err != nil {
				return "", nil, err
			}
			items = append(items, mapped)
		case "tool_result":
			if role != "user" {
				return "", nil, fmt.Errorf("tool_result requires user role")
			}
			mapped, err := anthropicToolBlockToResponses(block)
			if err != nil {
				return "", nil, err
			}
			if err := registry.Resolve(stringValue(mapped["call_id"])); err != nil {
				return "", nil, err
			}
			items = append(items, mapped)
		default:
			return "", nil, fmt.Errorf("content.%s", typ)
		}
	}
	return text.String(), items, nil
}

func anthropicToolChoiceToResponses(raw any) (any, error) {
	if choice, ok := raw.(string); ok {
		switch choice {
		case "auto":
			return "auto", nil
		case "any":
			return "required", nil
		case "none":
			return "none", nil
		default:
			return nil, fmt.Errorf("tool_choice.%s", choice)
		}
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tool_choice")
	}
	if err := rejectConversionFields(obj, map[string]struct{}{"type": {}, "name": {}}); err != nil {
		return nil, err
	}
	name, _ := obj["name"].(string)
	choiceType, _ := obj["type"].(string)
	switch choiceType {
	case "auto":
		if name != "" {
			return nil, fmt.Errorf("tool_choice.name")
		}
		return "auto", nil
	case "any":
		if name != "" {
			return nil, fmt.Errorf("tool_choice.name")
		}
		return "required", nil
	case "none":
		if name != "" {
			return nil, fmt.Errorf("tool_choice.name")
		}
		return "none", nil
	case "tool", "":
	default:
		return nil, fmt.Errorf("tool_choice.type")
	}
	if name == "" {
		return nil, fmt.Errorf("tool_choice.name")
	}
	return map[string]any{"type": "function", "name": name}, nil
}

func anthropicSystemText(raw any) (string, error) {
	if text, ok := raw.(string); ok {
		return strings.TrimSpace(text), nil
	}
	parts, ok := raw.([]any)
	if !ok {
		if raw == nil {
			return "", nil
		}
		return "", fmt.Errorf("system")
	}
	if len(parts) > maxConversionContentBlocks {
		return "", fmt.Errorf("system exceeds %d content blocks", maxConversionContentBlocks)
	}
	var out strings.Builder
	for _, item := range parts {
		block, ok := item.(map[string]any)
		if !ok || block["type"] != "text" {
			return "", fmt.Errorf("system.content")
		}
		if err := rejectConversionFields(block, map[string]struct{}{"type": {}, "text": {}}); err != nil {
			return "", fmt.Errorf("system.content.%w", err)
		}
		text, ok := block["text"].(string)
		if !ok {
			return "", fmt.Errorf("system.content.text")
		}
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(text)
	}
	return strings.TrimSpace(out.String()), nil
}

func responsesToolsToAnthropic(raw any) ([]map[string]any, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("tools")
	}
	if len(items) > maxConversionTools {
		return nil, fmt.Errorf("tools exceeds %d definitions", maxConversionTools)
	}
	out := make([]map[string]any, 0, len(items))
	for i, item := range items {
		tool, ok := item.(map[string]any)
		if !ok || tool["type"] != "function" {
			return nil, fmt.Errorf("tools[%d].type", i)
		}
		if err := rejectConversionFields(tool, map[string]struct{}{"type": {}, "name": {}, "description": {}, "parameters": {}}); err != nil {
			return nil, fmt.Errorf("tools[%d].%w", i, err)
		}
		name, _ := tool["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("tools[%d].name", i)
		}
		mapped := map[string]any{"name": name}
		if description, exists := tool["description"]; exists {
			value, ok := description.(string)
			if !ok {
				return nil, fmt.Errorf("tools[%d].description", i)
			}
			mapped["description"] = value
		}
		if rawParameters, exists := tool["parameters"]; exists {
			parameters, ok := rawParameters.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("tools[%d].parameters", i)
			}
			if err := validateConversionToolSchema(parameters); err != nil {
				return nil, fmt.Errorf("tools[%d].parameters: %w", i, err)
			}
			mapped["input_schema"] = parameters
		} else {
			mapped["input_schema"] = map[string]any{"type": "object"}
		}
		out = append(out, mapped)
	}
	return out, nil
}

func anthropicToolsToResponses(raw any) ([]map[string]any, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("tools")
	}
	if len(items) > maxConversionTools {
		return nil, fmt.Errorf("tools exceeds %d definitions", maxConversionTools)
	}
	out := make([]map[string]any, 0, len(items))
	for i, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tools[%d]", i)
		}
		if err := rejectConversionFields(tool, map[string]struct{}{"name": {}, "description": {}, "input_schema": {}}); err != nil {
			return nil, fmt.Errorf("tools[%d].%w", i, err)
		}
		name, _ := tool["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("tools[%d].name", i)
		}
		mapped := map[string]any{"type": "function", "name": name}
		if description, exists := tool["description"]; exists {
			value, ok := description.(string)
			if !ok {
				return nil, fmt.Errorf("tools[%d].description", i)
			}
			mapped["description"] = value
		}
		if rawSchema, exists := tool["input_schema"]; exists {
			schema, ok := rawSchema.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("tools[%d].input_schema", i)
			}
			if err := validateConversionToolSchema(schema); err != nil {
				return nil, fmt.Errorf("tools[%d].input_schema: %w", i, err)
			}
			mapped["parameters"] = schema
		} else {
			mapped["parameters"] = map[string]any{"type": "object"}
		}
		out = append(out, mapped)
	}
	return out, nil
}

func responsesToolItemToAnthropic(item map[string]any) (map[string]any, error) {
	typ, _ := item["type"].(string)
	switch typ {
	case "function_call":
		if err := rejectConversionFields(item, map[string]struct{}{"type": {}, "call_id": {}, "name": {}, "arguments": {}}); err != nil {
			return nil, err
		}
		callID, _ := item["call_id"].(string)
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		if callID == "" || name == "" || len(args) > maxConversionToolArgumentBytes || !json.Valid([]byte(args)) {
			return nil, fmt.Errorf("function_call.call_id/name")
		}
		return map[string]any{"type": "tool_use", "id": callID, "name": name, "input": json.RawMessage(args)}, nil
	case "function_call_output":
		if err := rejectConversionFields(item, map[string]struct{}{"type": {}, "call_id": {}, "output": {}}); err != nil {
			return nil, err
		}
		callID, _ := item["call_id"].(string)
		output, err := stringifyToolOutput(item["output"])
		if callID == "" {
			return nil, fmt.Errorf("function_call_output.call_id")
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "tool_result", "tool_use_id": callID, "content": output}, nil
	default:
		return nil, fmt.Errorf("unsupported tool item %q", typ)
	}
}

func stringifyToolOutput(raw any) (string, error) {
	if text, ok := raw.(string); ok {
		if len(text) > maxConversionToolArgumentBytes {
			return "", fmt.Errorf("tool output exceeds %d bytes", maxConversionToolArgumentBytes)
		}
		return text, nil
	}
	if raw == nil {
		return "", fmt.Errorf("tool output is required")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return "", err
	}
	if len(encoded) > maxConversionToolArgumentBytes {
		return "", fmt.Errorf("tool output exceeds %d bytes", maxConversionToolArgumentBytes)
	}
	return string(encoded), nil
}

func anthropicToolBlockToResponses(block map[string]any) (map[string]any, error) {
	typ, _ := block["type"].(string)
	switch typ {
	case "tool_use":
		if err := rejectConversionFields(block, map[string]struct{}{"type": {}, "id": {}, "name": {}, "input": {}}); err != nil {
			return nil, err
		}
		id, _ := block["id"].(string)
		name, _ := block["name"].(string)
		input, ok := block["input"]
		if id == "" || name == "" || !ok {
			return nil, fmt.Errorf("tool_use.id/name/input")
		}
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		if len(encoded) > maxConversionToolArgumentBytes {
			return nil, fmt.Errorf("tool input exceeds %d bytes", maxConversionToolArgumentBytes)
		}
		return map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": string(encoded)}, nil
	case "tool_result":
		if err := rejectConversionFields(block, map[string]struct{}{"type": {}, "tool_use_id": {}, "content": {}}); err != nil {
			return nil, err
		}
		id, _ := block["tool_use_id"].(string)
		content, _ := block["content"].(string)
		if content == "" {
			if encoded, err := json.Marshal(block["content"]); err == nil && string(encoded) != "null" {
				content = string(encoded)
			}
		}
		if id == "" {
			return nil, fmt.Errorf("tool_result.tool_use_id")
		}
		if len(content) > maxConversionToolArgumentBytes {
			return nil, fmt.Errorf("tool result exceeds %d bytes", maxConversionToolArgumentBytes)
		}
		return map[string]any{"type": "function_call_output", "call_id": id, "output": content}, nil
	default:
		return nil, fmt.Errorf("unsupported Anthropic tool block %q", typ)
	}
}

func validateConversionToolSchema(schema map[string]any) error {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	if len(encoded) > maxConversionToolSchemaBytes {
		return fmt.Errorf("schema exceeds %d bytes", maxConversionToolSchemaBytes)
	}
	var walk func(any, int) error
	walk = func(value any, depth int) error {
		if depth > maxConversionSchemaDepth {
			return fmt.Errorf("schema exceeds depth %d", maxConversionSchemaDepth)
		}
		switch v := value.(type) {
		case map[string]any:
			for _, child := range v {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range v {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(schema, 0)
}

func validateConversionTree(value any, label string) error {
	blocks := 0
	var walk func(any, int) error
	walk = func(node any, depth int) error {
		if depth > maxConversionSchemaDepth {
			return fmt.Errorf("%s exceeds depth %d", label, maxConversionSchemaDepth)
		}
		switch v := node.(type) {
		case map[string]any:
			blocks++
			if blocks > maxConversionContentBlocks {
				return fmt.Errorf("%s exceeds %d content blocks", label, maxConversionContentBlocks)
			}
			for _, child := range v {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		case []any:
			if len(v) > maxConversionContentBlocks {
				return fmt.Errorf("%s exceeds %d content blocks", label, maxConversionContentBlocks)
			}
			for _, child := range v {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(value, 0)
}

// rejectConversionFields keeps protocol conversion fail-closed. Any API
// field that is not explicitly modeled by the adapter is rejected instead of
// being silently dropped and changing request semantics.
func rejectConversionFields(body map[string]any, allowed map[string]struct{}) error {
	for key := range body {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("%s", key)
		}
	}
	return nil
}

type textConversionStreamState struct {
	ID, Model                                      string
	InputTokens, OutputTokens                      int
	StopReason, StopSequence                       string
	Started, TextStarted, TextCompleted, Completed bool
	OmittedContentBlocks                           map[int]struct{}
	IgnoredFeatures                                []string
	HeadersWritten                                 bool
	BytesWritten                                   int64
	TextBlocks, TextOutputIndex                    int
	Output                                         bytes.Buffer
}

func (s *textConversionStreamState) markIgnored(feature string) {
	if s == nil || feature == "" {
		return
	}
	s.IgnoredFeatures = uniqueSortedFeatures(append(s.IgnoredFeatures, feature))
}

// responsesTerminationToAnthropic keeps the conversion fail-closed when a
// Responses response did not complete normally. Anthropic has no generic
// equivalent for failed/cancelled responses, so those states must not be
// rewritten as a successful end_turn.
func responsesTerminationToAnthropic(status, incompleteReason string, toolStop bool) (string, error) {
	status = strings.TrimSpace(status)
	incompleteReason = strings.TrimSpace(incompleteReason)
	if status == "" || status == "completed" {
		if toolStop {
			return "tool_use", nil
		}
		return "end_turn", nil
	}
	if status != "incomplete" {
		return "", fmt.Errorf("response.status.%s", status)
	}
	switch incompleteReason {
	case "max_output_tokens":
		return "max_tokens", nil
	case "stop_sequence":
		return "stop_sequence", nil
	default:
		return "", fmt.Errorf("response.incomplete_details.reason.%s", incompleteReason)
	}
}

// anthropicTerminationToResponses maps Anthropic stop reasons to the bounded
// Responses status vocabulary. Reasons without a safe equivalent are kept as
// an incomplete detail rather than being advertised as completed.
func anthropicTerminationToResponses(reason string) (string, map[string]any, error) {
	reason = strings.TrimSpace(reason)
	switch reason {
	case "", "end_turn", "tool_use", "stop_sequence":
		return "completed", nil, nil
	case "max_tokens":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}, nil
	case "pause_turn", "refusal", "model_context_window_exceeded":
		return "incomplete", map[string]any{"reason": reason}, nil
	default:
		return "", nil, fmt.Errorf("message.stop_reason.%s", reason)
	}
}

func anthropicEventToResponses(payload []byte, state *textConversionStreamState) ([]map[string]any, error) {
	return anthropicEventToResponsesWithCapability(config.ConversionCapability{})(payload, state)
}

func anthropicEventToResponsesWithCapability(capability config.ConversionCapability) conversionSSEMapper {
	return func(payload []byte, state *textConversionStreamState) ([]map[string]any, error) {
		return anthropicEventToResponsesWithCapabilityState(payload, state, capability)
	}
}

func anthropicEventToResponsesWithCapabilityState(payload []byte, state *textConversionStreamState, capability config.ConversionCapability) ([]map[string]any, error) {
	if state == nil {
		return nil, fmt.Errorf("stream state is required")
	}
	if state.Completed {
		return nil, fmt.Errorf("stream event received after completion")
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, err
	}
	typ, _ := event["type"].(string)
	out := []map[string]any{}
	switch typ {
	case "message_start":
		if state.Started {
			return nil, fmt.Errorf("anthropic stream message start is duplicated")
		}
		message, _ := event["message"].(map[string]any)
		state.ID, _ = message["id"].(string)
		state.Model, _ = message["model"].(string)
		state.Started = true
		if usage, ok := message["usage"].(map[string]any); ok {
			state.InputTokens = intNumber(usage["input_tokens"])
		}
		out = append(out, map[string]any{"type": "response.created", "response": map[string]any{"id": state.ID, "object": "response", "model": state.Model, "status": "in_progress", "output": []any{}}})
	case "content_block_start":
		if !state.Started {
			return nil, fmt.Errorf("anthropic stream content block start is out of order")
		}
		block, _ := event["content_block"].(map[string]any)
		if block == nil {
			return nil, fmt.Errorf("content_block is required")
		}
		index, err := conversionEventIndex(event)
		if err != nil {
			return nil, err
		}
		blockType, _ := block["type"].(string)
		if isAnthropicThinkingBlock(blockType) {
			if !capability.Reasoning {
				return nil, fmt.Errorf("content_block.%s", blockType)
			}
			if state.OmittedContentBlocks == nil {
				state.OmittedContentBlocks = map[int]struct{}{}
			}
			if _, exists := state.OmittedContentBlocks[index]; exists {
				return nil, fmt.Errorf("anthropic stream thinking block start is duplicated")
			}
			state.OmittedContentBlocks[index] = struct{}{}
			state.markIgnored("thinking_output")
			break
		}
		if state.TextStarted || blockType != "text" {
			return nil, fmt.Errorf("content_block.%v", block["type"])
		}
		if _, ok := block["text"].(string); !ok {
			return nil, fmt.Errorf("content_block.text")
		}
		state.TextStarted = true
		state.TextCompleted = false
		state.TextOutputIndex = state.TextBlocks
		state.TextBlocks++
		itemID := fmt.Sprintf("%s-message-%d", state.ID, state.TextOutputIndex)
		out = append(out, map[string]any{"type": "response.output_item.added", "output_index": state.TextOutputIndex, "item": map[string]any{"id": itemID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
	case "content_block_delta":
		index, err := conversionEventIndex(event)
		if err != nil {
			return nil, err
		}
		if _, omitted := state.OmittedContentBlocks[index]; omitted {
			delta, _ := event["delta"].(map[string]any)
			deltaType, _ := delta["type"].(string)
			if deltaType != "thinking_delta" && deltaType != "signature_delta" {
				return nil, fmt.Errorf("delta.%v", delta["type"])
			}
			break
		}
		if !state.TextStarted || state.TextCompleted {
			return nil, fmt.Errorf("anthropic stream content delta is out of order")
		}
		delta, _ := event["delta"].(map[string]any)
		if delta["type"] != "text_delta" {
			return nil, fmt.Errorf("delta.%v", delta["type"])
		}
		text, ok := delta["text"].(string)
		if !ok {
			return nil, fmt.Errorf("delta.text")
		}
		itemID := fmt.Sprintf("%s-message-%d", state.ID, state.TextOutputIndex)
		out = append(out, map[string]any{"type": "response.output_text.delta", "item_id": itemID, "output_index": state.TextOutputIndex, "content_index": 0, "delta": text})
	case "content_block_stop":
		index, err := conversionEventIndex(event)
		if err != nil {
			return nil, err
		}
		if _, omitted := state.OmittedContentBlocks[index]; omitted {
			delete(state.OmittedContentBlocks, index)
			break
		}
		if !state.TextStarted || state.TextCompleted {
			return nil, fmt.Errorf("anthropic stream content block stop is out of order")
		}
		state.TextCompleted = true
		state.TextStarted = false
		itemID := fmt.Sprintf("%s-message-%d", state.ID, state.TextOutputIndex)
		out = append(out, map[string]any{"type": "response.output_text.done", "item_id": itemID, "output_index": state.TextOutputIndex, "content_index": 0}, map[string]any{"type": "response.output_item.done", "output_index": state.TextOutputIndex, "item": map[string]any{"id": itemID, "type": "message", "role": "assistant", "status": "completed"}})
	case "message_delta":
		if delta, ok := event["delta"].(map[string]any); ok {
			if reason, ok := delta["stop_reason"].(string); ok {
				state.StopReason = reason
			}
			if sequence, ok := delta["stop_sequence"].(string); ok {
				state.StopSequence = sequence
			}
		}
		if usage, ok := event["usage"].(map[string]any); ok {
			state.OutputTokens = intNumber(usage["output_tokens"])
		}
	case "message_stop":
		if !state.Started {
			return nil, fmt.Errorf("anthropic stream message stop before start")
		}
		if state.TextStarted || len(state.OmittedContentBlocks) > 0 {
			return nil, fmt.Errorf("anthropic stream message stop with unclosed content block")
		}
		status, incompleteDetails, err := anthropicTerminationToResponses(state.StopReason)
		if err != nil {
			return nil, err
		}
		state.Completed = true
		response := map[string]any{"id": state.ID, "object": "response", "model": state.Model, "status": status, "usage": map[string]any{"input_tokens": state.InputTokens, "output_tokens": state.OutputTokens, "total_tokens": state.InputTokens + state.OutputTokens}}
		if incompleteDetails != nil {
			response["incomplete_details"] = incompleteDetails
		}
		terminalType := "response.completed"
		if status == "incomplete" {
			terminalType = "response.incomplete"
		}
		out = append(out, map[string]any{"type": terminalType, "response": response})
	case "ping":
	default:
		return nil, fmt.Errorf("anthropic stream event %q", typ)
	}
	return out, nil
}

func conversionEventIndex(event map[string]any) (int, error) {
	raw, exists := event["index"]
	if !exists {
		return 0, nil
	}
	index, ok := raw.(float64)
	if !ok || index < 0 || index != float64(int(index)) {
		return 0, fmt.Errorf("stream content block index")
	}
	return int(index), nil
}

func isAnthropicThinkingBlock(blockType string) bool {
	return blockType == "thinking" || blockType == "redacted_thinking"
}

func responsesEventToAnthropic(payload []byte, state *textConversionStreamState) ([]map[string]any, error) {
	return responsesEventToAnthropicWithCapability(config.ConversionCapability{})(payload, state)
}

func responsesEventToAnthropicWithCapability(capability config.ConversionCapability) conversionSSEMapper {
	return func(payload []byte, state *textConversionStreamState) ([]map[string]any, error) {
		return responsesEventToAnthropicWithCapabilityState(payload, state, capability)
	}
}

func responsesEventToAnthropicWithCapabilityState(payload []byte, state *textConversionStreamState, capability config.ConversionCapability) ([]map[string]any, error) {
	if state == nil {
		return nil, fmt.Errorf("stream state is required")
	}
	if state.Completed {
		return nil, fmt.Errorf("stream event received after completion")
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, err
	}
	typ, _ := event["type"].(string)
	out := []map[string]any{}
	switch typ {
	case "response.created", "response.in_progress":
		if state.Started {
			return out, nil
		}
		response, _ := event["response"].(map[string]any)
		state.ID, _ = response["id"].(string)
		state.Model, _ = response["model"].(string)
		state.Started = true
		out = append(out, map[string]any{"type": "message_start", "message": map[string]any{"id": state.ID, "type": "message", "role": "assistant", "model": state.Model, "content": []any{}, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}}, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	case "response.output_text.delta":
		if !state.Started || state.Completed {
			return nil, fmt.Errorf("responses stream text delta is out of order")
		}
		text, ok := event["delta"].(string)
		if !ok {
			return nil, fmt.Errorf("response.output_text.delta")
		}
		out = append(out, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
	case "response.reasoning.delta", "response.reasoning.done", "response.reasoning_text.delta", "response.reasoning_text.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		if !capability.Reasoning {
			return nil, fmt.Errorf("responses stream event %q", typ)
		}
		state.markIgnored("reasoning_output")
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		itemType, _ := item["type"].(string)
		if itemType == "reasoning" {
			if !capability.Reasoning {
				return nil, fmt.Errorf("responses stream item %q", itemType)
			}
			state.markIgnored("reasoning_output")
			break
		}
		if itemType != "" && itemType != "message" {
			return nil, fmt.Errorf("responses stream item %q", itemType)
		}
	case "response.content_part.added", "response.content_part.done":
		part, _ := event["part"].(map[string]any)
		partType, _ := part["type"].(string)
		if partType == "reasoning_text" || partType == "reasoning_summary" {
			if !capability.Reasoning {
				return nil, fmt.Errorf("responses stream content part %q", partType)
			}
			state.markIgnored("reasoning_output")
			break
		}
		if partType != "" && partType != "output_text" {
			return nil, fmt.Errorf("responses stream content part %q", partType)
		}
	case "response.output_text.done":
	case "response.output_item.done":
		item, _ := event["item"].(map[string]any)
		if itemType, _ := item["type"].(string); itemType == "reasoning" {
			if !capability.Reasoning {
				return nil, fmt.Errorf("responses stream item %q", itemType)
			}
			state.markIgnored("reasoning_output")
		}
	case "response.completed", "response.incomplete":
		if !state.Started {
			return nil, fmt.Errorf("responses stream completed before created")
		}
		response, _ := event["response"].(map[string]any)
		status, _ := response["status"].(string)
		incompleteDetails, _ := response["incomplete_details"].(map[string]any)
		incompleteReason, _ := incompleteDetails["reason"].(string)
		if typ == "response.incomplete" {
			status = "incomplete"
		}
		if _, err := responsesTerminationToAnthropic(status, incompleteReason, false); err != nil {
			return nil, err
		}
		if usage, ok := response["usage"].(map[string]any); ok {
			state.InputTokens = intNumber(usage["input_tokens"])
			state.OutputTokens = intNumber(usage["output_tokens"])
		}
		state.Completed = true
		stopReason := "end_turn"
		if status == "incomplete" {
			stopReason, _ = responsesTerminationToAnthropic(status, incompleteReason, false)
		}
		out = append(out, map[string]any{"type": "content_block_stop", "index": 0}, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": state.OutputTokens}}, map[string]any{"type": "message_stop"})
	default:
		return nil, fmt.Errorf("responses stream event %q", typ)
	}
	return out, nil
}

func intNumber(value any) int {
	if n, ok := value.(float64); ok {
		return int(n)
	}
	return 0
}
