package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"aetherrelay/internal/modules/application/proxyapi/pkg/usagetarget"
	"aetherrelay/internal/pkg/aetherrelayclientauth"
	"aetherrelay/internal/pkg/aetherrelaycodex"
	"github.com/gorilla/websocket"
)

var codexWebsocketUpgrader = websocket.Upgrader{
	ReadBufferSize: 4096, WriteBufferSize: 4096,
	CheckOrigin: func(*http.Request) bool { return true },
}

func (h *Handler) handleCodexWebsocket(w http.ResponseWriter, r *http.Request, requestID string) {
	_ = requestID
	started := time.Now()
	round := archiveRoundFromContext(r.Context())
	if h.codexResponses == nil {
		h.writeCodexResponsesError(w, r, round, started, effectivecatalog.CodexOAuthProviderID, "", true, codexresponses.NewFailure(codexresponses.KindProviderUnavailable, 0, fmt.Errorf("Codex websocket executor is unavailable")))
		return
	}
	maxSessions, maxMessageBytes, idleTimeout, maxLifetime := h.currentConfig().CodexOAuth.EffectiveWebsocketLimits()
	active := h.codexWebsockets.Add(1)
	if active > int64(maxSessions) {
		h.codexWebsockets.Add(-1)
		h.writeArchivedAPIError(w, round, r, started, effectivecatalog.CodexOAuthProviderID, "", true, http.StatusServiceUnavailable, APIError{Code: ErrorCodeProviderUnavailable, Message: "Codex websocket session limit reached", ClientEndpoint: NormalizeClientEndpoint(r.URL.Path), ClientProtocol: ClientProtocolOpenAI, UpstreamProtocol: effectivecatalog.CodexOAuthProviderID})
		return
	}
	defer h.codexWebsockets.Add(-1)
	// Copy the upgrader: its error callback captures this request and must not
	// be installed on the shared value used by concurrent handshakes.
	upgrader := codexWebsocketUpgrader
	rejected := false
	upgrader.Error = func(w http.ResponseWriter, r *http.Request, status int, reason error) {
		rejected = true
		code := ErrorCodeInvalidRequest
		if status >= 500 {
			code = ErrorCodeProxyInternalError
		}
		w.Header().Set("Sec-WebSocket-Version", "13")
		h.archiveAndLogClientRequest(round, r, 0)
		h.writeArchivedAPIError(w, round, r, started, "", "", false, status, APIError{
			Code: code, Message: reason.Error(), ClientEndpoint: "/v1/responses", ClientProtocol: ClientProtocolOpenAI,
		})
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		if !rejected {
			// A transport failure after hijacking cannot receive another HTTP
			// response. Keep the captured status and finish its observation.
			status := http.StatusInternalServerError
			if response, ok := round.ClientResponse(); ok {
				status = response.Status
			}
			h.archiveAndLogClientRequest(round, r, 0)
			h.recordCodexWebsocketLocalResult(r, "", started, status, newStreamFailWithCode(streamKindClientWrite, "client_write", "websocket handshake transport failed", nil, false))
		}
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxMessageBytes)
	_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(idleTimeout))
	})
	ctx, cancel := context.WithTimeout(r.Context(), maxLifetime)
	defer cancel()
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		interval := idleTimeout / 2
		if interval <= 0 || interval > 30*time.Second {
			interval = 30 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "session closed"), time.Now().Add(time.Second))
				_ = conn.Close()
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					cancel()
					_ = conn.Close()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-pingDone
	}()
	model := ""
	sessionID := ""
	httpFallback := false
	lastResponseID := ""
	var sessionFeatures codexRequestFeatures
	var sessionOpenRequest codexresponses.WebsocketOpenRequest
	replayState := newCodexWebsocketReplayState(maxMessageBytes)
	turnNumber := 0
	baseRequest, baseRound, sessionContext := r, round, ctx
	messageNumber := 0
	turnStarted := started
	var finishTurn func()
	defer func() {
		if recovered := recover(); recovered != nil {
			// After the upgrade, only WebSocket frames may be written.
			slog.Error("Codex websocket panic recovered", "request_id", requestID, "error", recovered, "stack", string(debug.Stack()))
			completion := usageCompletionFromContext(r.Context())
			if completion == nil || !completion.done.Load() {
				failure := newStreamFailWithCode(streamKindError, ErrorCodeProxyInternalError, "Codex websocket internal error", nil, false)
				h.recordCodexWebsocketLocalResult(r, model, turnStarted, http.StatusSwitchingProtocols, failure)
			}
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "internal error"), time.Now().Add(time.Second))
		}
		if finishTurn != nil {
			finishTurn()
		}
	}()
	defer func() {
		if sessionID != "" {
			h.codexResponses.CloseCodexWebsocket(context.WithoutCancel(ctx), sessionID)
		}
	}()
	for {
		if finishTurn != nil {
			finishTurn()
			finishTurn = nil
		}
		_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
		messageType, raw, readErr := conn.ReadMessage()
		if readErr != nil {
			if messageNumber == 0 {
				// The HTTP request already owns a usage event, even if no create
				// arrives. Closing an unused session is not a generation failure.
				h.archiveAndLogClientRequest(baseRound, baseRequest, 0)
				failure := newStreamFailWithCode(streamKindClientCanceled, "client_canceled", "websocket closed before response.create", nil, false)
				var timeout net.Error
				switch {
				case errors.Is(readErr, websocket.ErrReadLimit):
					failure = newStreamFailWithCode(streamKindLimitExceeded, ErrorCodeRequestTooLarge, "websocket message exceeds configured limit", nil, false)
				case ctx.Err() == context.DeadlineExceeded:
					failure = newStreamFailWithCode(streamKindRequestTimeout, "request_timeout", "websocket lifetime expired before response.create", nil, false)
				case errors.As(readErr, &timeout) && timeout.Timeout():
					failure = newStreamFailWithCode(streamKindIdleTimeout, "idle_timeout", "websocket idle timeout before response.create", nil, false)
				}
				h.recordCodexWebsocketLocalResult(baseRequest, "", started, http.StatusSwitchingProtocols, failure)
			}
			return
		}
		turnStarted = time.Now()
		messageNumber++
		if messageNumber > 1 {
			var err error
			round, err = h.startRound(websocketAPIKeyID(baseRequest))
			if err != nil {
				writeCodexWebsocketError(conn, "proxy_internal_error", "start websocket archive failed")
				return
			}
			eventID := newRequestID()
			r = baseRequest.WithContext(withUsageCompletion(withUsageEventID(withArchiveRound(sessionContext, round), eventID), &usageCompletion{}))
			if round != nil {
				round.EventID = eventID
				round.SetRequestID(requestID)
				round.SetAPIKeyID(websocketAPIKeyID(baseRequest))
			}
			if err := h.startUsage(r, eventID, round); err != nil {
				if round != nil {
					_ = round.Abort()
				}
				writeCodexWebsocketError(conn, "usage_store_unavailable", "usage store unavailable")
				return
			}
		} else {
			// The HTTP request owns the first completion; later turns receive
			// their own completion and target tracker from withUsageCompletion.
			r = baseRequest.WithContext(usagetarget.WithTracker(sessionContext))
		}
		turnCtx := r.Context()
		currentRequest, currentRound := r, round
		finishTurn = func() {
			h.completePendingUsage(currentRequest, currentRound)
			if currentRound != nil && currentRound != baseRound {
				_ = currentRound.Abort()
			}
		}
		_ = h.writeArchiveRequest(round, raw)
		h.archiveAndLogClientRequest(round, r, len(raw))
		round.SetTransportPlan(RouteLabel(r), "/v1/responses", ClientProtocolOpenAI, effectivecatalog.CodexOAuthProviderID, "codex_oauth_responses", TransportModeCodexOAuthResponses)
		reject := func(code, message string) {
			writeCodexWebsocketError(conn, code, message)
			h.recordCodexWebsocketLocalResult(r, model, turnStarted, http.StatusSwitchingProtocols, newStreamFailWithCode(streamKindError, code, message, nil, false))
		}
		if messageType != websocket.TextMessage {
			reject("invalid_request", "only text response.create messages are supported")
			return
		}
		normalized, requestModel, features, normalizeErr := normalizeCodexWebsocketCreate(raw, model, r.Header, sessionFeatures)
		if normalizeErr != nil {
			reject("invalid_request", normalizeErr.Error())
			return
		}
		if model == "" {
			model = requestModel
			sessionFeatures = features
			routeRequest := (&http.Request{Method: http.MethodPost, URL: cloneURLPath(r, "/v1/responses"), Header: r.Header, Body: r.Body, Host: r.Host}).WithContext(r.Context())
			plans, apiErr := h.resolveTransportPlans(routeRequest, model)
			if apiErr != nil || !hasCodexWebsocketPlan(plans) {
				message := "model is not available for Codex websocket"
				if apiErr != nil {
					message = apiErr.Message
				}
				reject("model_not_found", message)
				return
			}
			sessionHash := codexSessionHash(r, model, map[string]any{"prompt_cache_key": websocketPromptCacheKey(normalized)})
			// CP-HDR-022: the record unit comes from the declared conversation,
			// which only exists on the raw create payload (normalization strips
			// client_metadata before the upstream request).
			var scopeBody map[string]any
			_ = decodeCodexJSON(raw, &scopeBody)
			userAgent, originator := codexClientIdentity(r.Header)
			turnMetadata, turnMetadataIgnored := codexTurnMetadataFrom(r.Header, raw)
			if round != nil && len(turnMetadataIgnored) > 0 {
				round.SetIgnoredFeatures(uniqueSortedFeatures(append(round.IgnoredFeatures, turnMetadataIgnored...)))
			}
			sessionOpenRequest = codexresponses.WebsocketOpenRequest{Model: model, SessionHash: sessionHash, LogicalThreadHash: codexLogicalThreadHash(r, model, scopeBody), BetaFeatures: features.BetaFeatures, ResponsesLite: features.ResponsesLite, TurnState: features.TurnState, SessionScope: codexTurnStateScopeDigest(r, model, scopeBody), ClientUserAgent: userAgent, ClientOriginator: originator, TurnMetadata: turnMetadata}
			opened, openErr := h.codexResponses.OpenCodexWebsocket(turnCtx, sessionOpenRequest)
			if openErr != nil {
				if failure, ok := codexresponses.AsFailure(openErr); ok {
					h.archiveCodexUpstreamAttempt(round, r, effectivecatalog.CodexOAuthProviderID, failure.Attempt, openErr)
					recordCodexTurnStateFallback(round, failure.TurnStateSource, failure.Attempt.Response.Observed)
				}
				writeCodexWebsocketFailure(conn, openErr, "upstream_unavailable", "Codex websocket could not be opened")
				h.recordCodexWebsocketFailure(r, model, turnStarted, openErr)
				return
			}
			h.archiveCodexUpstreamAttempt(round, r, effectivecatalog.CodexOAuthProviderID, opened.Attempt, nil)
			round.SetTurnStateFallback(codexresponses.TurnStateFallback(opened.TurnStateSource))
			sessionID = opened.SessionID
			httpFallback = opened.HTTPFallback
		}
		if requestModel != model {
			reject("invalid_request", "websocket model cannot change within a session")
			return
		}
		if features != sessionFeatures {
			reject("invalid_request", "websocket feature profile cannot change within a session")
			return
		}
		var normalizedBody map[string]any
		if err := decodeCodexJSON(normalized, &normalizedBody); err != nil {
			reject("invalid_request", "invalid normalized response.create message")
			return
		}
		var cacheErr error
		normalized, normalizedBody, _, cacheErr = ensureCodexPromptCacheKey(normalized, normalizedBody, codexPromptCacheHash(r, model, normalizedBody))
		if cacheErr != nil {
			reject("invalid_request", cacheErr.Error())
			return
		}
		turnReplay, replayErr := replayState.prepare(normalized)
		if replayErr != nil {
			reject("invalid_request", "Codex websocket replay state is invalid")
			return
		}
		if matches, _ := codexWebsocketHistoryMatches(normalized, lastResponseID); !matches {
			// Native WS may resolve IDs outside our retained transcript, but that
			// result must not make local replay history appear complete.
			turnReplay.safe = false
		}
		turnNumber++
		attemptPayload := normalized
		if httpFallback {
			full, safe, err := buildCodexWebsocketHTTPPayload(normalized, turnReplay, lastResponseID, maxMessageBytes)
			if err != nil || !safe {
				writeCodexWebsocketError(conn, "invalid_request", "HTTP fallback requires a complete validated transcript")
				h.recordCodexWebsocketFailure(r, model, turnStarted, codexresponses.NewFailure(codexresponses.KindInvalidRequest, 0, fmt.Errorf("HTTP fallback requires a complete validated transcript")))
				return
			}
			attemptPayload = full
		}
		migrationAttempts := 0
		var turnResult codexWebsocketForwardResult
		for {
			sendErr := h.codexResponses.SendCodexWebsocket(turnCtx, sessionID, attemptPayload)
			if sendErr != nil {
				failure, typed := codexresponses.AsFailure(sendErr)
				if typed {
					h.archiveCodexUpstreamAttempt(round, r, effectivecatalog.CodexOAuthProviderID, failure.Attempt, sendErr)
				}
				// Only a definite HTTP 429 rejection can enter the replay path.
				// A failed native write or HTTP network error has unknown execution.
				if !httpFallback || !typed || failure.Kind != codexresponses.KindRateLimit || failure.HTTPStatus != http.StatusTooManyRequests {
					writeCodexWebsocketFailure(conn, sendErr, "upstream_unavailable", "Codex websocket send failed")
					h.recordCodexWebsocketFailure(r, model, turnStarted, sendErr)
					return
				}
				turnResult = codexWebsocketForwardResult{terminal: true, failure: failure}
			} else {
				turnResult, err = h.forwardCodexWebsocketTurn(r, conn, sessionID, maxMessageBytes, turnStarted)
			}
			if err != nil {
				writeCodexWebsocketError(conn, "upstream_failed", err.Error())
				h.recordAndPrint(round, r, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(turnStarted), turnResult.usage, err.Error())
				h.writeArchiveMetadata(round, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(turnStarted), turnResult.usage, "response.ws.jsonl", err.Error(), "", "upstream_failed")
				return
			}
			canMigrate := turnCtx.Err() == nil && turnNumber > 1 && migrationAttempts < maxCodexWebsocketTurnMigrations && !turnResult.downstreamWritten && turnResult.failure != nil && turnResult.failure.Kind == codexresponses.KindRateLimit
			if canMigrate {
				retryPayload, retrySafe, retryErr := buildCodexWebsocketHTTPPayload(normalized, turnReplay, lastResponseID, maxMessageBytes)
				if retryErr != nil {
					reject("invalid_request", "Codex websocket replay could not be prepared")
					return
				}
				if retrySafe {
					h.codexResponses.CloseCodexWebsocket(context.WithoutCancel(turnCtx), sessionID)
					sessionID = ""
					opened, openErr := h.codexResponses.OpenCodexWebsocket(turnCtx, sessionOpenRequest)
					if openErr != nil {
						if failure, ok := codexresponses.AsFailure(openErr); ok {
							h.archiveCodexUpstreamAttempt(round, r, effectivecatalog.CodexOAuthProviderID, failure.Attempt, openErr)
							recordCodexTurnStateFallback(round, failure.TurnStateSource, failure.Attempt.Response.Observed)
						}
						writeCodexWebsocketFailure(conn, openErr, "upstream_unavailable", "Codex websocket replacement account is unavailable")
						h.recordCodexWebsocketFailure(r, model, turnStarted, openErr)
						return
					}
					h.archiveCodexUpstreamAttempt(round, r, effectivecatalog.CodexOAuthProviderID, opened.Attempt, nil)
					round.SetTurnStateFallback(codexresponses.TurnStateFallback(opened.TurnStateSource))
					sessionID = opened.SessionID
					httpFallback = opened.HTTPFallback
					attemptPayload = retryPayload
					migrationAttempts++
					continue
				}
			}
			if sendErr != nil {
				writeCodexWebsocketFailure(conn, sendErr, "upstream_unavailable", "Codex websocket send failed")
				h.recordCodexWebsocketFailure(r, model, turnStarted, sendErr)
				return
			}
			if len(turnResult.deferred) > 0 {
				if err := writeCodexWebsocketFrames(conn, turnResult.deferred); err != nil {
					return
				}
				turnResult.downstreamWritten = true
			}
			if turnResult.failure != nil && !turnResult.downstreamWritten {
				writeCodexWebsocketFailure(conn, turnResult.failure, "upstream_failed", "Codex websocket turn failed")
			}
			break
		}
		if !turnResult.terminal {
			return
		}
		outcome := "success"
		message := ""
		if !turnResult.reusable {
			outcome = "upstream_failed"
			message = "Codex websocket turn failed"
		} else {
			replayState.commit(turnReplay, turnResult.outputItems)
			lastResponseID = turnResult.responseID
		}
		if turnResult.failure != nil {
			failure := streamFailFromCodexError(turnResult.failure)
			h.recordAndPrintFail(round, r, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(turnStarted), turnResult.usage, failure)
			outcome = outcomeFromStreamFail(failure, http.StatusSwitchingProtocols)
			message = turnResult.failure.Error()
		} else {
			h.recordAndPrint(round, r, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(turnStarted), turnResult.usage, message)
		}
		h.writeArchiveMetadata(round, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(turnStarted), turnResult.usage, "response.ws.jsonl", message, "", outcome)
		if !turnResult.reusable {
			return
		}
	}
}

