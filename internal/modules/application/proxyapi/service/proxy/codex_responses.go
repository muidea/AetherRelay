package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	archivepkg "aetherrelay/internal/pkg/aetherrelayarchive"
)

var codexCompactHeartbeatInterval = 15 * time.Second

func (h *Handler) handleCodexCompact(w http.ResponseWriter, r *http.Request, requestID string) {
	started := time.Now()
	round := archiveRoundFromContext(r.Context())
	raw, err := h.readLimitedBody(w, r)
	if err != nil {
		h.writeArchivedError(w, round, r, started, "", "", false, http.StatusBadRequest, err.Error())
		return
	}
	defer r.Body.Close()
	if err := h.writeArchiveRequest(round, raw); err != nil {
		h.writeArchivedAPIError(w, round, r, started, "", "", false, http.StatusInternalServerError, APIError{Code: ErrorCodeProxyInternalError, Message: "archive request failed", ClientEndpoint: NormalizeClientEndpoint(r.URL.Path), ClientProtocol: ClientProtocolOpenAI})
		return
	}
	h.archiveAndLogClientRequest(round, r, len(raw))
	clientBody, model, clientStream := parseRawRequestBody(raw)
	// Compact shares the Responses model and access contract, while retaining
	// its distinct endpoint for usage and archive labels.
	routeRequest := r.Clone(r.Context())
	routeURL := *r.URL
	routeURL.Path = "/v1/responses"
	routeRequest.URL = &routeURL
	plans, apiErr := h.resolveTransportPlans(routeRequest, model)
	if apiErr != nil {
		h.writeArchivedAPIError(w, round, r, started, "", model, clientStream, statusForAPIError(apiErr), *apiErr)
		return
	}
	var plan TransportPlan
	for _, candidate := range plans {
		if candidate.Mode == TransportModeCodexOAuthResponses && candidate.UpstreamProtocol == effectivecatalog.CodexOAuthProviderID {
			plan = candidate
			break
		}
	}
	if plan.RouteOwner == "" {
		h.writeArchivedAPIError(w, round, r, started, "", model, clientStream, http.StatusBadRequest, APIError{Code: ErrorCodeEndpointUnsupported, Message: "no Codex OAuth account can serve responses compact", Model: model, ClientEndpoint: "/v1/responses/compact", ClientProtocol: ClientProtocolOpenAI})
		return
	}
	normalized, normalizedBody, ignored, features, normalizeErr := normalizeCodexHTTPRequest(raw, true, r.Header)
	if normalizeErr != nil {
		h.writeArchivedError(w, round, r, started, plan.RouteOwner, model, clientStream, http.StatusBadRequest, normalizeErr.Error())
		return
	}
	if round != nil {
		round.SetIgnoredFeatures(uniqueSortedFeatures(append(round.IgnoredFeatures, ignored...)))
	}
	h.archiveAndLogTransportPlan(round, r, plan, effectivecatalog.BuiltinProviderViewFor(plan.RouteOwner), clientStream)
	if h.codexResponses == nil {
		h.writeCodexResponsesError(w, r, round, started, plan.RouteOwner, model, clientStream, codexresponses.NewFailure(codexresponses.KindProviderUnavailable, 0, fmt.Errorf("Codex compact executor is unavailable")))
		return
	}
	sessionHash := codexSessionHash(r, model, clientBody)
	normalized, _, normalizeErr = ensureCodexPromptCacheKey(normalized, normalizedBody, sessionHash)
	if normalizeErr != nil {
		h.writeArchivedError(w, round, r, started, plan.RouteOwner, model, clientStream, http.StatusInternalServerError, normalizeErr.Error())
		return
	}
	request := codexresponses.Request{Model: model, Body: normalized, SessionHash: sessionHash, RemoteCompactionV2: features.RemoteCompactionV2, ResponsesLite: features.ResponsesLite}
	if clientStream {
		h.handleCodexCompactStream(w, r, round, started, plan, model, clientBody, request)
		return
	}
	response, compactErr := h.codexResponses.CompleteCodexCompact(r.Context(), request)
	if compactErr != nil {
		h.writeCodexResponsesError(w, r, round, started, plan.RouteOwner, model, clientStream, compactErr)
		return
	}
	copyCodexHeaders(w.Header(), response.Headers)
	responseBody := response.Body
	responseFile := "response.json"
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(http.StatusOK)
	_, writeErr := w.Write(responseBody)
	_ = h.writeArchiveResponse(round, responseFile, responseBody)
	tok := codexResponseUsage(response.Body, clientBody)
	if writeErr != nil {
		h.recordAndPrintFail(round, r, plan.RouteOwner, model, clientStream, http.StatusOK, time.Since(started), tok, streamFailFromCodex(codexresponses.NewFailure(codexresponses.KindClientWrite, 0, writeErr)))
		return
	}
	h.recordAndPrint(round, r, plan.RouteOwner, model, clientStream, http.StatusOK, time.Since(started), tok, "")
	h.writeArchiveMetadata(round, plan.RouteOwner, model, clientStream, http.StatusOK, time.Since(started), tok, responseFile, "", "", "success")
	_ = requestID
}

