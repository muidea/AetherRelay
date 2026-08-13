package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
)

func (h *Handler) handleAnthropicToCodex(w http.ResponseWriter, r *http.Request, started time.Time, plan TransportPlan, model string, stream bool, body map[string]any) {
	round := archiveRoundFromContext(r.Context())
	if h.codexResponses == nil {
		h.writeCodexResponsesError(w, r, round, started, plan.RouteOwner, model, stream, codexresponses.NewFailure(codexresponses.KindProviderUnavailable, 0, fmt.Errorf("Codex Responses executor is unavailable")))
		return
	}
	capability := config.ConversionCapability{Level: 2, Text: true, Tools: true, Streaming: true, Continuation: true}
	responsesBody, degraded, err := buildResponsesFromAnthropicWithCapability(body, model, stream, capability)
	if err != nil {
		h.writeArchivedAPIError(w, round, r, started, plan.RouteOwner, model, stream, http.StatusBadRequest, conversionAPIError(plan, err))
		return
	}
	normalized, normalizedBody, ignored, err := normalizeCodexRequest(responsesBody, false)
	if err != nil {
		h.writeArchivedError(w, round, r, started, plan.RouteOwner, model, stream, http.StatusBadRequest, err.Error())
		return
	}
	markConversionDegraded(round, append(degraded, ignored...))
	sessionHash := codexSessionHash(r, model, normalizedBody)
	normalized, _, err = ensureCodexPromptCacheKey(normalized, normalizedBody, sessionHash)
	if err != nil {
		h.writeArchivedError(w, round, r, started, plan.RouteOwner, model, stream, http.StatusInternalServerError, err.Error())
		return
	}
	h.archiveAndLogTransportPlan(round, r, plan, effectivecatalog.BuiltinProviderViewFor(plan.RouteOwner), stream)
	request := codexresponses.Request{Model: model, Body: normalized, SessionHash: sessionHash}
	if !stream {
		result, execErr := h.codexResponses.CompleteCodexResponses(r.Context(), request)
		if execErr != nil {
			h.writeCodexResponsesError(w, r, round, started, plan.RouteOwner, model, false, execErr)
			return
		}
		converted, usage, degradedResponse, convertErr := convertOpenAIResponsesToAnthropicWithCapability(result.Body, model, capability)
		if convertErr != nil {
			h.writeArchivedError(w, round, r, started, plan.RouteOwner, model, false, http.StatusBadGateway, "upstream_protocol_error: "+convertErr.Error())
			return
		}
		markConversionDegraded(round, degradedResponse)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(converted)
		_ = h.writeArchiveResponse(round, "response.json", converted)
		duration := time.Since(started)
		h.recordAndPrint(round, r, plan.RouteOwner, model, false, http.StatusOK, duration, usage, "")
		h.writeArchiveMetadata(round, plan.RouteOwner, model, false, http.StatusOK, duration, usage, "response.json", "", "", "success")
		return
	}
	h.streamAnthropicToCodex(w, r, started, plan, model, request, capability)
}

func (h *Handler) streamAnthropicToCodex(w http.ResponseWriter, r *http.Request, started time.Time, plan TransportPlan, model string, request codexresponses.Request, capability config.ConversionCapability) {
	round := archiveRoundFromContext(r.Context())
	state := &textConversionStreamState{}
	mapper := responsesEventToAnthropicWithCapability(capability)
	var archive bytes.Buffer
	streamStarted := false
	startStream := func(codexresponses.StreamStart) error { return nil }
	emit := func(line []byte) error {
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" || !strings.HasPrefix(trimmed, "data:") {
			return nil
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		events, err := mapper([]byte(payload), state)
		if err != nil {
			return err
		}
		encoded, err := encodeConversionSSE(events, true)
		if err != nil {
			return err
		}
		if len(encoded) == 0 {
			return nil
		}
		if archive.Len()+len(encoded) > maxConversionSSEBytes {
			return fmt.Errorf("conversion SSE exceeds %d bytes", maxConversionSSEBytes)
		}
		if !streamStarted {
			prepareSSEHeaders(w.Header())
			w.WriteHeader(http.StatusOK)
			streamStarted = true
		}
		if _, err := w.Write(encoded); err != nil {
			return err
		}
		archive.Write(encoded)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	}
	err := h.codexResponses.StreamCodexResponses(r.Context(), request, startStream, emit)
	usage := tokenUsage{PromptTokens: state.InputTokens, CompletionTokens: state.OutputTokens, TotalTokens: state.InputTokens + state.OutputTokens, Known: state.InputTokens > 0 || state.OutputTokens > 0}
	duration := time.Since(started)
	_ = h.writeArchiveResponse(round, "response.sse", archive.Bytes())
	if err != nil || !state.Completed {
		if err == nil {
			err = fmt.Errorf("conversion SSE ended without terminal event")
		}
		if !streamStarted {
			h.writeCodexResponsesError(w, r, round, started, plan.RouteOwner, model, true, err)
			return
		}
		failure := conversionStreamFailure(err)
		h.recordAndPrintFail(round, r, plan.RouteOwner, model, true, http.StatusOK, duration, usage, failure)
		h.writeArchiveMetadata(round, plan.RouteOwner, model, true, http.StatusOK, duration, usage, "response.sse", err.Error(), "", outcomeFromStreamFail(failure, http.StatusOK))
		return
	}
	h.recordAndPrint(round, r, plan.RouteOwner, model, true, http.StatusOK, duration, usage, "")
	h.writeArchiveMetadata(round, plan.RouteOwner, model, true, http.StatusOK, duration, usage, "response.sse", "", "", "success")
}