// Local outcomes do not count as upstream errors. Only a target observed in
// this turn is retained; HTTP 101 remains the actual upgraded status.
func (h *Handler) recordCodexWebsocketLocalResult(r *http.Request, model string, started time.Time, status int, failure *streamFail) {
	round := archiveRoundFromContext(r.Context())
	if round != nil {
		round.Operation = RouteLabel(r)
		round.ClientEndpoint = "/v1/responses"
		round.ClientProtocol = ClientProtocolOpenAI
	}
	duration := time.Since(started)
	provider := usagetarget.Snapshot(r.Context()).Provider
	h.recordAndPrintFail(round, r, provider, model, true, status, duration, tokenUsage{}, failure)
	h.writeArchiveMetadata(round, provider, model, true, status, duration, tokenUsage{}, "", failure.Error(), "", outcomeFromStreamFail(failure, status))
}

func (h *Handler) recordCodexWebsocketFailure(r *http.Request, model string, started time.Time, err error) {
	round := archiveRoundFromContext(r.Context())
	failure := streamFailFromCodexError(err)
	h.recordAndPrintFail(round, r, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(started), tokenUsage{}, failure)
	h.writeArchiveMetadata(round, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(started), tokenUsage{}, "", err.Error(), "", outcomeFromStreamFail(failure, http.StatusSwitchingProtocols))
}