func (h *Handler) handleCodexCompactStream(w http.ResponseWriter, r *http.Request, round *archivepkg.Round, started time.Time, plan TransportPlan, model string, requestBody map[string]any, request codexresponses.Request) {
	prepareSSEHeaders(w.Header())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(": aetherrelay compact pending\n\n"))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	done, startErr := h.codexResponses.StartCodexCompact(r.Context(), request)
	if startErr != nil {
		_, _ = w.Write(codexCompactFailureSSE(startErr))
		return
	}
	ticker := time.NewTicker(codexCompactHeartbeatInterval)
	defer ticker.Stop()
	var response codexresponses.Result
	var compactErr error
	waiting := true
	for waiting {
		select {
		case completed := <-done:
			response, compactErr, waiting = completed.Result, completed.Err, false
		case <-ticker.C:
			if _, err := w.Write([]byte(": aetherrelay compact pending\n\n")); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
	var responseBody []byte
	if compactErr != nil {
		responseBody = codexCompactFailureSSE(compactErr)
	} else {
		var err error
		responseBody, err = codexCompactSSE(response.Body)
		if err != nil {
			compactErr = codexresponses.NewFailure(codexresponses.KindProtocol, 0, err)
			responseBody = codexCompactFailureSSE(compactErr)
		}
	}
	_, writeErr := w.Write(responseBody)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	_ = h.writeArchiveResponse(round, "response.sse", responseBody)
	usage := codexResponseUsage(response.Body, requestBody)
	duration := time.Since(started)
	if compactErr != nil || writeErr != nil {
		failureErr := compactErr
		if writeErr != nil {
			failureErr = codexresponses.NewFailure(codexresponses.KindClientWrite, 0, writeErr)
		}
		failure := streamFailFromCodexError(failureErr)
		h.recordAndPrintFail(round, r, plan.RouteOwner, model, true, http.StatusOK, duration, usage, failure)
		h.writeArchiveMetadata(round, plan.RouteOwner, model, true, http.StatusOK, duration, usage, "response.sse", failure.Error(), "", outcomeFromStreamFail(failure, http.StatusOK))
		return
	}
	h.recordAndPrint(round, r, plan.RouteOwner, model, true, http.StatusOK, duration, usage, "")
	h.writeArchiveMetadata(round, plan.RouteOwner, model, true, http.StatusOK, duration, usage, "response.sse", "", "", "success")
}

func codexCompactFailureSSE(err error) []byte {
	failure := streamFailFromCodexError(err)
	payload, _ := json.Marshal(map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"object": "response", "status": "failed",
			"error": map[string]any{"code": failure.ErrorCode, "message": "Codex compact request failed"},
		},
	})
	return []byte("event: response.failed\ndata: " + string(payload) + "\n\n")
}

