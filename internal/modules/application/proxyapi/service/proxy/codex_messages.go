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
	plan.ConversionLevel = 2
	h.archiveAndLogTransportPlan(round, r, plan, effectivecatalog.BuiltinProviderViewFor(plan.RouteOwner), stream)
	conversionStart := time.Now()
	capability := config.ConversionCapability{Level: 2, Text: true, Tools: true, Streaming: true, Continuation: true, StructuredOutput: true}
	var err error
	capability, err = anthropicTargetReasoning(body, h.currentConfig().ModelMetadata[model], capability)
	if err != nil {
		round.SetConversionDuration(time.Since(conversionStart))
		h.writeArchivedAPIError(w, round, r, started, plan.RouteOwner, model, stream, http.StatusBadRequest, conversionAPIError(plan, err))
		return
	}
	if session := anthropicEmbeddedSession(body); session != "" {
		header := strings.TrimSpace(r.Header.Get("X-Claude-Code-Session-Id"))
		if header != "" && header != session {
			round.SetConversionDuration(time.Since(conversionStart))
			h.writeArchivedAPIError(w, round, r, started, plan.RouteOwner, model, stream, http.StatusBadRequest, conversionAPIError(plan, fmt.Errorf("metadata.user_id.session_id conflicts with session header")))
			return
		}
		if header == "" {
			r = r.Clone(r.Context())
			r.Header.Set("X-Claude-Code-Session-Id", session)
		}
	}
	responsesBody, degraded, err := buildResponsesFromAnthropicWithCapability(body, model, stream, capability)
	if err != nil {
		round.SetConversionDuration(time.Since(conversionStart))
		h.writeArchivedAPIError(w, round, r, started, plan.RouteOwner, model, stream, http.StatusBadRequest, conversionAPIError(plan, err))
		return
	}
	r = withAnthropicStops(r, body)
	normalized, normalizedBody, ignored, err := normalizeCodexRequest(responsesBody, false)
	if err != nil {
		round.SetConversionDuration(time.Since(conversionStart))
		h.writeArchivedError(w, round, r, started, plan.RouteOwner, model, stream, http.StatusBadRequest, err.Error())
		return
	}
	turnMetadata, turnMetadataIgnored := codexTurnMetadataFrom(r.Header, responsesBody)
	ignored = append(ignored, turnMetadataIgnored...)
	markConversionDegraded(round, append(degraded, ignored...))
	// CP-HDR-022: the adapter entry is a Codex entry too, so an inbound turn
	// state must reach the executor instead of being replaced by the fallback.
	turnState, err := codexTurnStateFromHeaders(r.Header)
	if err != nil {
		round.SetConversionDuration(time.Since(conversionStart))
		h.writeArchivedError(w, round, r, started, plan.RouteOwner, model, stream, http.StatusBadRequest, err.Error())
		return
	}
	sessionHash := codexSessionHash(r, model, normalizedBody)
	normalized, _, cacheKeySource, err := ensureCodexPromptCacheKey(normalized, normalizedBody, codexPromptCacheHash(r, model, normalizedBody))
	if err != nil {
		round.SetConversionDuration(time.Since(conversionStart))
		h.writeArchivedError(w, round, r, started, plan.RouteOwner, model, stream, http.StatusInternalServerError, err.Error())
		return
	}
	round.SetConversionDuration(time.Since(conversionStart))
	// Codex may emit reasoning even when the client did not request thinking.
	// Response projection records its omission independently of request adaptation.
	capability.Reasoning = true
	diagnostics := codexresponses.ParseDiagnostics(r.Header.Get("X-Codex-Turn-Metadata"))
	userAgent, originator := codexConvertedClientIdentityWithDiagnostics(r.Header, &diagnostics)
	request := codexresponses.Request{ObserveAttempt: h.codexAttemptObserver(round, r, "codexoauth"), Model: model, Body: normalized, SessionHash: sessionHash, LogicalThreadHash: codexLogicalThreadHash(r, model, body), TurnState: turnState, SessionScope: codexTurnStateScopeDigest(r, model, body), PromptCacheKeySource: cacheKeySource, ClientUserAgent: userAgent, ClientOriginator: originator, TurnMetadata: turnMetadata}
	request.Diagnostics = diagnostics
	request.Diagnostics.RequestID = requestIDFromContext(r.Context())
	if !stream {
		result, execErr := h.codexResponses.CompleteCodexResponses(r.Context(), request)
		if execErr != nil {
			h.writeCodexResponsesError(w, r, round, started, plan.RouteOwner, model, false, execErr)
			return
		}
		h.archiveCodexUpstreamAttempt(round, r, plan.RouteOwner, result.Attempt, nil)
		round.SetTurnStateFallback(codexresponses.TurnStateFallback(result.TurnStateSource))
		conversionStart = time.Now()
		converted, usage, degradedResponse, convertErr := convertOpenAIResponsesToAnthropicWithCapability(result.Body, model, capability)
		if convertErr == nil {
			converted, convertErr = applyAnthropicStops(converted, anthropicStops(r))
		}
		round.SetConversionDuration(round.ConversionDuration + time.Since(conversionStart))
		if convertErr != nil {
			failure := newStreamFailWithCode(streamKindConversion, "conversion_response_error", "Codex response conversion failed", convertErr, false)
			h.writeArchivedAPIError(w, round, r, started, plan.RouteOwner, model, false, http.StatusBadGateway, APIError{Code: "conversion_response_error", Message: failure.Message}, failure)
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
	mapper := withAnthropicStopMapper(responsesEventToAnthropicWithCapability(capability), anthropicStops(r))
	var archive bytes.Buffer
	streamStarted := false
	startStream := func(info codexresponses.StreamStart) error {
		h.archiveCodexUpstreamAttempt(round, r, plan.RouteOwner, info.Attempt, nil)
		round.SetTurnStateFallback(codexresponses.TurnStateFallback(info.TurnStateSource))
		recordFirstEventDuration(r.Context(), round, info.FirstEventDuration)
		return nil
	}
	emit := func(line []byte) error {
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" || !strings.HasPrefix(trimmed, "data:") {
			return nil
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		mappingStart := time.Now()
		events, err := mapper([]byte(payload), state)
		if err != nil {
			round.SetConversionDuration(round.ConversionDuration + time.Since(mappingStart))
			return codexresponses.NewFailure(codexresponses.KindConversion, 0, fmt.Errorf("converted SSE: %w", err))
		}
		encoded, err := encodeConversionSSE(events, true)
		round.SetConversionDuration(round.ConversionDuration + time.Since(mappingStart))
		if err != nil {
			return codexresponses.NewFailure(codexresponses.KindConversion, 0, fmt.Errorf("converted SSE: %w", err))
		}
		if len(encoded) == 0 {
			return nil
		}
		if archive.Len()+len(encoded) > maxConversionSSEBytes {
			return codexresponses.NewFailure(codexresponses.KindConversion, 0, fmt.Errorf("conversion SSE exceeds %d bytes", maxConversionSSEBytes))
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
	markConversionDegraded(round, state.IgnoredFeatures)
	usage := state.tokenUsage()
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
		if r.Context().Err() == nil && failure.Kind != streamKindClientWrite {
			// The HTTP status is already committed. Deliver an Anthropic error
			// terminal instead of silently closing an unfinished message.
			terminal := []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream stream did not complete\"}}\n\n")
			if _, writeErr := w.Write(terminal); writeErr == nil {
				archive.Write(terminal)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				_ = h.writeArchiveResponse(round, "response.sse", archive.Bytes())
			}
		}
		h.recordAndPrintFail(round, r, plan.RouteOwner, model, true, http.StatusOK, duration, usage, failure)
		h.writeArchiveMetadata(round, plan.RouteOwner, model, true, http.StatusOK, duration, usage, "response.sse", err.Error(), "", outcomeFromStreamFail(failure, http.StatusOK))
		return
	}
	h.recordAndPrint(round, r, plan.RouteOwner, model, true, http.StatusOK, duration, usage, "")
	h.writeArchiveMetadata(round, plan.RouteOwner, model, true, http.StatusOK, duration, usage, "response.sse", "", "", "success")
}