func websocketAPIKeyID(r *http.Request) string {
	if identity, internal := internalFeatureIdentity(r.Context()); internal {
		return identity.KeyID
	}
	return clientauth.ClientIdentityFromContext(r.Context()).KeyID
}

func cloneURLPath(r *http.Request, path string) *url.URL {
	if r == nil || r.URL == nil {
		return &url.URL{Path: path}
	}
	copyURL := *r.URL
	copyURL.Path = path
	return &copyURL
}

func hasCodexWebsocketPlan(plans []TransportPlan) bool {
	for _, plan := range plans {
		if plan.Mode == TransportModeCodexOAuthResponses {
			return true
		}
	}
	return false
}

func normalizeCodexWebsocketCreate(raw []byte, currentModel string, headers http.Header, inherited codexRequestFeatures) ([]byte, string, codexRequestFeatures, error) {
	var envelope map[string]any
	if err := decodeCodexJSON(raw, &envelope); err != nil {
		return nil, "", codexRequestFeatures{}, fmt.Errorf("invalid JSON websocket message")
	}
	if typ, _ := envelope["type"].(string); typ != "response.create" {
		return nil, "", codexRequestFeatures{}, fmt.Errorf("message type must be response.create")
	}
	if generate, exists := envelope["generate"]; exists {
		if _, ok := generate.(bool); !ok {
			return nil, "", codexRequestFeatures{}, fmt.Errorf("generate must be a boolean")
		}
	}
	model, _ := envelope["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		model = currentModel
	}
	if model == "" {
		return nil, "", codexRequestFeatures{}, fmt.Errorf("model is required on the first response.create")
	}
	envelope["model"] = model
	encoded, _ := json.Marshal(envelope)
	features, err := codexFeaturesFromHeaders(headers)
	if err != nil {
		return nil, "", codexRequestFeatures{}, err
	}
	features.ResponsesLite = features.ResponsesLite || rawCodexResponsesLite(encoded)
	if currentModel != "" {
		features.BetaFeatures = inherited.BetaFeatures
		features.ResponsesLite = features.ResponsesLite || inherited.ResponsesLite
		features.TurnState = inherited.TurnState
		if rawCodexInputHasCompactionTrigger(envelopeRawInput(envelope)) && !codexBetaFeaturePresent(features.BetaFeatures, codexRemoteCompactionV2Feature) {
			return nil, "", codexRequestFeatures{}, fmt.Errorf("compaction_trigger requires remote_compaction_v2 in the websocket session profile")
		}
	} else if rawCodexInputHasCompactionTrigger(envelopeRawInput(envelope)) {
		features.BetaFeatures = ensureCodexBetaFeature(features.BetaFeatures, codexRemoteCompactionV2Feature)
	}
	_, body, _, err := normalizeCodexRequestWithOptions(encoded, codexNormalizationOptions{
		allowPreviousID: currentModel != "", allowIncrementalOut: currentModel != "", responsesLite: features.ResponsesLite,
	})
	if err != nil {
		return nil, "", codexRequestFeatures{}, err
	}
	delete(body, "stream")
	body["type"] = "response.create"
	normalized, err := encodeCodexJSON(body)
	return normalized, model, features, err
}