func codexCompactSSE(raw []byte) ([]byte, error) {
	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("invalid Codex compact response")
	}
	if _, ok := response["id"].(string); !ok {
		return nil, fmt.Errorf("Codex compact response id is missing")
	}
	var out bytes.Buffer
	if items, ok := response["output"].([]any); ok {
		for index, item := range items {
			event, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
			fmt.Fprintf(&out, "event: response.output_item.done\ndata: %s\n\n", event)
		}
	}
	completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
	fmt.Fprintf(&out, "event: response.completed\ndata: %s\n\n", completed)
	return out.Bytes(), nil
}

// handleCodexOAuthResponses relays the native Responses object rather than
// converting it through the ChatGPT Web message-tree adapter. This preserves
// native output items and tools within the documented HTTP Responses P0.
func (h *Handler) handleCodexOAuthResponses(w http.ResponseWriter, r *http.Request, started time.Time, provider, model string, raw []byte, body map[string]any, stream bool, sessionHash string, features codexRequestFeatures) {
	round := archiveRoundFromContext(r.Context())
	executor := h.codexResponses
	if executor == nil {
		h.writeCodexResponsesError(w, r, round, started, provider, model, stream, codexresponses.NewFailure(codexresponses.KindProviderUnavailable, 0, fmt.Errorf("Codex Responses executor is unavailable")))
		return
	}
	request := codexresponses.Request{Model: model, Body: bytes.Clone(raw), SessionHash: sessionHash, RemoteCompactionV2: features.RemoteCompactionV2, ResponsesLite: features.ResponsesLite}
	if !stream {
		response, err := executor.CompleteCodexResponses(r.Context(), request)
		if err != nil {
			h.writeCodexResponsesError(w, r, round, started, provider, model, false, err)
			return
		}
		h.writeCodexOAuthCompleteSuccess(w, r, round, started, provider, model, body, response)
		return
	}

	streamStarted := false
	var archive bytes.Buffer
	accumulator := newResponsesStreamAccumulator(model)
	maxStream, _ := h.streamLimits()
	accumulator.SetMaxContent(maxStream)
	var total int64
	terminalObserved := false
	startStream := func(info codexresponses.StreamStart) error {
		if streamStarted {
			return nil
		}
		copyCodexHeaders(w.Header(), info.Headers)
		prepareSSEHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		streamStarted = true
		return nil
	}
	emit := func(line []byte) error {
		upstreamLine := line
		line, _ = sanitizeCodexCapacitySSEForClient(upstreamLine)
		total += int64(len(line))
		if total > maxStream {
			return codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("stream exceeds limit of %d bytes", maxStream))
		}
		accumulator.TrackSSELine(upstreamLine)
		terminalObserved = terminalObserved || parseTerminalSSELine(streamProtoResponses, upstreamLine).Terminal
		archive.Write(upstreamLine)
		if _, err := w.Write(line); err != nil {
			return err
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	}
	err := executor.StreamCodexResponses(r.Context(), request, startStream, emit)
	if err != nil {
		if !streamStarted {
			h.writeCodexResponsesError(w, r, round, started, provider, model, true, err)
			return
		}
		failure := streamFailFromCodexError(err)
		if !terminalObserved {
			terminal := codexResponsesFailureSSE(failure)
			archive.Write(terminal)
			_, _ = w.Write(terminal)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		tok := accumulator.FinalizeUsage(body)
		_ = h.writeArchiveResponse(round, "response.sse", archive.Bytes())
		h.recordAndPrintFail(round, r, provider, model, true, http.StatusOK, time.Since(started), tok, failure)
		h.writeArchiveMetadata(round, provider, model, true, http.StatusOK, time.Since(started), tok, "response.sse", failure.Error(), "", outcomeFromStreamFail(failure, http.StatusOK))
		return
	}
	if !streamStarted {
		_ = startStream(codexresponses.StreamStart{})
	}
	_ = h.writeArchiveResponse(round, "response.sse", archive.Bytes())
	tok := accumulator.FinalizeUsage(body)
	h.recordAndPrint(round, r, provider, model, true, http.StatusOK, time.Since(started), tok, "")
	h.writeArchiveMetadata(round, provider, model, true, http.StatusOK, time.Since(started), tok, "response.sse", "", "", "success")
}

func sanitizeCodexCapacitySSEForClient(line []byte) ([]byte, bool) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return line, false
	}
	payload := []byte(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
	sanitized, changed := sanitizeCodexCapacityEventForClient(payload)
	if !changed {
		return line, false
	}
	ending := ""
	switch {
	case bytes.HasSuffix(line, []byte("\r\n\r\n")):
		ending = "\r\n\r\n"
	case bytes.HasSuffix(line, []byte("\n\n")):
		ending = "\n\n"
	case bytes.HasSuffix(line, []byte("\r\n")):
		ending = "\r\n"
	case bytes.HasSuffix(line, []byte("\n")):
		ending = "\n"
	}
	return []byte("data: " + string(sanitized) + ending), true
}