func envelopeRawInput(envelope map[string]any) json.RawMessage {
	input, ok := envelope["input"]
	if !ok {
		return nil
	}
	raw, _ := json.Marshal(input)
	return raw
}

func websocketPromptCacheKey(raw []byte) string {
	var body map[string]any
	_ = decodeCodexJSON(raw, &body)
	value, _ := body["prompt_cache_key"].(string)
	return value
}

type codexWebsocketForwardResult struct {
	responseID        string
	terminal          bool
	reusable          bool
	downstreamWritten bool
	usage             tokenUsage
	failure           *codexresponses.Failure
	deferred          [][]byte
	outputItems       []json.RawMessage
}

func (h *Handler) forwardCodexWebsocketTurn(r *http.Request, conn *websocket.Conn, sessionID string, deferredLimit int64, started time.Time) (codexWebsocketForwardResult, error) {
	ctx := r.Context()
	result := codexWebsocketForwardResult{}
	writer, archiveErr := archiveRoundFromContext(ctx).CreateResponseWriter("response.ws.jsonl")
	if archiveErr != nil {
		return result, archiveErr
	}
	if writer != nil {
		defer writer.Close()
	}
	firstEvent := true
	collector := codexWebsocketOutputCollector{}
	deferredBytes := int64(0)
	for {
		update, err := h.codexResponses.PullCodexWebsocket(ctx, sessionID)
		if err != nil {
			result.outputItems = collector.result()
			return result, err
		}
		if update.Attempt.Request.URL != "" {
			var attemptErr error
			if update.Failure != nil {
				attemptErr = update.Failure
			}
			h.archiveCodexUpstreamAttempt(archiveRoundFromContext(ctx), r, effectivecatalog.CodexOAuthProviderID, update.Attempt, attemptErr)
		}
		payload := update.Payload
		if len(payload) > 0 {
			if firstEvent {
				recordFirstEventDuration(ctx, archiveRoundFromContext(ctx), time.Since(started))
				firstEvent = false
			}
			payload = normalizeCodexWebsocketEvent(payload)
			var identity struct {
				Type     string `json:"type"`
				Response struct {
					ID string `json:"id"`
				} `json:"response"`
			}
			if json.Unmarshal(payload, &identity) == nil && aetherrelaycodex.ObservationFeature(identity.Type) == "" && identity.Response.ID != "" {
				result.responseID = identity.Response.ID
			}
			result.usage = codexWebsocketUsage(payload, result.usage)
			collector.addEvent(payload)
			payload, _ = sanitizeCodexCapacityEventForClient(payload)
			if writer != nil {
				if _, err := writer.Write(append(append([]byte(nil), payload...), '\n')); err != nil {
					return result, err
				}
			}

			if update.Failure != nil && update.Failure.Kind == codexresponses.KindRateLimit && !result.downstreamWritten {
				result.failure = update.Failure
				result.terminal = true
				result.deferred = append(result.deferred, append([]byte(nil), payload...))
				result.outputItems = collector.result()
				return result, nil
			}
			if codexWebsocketNeutralPrelude(payload) && !result.downstreamWritten {
				result.deferred = append(result.deferred, append([]byte(nil), payload...))
				deferredBytes += int64(len(payload))
				if deferredLimit > 0 && deferredBytes > deferredLimit {
					if err := writeCodexWebsocketFrames(conn, result.deferred); err != nil {
						result.outputItems = collector.result()
						return result, err
					}
					result.deferred = nil
					result.downstreamWritten = true
				}
			} else {
				if len(result.deferred) > 0 {
					if err := writeCodexWebsocketFrames(conn, result.deferred); err != nil {
						result.outputItems = collector.result()
						return result, err
					}
					result.deferred = nil
				}
				if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
					result.outputItems = collector.result()
					return result, err
				}
				result.downstreamWritten = true
			}
			if terminal, reusable := codexWebsocketTerminalOutcome(payload); terminal {
				result.terminal, result.reusable, result.failure = true, reusable, update.Failure
				result.outputItems = collector.result()
				return result, nil
			}
		}
		if update.Failure != nil {
			result.failure = update.Failure
			result.outputItems = collector.result()
			if update.Failure.Kind == codexresponses.KindRateLimit && !result.downstreamWritten {
				result.terminal = true
				return result, nil
			}
			return result, update.Failure
		}
		if update.Done {
			if len(result.deferred) > 0 {
				if err := writeCodexWebsocketFrames(conn, result.deferred); err != nil {
					result.outputItems = collector.result()
					return result, err
				}
				result.deferred = nil
				result.downstreamWritten = true
			}
			result.outputItems = collector.result()
			return result, codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("Codex websocket stream ended without terminal event"))
		}
	}
}

func codexWebsocketNeutralPrelude(payload []byte) bool {
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return false
	}
	if aetherrelaycodex.ObservationFeature(event.Type) != "" {
		return true
	}
	switch event.Type {
	case "response.created", "response.in_progress", "response.queued":
		return true
	default:
		return false
	}
}

func writeCodexWebsocketFrames(conn *websocket.Conn, frames [][]byte) error {
	for _, payload := range frames {
		if len(payload) == 0 {
			continue
		}
		if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			return err
		}
	}
	return nil
}

func codexWebsocketTerminalOutcome(payload []byte) (bool, bool) {
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return false, false
	}
	switch event.Type {
	case "response.completed", "response.incomplete":
		return true, true
	case "response.failed", "response.cancelled", "response.canceled", "error":
		return true, false
	default:
		return false, false
	}
}

func codexWebsocketUsage(payload []byte, current tokenUsage) tokenUsage {
	var event struct {
		Type     string `json:"type"`
		Response struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return current
	}
	switch event.Type {
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		if usage, ok := usageFromRaw(event.Response.Usage); ok {
			return usage
		}
	}
	return current
}

func codexWebsocketTerminal(payload []byte) bool {
	terminal, _ := codexWebsocketTerminalOutcome(payload)
	return terminal
}

func normalizeCodexWebsocketEvent(payload []byte) []byte {
	var event map[string]any
	if decodeCodexJSON(payload, &event) != nil || event["type"] != "response.done" {
		return payload
	}
	event["type"] = "response.completed"
	normalized, err := json.Marshal(event)
	if err != nil {
		return payload
	}
	return normalized
}