func sanitizeCodexCapacityEventForClient(payload []byte) ([]byte, bool) {
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil {
		return payload, false
	}
	typ, _ := event["type"].(string)
	_, bareError := event["error"].(map[string]any)
	if typ != "error" && typ != "response.failed" && !(typ == "" && bareError) {
		return payload, false
	}
	changed := sanitizeCodexCapacityErrorObject(event["error"])
	if response, ok := event["response"].(map[string]any); ok {
		changed = sanitizeCodexCapacityErrorObject(response["error"]) || changed
	}
	if !changed {
		return payload, false
	}
	sanitized, err := json.Marshal(event)
	if err != nil {
		return payload, false
	}
	return sanitized, true
}

func sanitizeCodexCapacityErrorObject(value any) bool {
	errorObject, ok := value.(map[string]any)
	if !ok {
		return false
	}
	code, _ := errorObject["code"].(string)
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "server_is_overloaded", "slow_down":
		errorObject["code"] = "server_error"
		return true
	default:
		return false
	}
}

func codexResponsesFailureSSE(failure *streamFail) []byte {
	code := string(codexresponses.KindUpstream)
	if failure != nil && strings.TrimSpace(failure.ErrorCode) != "" {
		code = failure.ErrorCode
	}
	payload, _ := json.Marshal(map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"object": "response", "status": "failed",
			"error": map[string]any{"code": code, "message": "Codex stream failed"},
		},
	})
	return []byte("event: response.failed\ndata: " + string(payload) + "\n\n")
}

func (h *Handler) completeCodexOAuthResponse(ctx context.Context, model string, raw []byte, sessionHash ...string) (codexresponses.Result, error) {
	if h.codexResponses == nil {
		return codexresponses.Result{}, codexresponses.NewFailure(codexresponses.KindProviderUnavailable, 0, fmt.Errorf("Codex Responses executor is unavailable"))
	}
	request := codexresponses.Request{Model: model, Body: bytes.Clone(raw)}
	if len(sessionHash) > 0 {
		request.SessionHash = sessionHash[0]
	}
	return h.codexResponses.CompleteCodexResponses(ctx, request)
}

func (h *Handler) writeCodexOAuthCompleteSuccess(w http.ResponseWriter, r *http.Request, round *archivepkg.Round, started time.Time, provider, model string, requestBody map[string]any, response codexresponses.Result) {
	copyCodexHeaders(w.Header(), response.Headers)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(response.Body); err != nil {
		failure := codexresponses.NewFailure(codexresponses.KindClientWrite, 0, err)
		tok := codexResponseUsage(response.Body, requestBody)
		h.recordAndPrintFail(round, r, provider, model, false, http.StatusOK, time.Since(started), tok, streamFailFromCodex(failure))
		h.writeArchiveMetadata(round, provider, model, false, http.StatusOK, time.Since(started), tok, "response.json", failure.Error(), "", outcomeFromStreamFail(streamFailFromCodex(failure), http.StatusOK))
		return
	}
	_ = h.writeArchiveResponse(round, "response.json", response.Body)
	tok := codexResponseUsage(response.Body, requestBody)
	h.recordAndPrint(round, r, provider, model, false, http.StatusOK, time.Since(started), tok, "")
	h.writeArchiveMetadata(round, provider, model, false, http.StatusOK, time.Since(started), tok, "response.json", "", "", "success")
}