func writeCodexWebsocketError(conn *websocket.Conn, code, message string) {
	payload := codexWebsocketErrorPayload(code, code, message, nil)
	_ = conn.WriteMessage(websocket.TextMessage, payload)
}

func writeCodexWebsocketFailure(conn *websocket.Conn, err error, fallbackCode, fallbackMessage string) {
	payload := codexWebsocketFailurePayload(err, fallbackCode, fallbackMessage)
	_ = conn.WriteMessage(websocket.TextMessage, payload)
}

func codexWebsocketFailurePayload(err error, fallbackCode, fallbackMessage string) []byte {
	errorType, code, message := fallbackCode, fallbackCode, fallbackMessage
	var retryable *bool
	param, retryAfter, status := "", 0, 0
	if failure, ok := codexresponses.AsFailure(err); ok && failure != nil {
		retryable = failure.Retryable
		retryAfter, status = failure.RetryAfterSeconds, failure.HTTPStatus
		switch failure.Kind {
		case codexresponses.KindRateLimit:
			errorType, code, message = "rate_limit_error", "rate_limit", "Codex account rate limit reached"
		case codexresponses.KindInvalidRequest, codexresponses.KindModelNotFound:
			errorType, code, message = "invalid_request_error", string(failure.Kind), "Codex request was rejected"
		}
		// These fields have passed the upstream safe-error projection. Never
		// expose arbitrary transport errors or raw response bodies to clients.
		if failure.Kind == codexresponses.KindRateLimit || failure.Kind == codexresponses.KindInvalidRequest || failure.Kind == codexresponses.KindModelNotFound {
			if failure.UpstreamType != "" {
				errorType = failure.UpstreamType
			}
			if failure.UpstreamCode != "" {
				code = failure.UpstreamCode
			}
			if failure.UpstreamMessage != "" {
				message = failure.UpstreamMessage
			}
			param = failure.UpstreamParam
		}
		if failure.UnavailableReason == "model_blocked" {
			errorType, code, message = "invalid_request_error", "model_not_found", "model is not available for the bound Codex account; reconnect to select another account"
		}
		if failure.Kind == codexresponses.KindAuthentication {
			errorType = "authentication_error"
			code = ErrorCodeUpstreamAuthRequired
			message = "Codex OAuth credentials require reauthentication"
			if retryable == nil {
				value := false
				retryable = &value
			}
		}
	}
	detail := map[string]any{"type": errorType, "code": code, "message": message}
	if retryable != nil {
		detail["retryable"] = *retryable
	}
	if param != "" {
		detail["param"] = param
	}
	if retryAfter > 0 {
		detail["retry_after_seconds"] = retryAfter
	}
	envelope := map[string]any{"type": "error", "error": detail}
	if status > 0 {
		envelope["status"] = status
	}
	payload, _ := json.Marshal(envelope)
	return payload
}

func codexWebsocketErrorPayload(errorType, code, message string, retryable *bool) []byte {
	detail := map[string]any{"type": errorType, "code": code, "message": message}
	if retryable != nil {
		detail["retryable"] = *retryable
	}
	payload, _ := json.Marshal(map[string]any{"type": "error", "error": detail})
	return payload
}