func (h *Handler) writeCodexResponsesError(w http.ResponseWriter, r *http.Request, round *archivepkg.Round, started time.Time, provider, model string, stream bool, err error) {
	failure := streamFailFromCodexError(err)
	codexFailure, _ := codexresponses.AsFailure(err)
	status := http.StatusBadGateway
	code := ErrorCodeUpstreamUnavailable
	switch failure.ErrorCode {
	case string(codexresponses.KindInvalidRequest):
		status = http.StatusBadRequest
		code = ErrorCodeInvalidRequest
	case string(codexresponses.KindProviderUnavailable):
		status = http.StatusServiceUnavailable
		code = ErrorCodeProviderUnavailable
	case string(codexresponses.KindRateLimit):
		status = http.StatusTooManyRequests
	case string(codexresponses.KindTimeout):
		status = http.StatusGatewayTimeout
	case string(codexresponses.KindClientCanceled):
		status = 499
	}
	if failure.ErrorCode == string(codexresponses.KindInvalidToken) {
		code = ErrorCodeUpstreamUnavailable
	}
	message := "Codex OAuth response failed: " + failure.ErrorCode
	if codexFailure != nil && codexFailure.HTTPStatus > 0 {
		message += fmt.Sprintf(" (upstream HTTP %d)", codexFailure.HTTPStatus)
	}
	errorType, param := "", ""
	if codexFailure != nil && codexFailure.Kind == codexresponses.KindInvalidRequest {
		if codexFailure.UpstreamCode != "" {
			code = codexFailure.UpstreamCode
		}
		if codexFailure.UpstreamMessage != "" {
			message = codexFailure.UpstreamMessage
		}
		errorType = codexFailure.UpstreamType
		param = codexFailure.UpstreamParam
	}
	h.writeArchivedAPIError(w, round, r, started, provider, model, stream, status, APIError{Code: code, Message: message, Type: errorType, Param: param, Model: model, ClientProtocol: ClientProtocolOpenAI, ClientEndpoint: NormalizeClientEndpoint(r.URL.Path), UpstreamProtocol: effectivecatalog.CodexOAuthProviderID})
}

func copyCodexHeaders(target http.Header, headers []codexresponses.Header) {
	for _, header := range headers {
		name, value := strings.TrimSpace(header.Name), strings.TrimSpace(header.Value)
		if name == "Content-Type" || name == "X-Request-ID" {
			if value != "" {
				target.Set(name, value)
			}
		}
	}
}

func codexResponseUsage(body []byte, request map[string]any) tokenUsage {
	var payload struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(body, &payload) == nil {
		if usage, ok := usageFromRaw(payload.Usage); ok {
			return usage
		}
	}
	return tokenUsage{PromptTokens: estimatePromptTokens(request), CompletionTokens: estimateCompletionTokensFromResponse(body), Estimated: true, Known: true}
}

func streamFailFromCodexError(err error) *streamFail {
	failure, ok := codexresponses.AsFailure(err)
	if !ok || failure == nil {
		return newStreamFailWithCode(streamKindUpstreamFailed, string(codexresponses.KindUpstream), "Codex upstream failed", err, true)
	}
	return streamFailFromCodex(failure)
}

func streamFailFromCodex(failure *codexresponses.Failure) *streamFail {
	if failure == nil {
		return nil
	}
	var kind streamKind
	countUpstream := false
	switch failure.Kind {
	case codexresponses.KindInvalidRequest:
		kind = streamKindError
	case codexresponses.KindClientCanceled:
		kind = streamKindClientCanceled
	case codexresponses.KindClientWrite:
		kind = streamKindClientWrite
	case codexresponses.KindProtocol:
		kind = streamKindProtocol
	case codexresponses.KindRateLimit, codexresponses.KindInvalidToken, codexresponses.KindTimeout, codexresponses.KindNetwork, codexresponses.KindUpstream:
		kind, countUpstream = streamKindUpstreamFailed, true
	default:
		kind = streamKindError
	}
	return newStreamFailWithCode(kind, string(failure.Kind), failure.Error(), failure, countUpstream)
}
