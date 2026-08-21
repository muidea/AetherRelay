package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
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
	conn, err := codexWebsocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
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
	var sessionFeatures codexRequestFeatures
	var sessionOpenRequest codexresponses.WebsocketOpenRequest
	replayState := newCodexWebsocketReplayState(maxMessageBytes)
	turnNumber := 0
	defer func() {
		if sessionID != "" {
			h.codexResponses.CloseCodexWebsocket(context.WithoutCancel(ctx), sessionID)
		}
	}()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
		messageType, raw, readErr := conn.ReadMessage()
		if readErr != nil {
			return
		}
		if messageType != websocket.TextMessage {
			writeCodexWebsocketError(conn, "invalid_request", "only text response.create messages are supported")
			return
		}
		normalized, requestModel, features, normalizeErr := normalizeCodexWebsocketCreate(raw, model, r.Header, sessionFeatures)
		if normalizeErr != nil {
			writeCodexWebsocketError(conn, "invalid_request", normalizeErr.Error())
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
				writeCodexWebsocketError(conn, "model_not_found", message)
				return
			}
			sessionHash := codexSessionHash(r, model, map[string]any{"prompt_cache_key": websocketPromptCacheKey(normalized)})
			sessionOpenRequest = codexresponses.WebsocketOpenRequest{Model: model, SessionHash: sessionHash, BetaFeatures: features.BetaFeatures, ResponsesLite: features.ResponsesLite, TurnState: features.TurnState}
			opened, openErr := h.codexResponses.OpenCodexWebsocket(ctx, sessionOpenRequest)
			if openErr != nil {
				writeCodexWebsocketError(conn, "upstream_unavailable", "Codex websocket could not be opened")
				return
			}
			sessionID = opened.SessionID
		}
		if requestModel != model {
			writeCodexWebsocketError(conn, "invalid_request", "websocket model cannot change within a session")
			return
		}
		if features != sessionFeatures {
			writeCodexWebsocketError(conn, "invalid_request", "websocket feature profile cannot change within a session")
			return
		}
		var normalizedBody map[string]any
		if err := json.Unmarshal(normalized, &normalizedBody); err != nil {
			writeCodexWebsocketError(conn, "invalid_request", "invalid normalized response.create message")
			return
		}
		sessionHash := codexSessionHash(r, model, normalizedBody)
		var cacheErr error
		normalized, normalizedBody, cacheErr = ensureCodexPromptCacheKey(normalized, normalizedBody, sessionHash)
		if cacheErr != nil {
			writeCodexWebsocketError(conn, "invalid_request", cacheErr.Error())
			return
		}
		turnReplay, replayErr := replayState.prepare(normalized)
		if replayErr != nil {
			writeCodexWebsocketError(conn, "invalid_request", "Codex websocket replay state is invalid")
			return
		}
		turnNumber++
		turnStarted := time.Now()
		attemptPayload := normalized
		migrationAttempts := 0
		var turnResult codexWebsocketForwardResult
		for {
			if err := h.codexResponses.SendCodexWebsocket(ctx, sessionID, attemptPayload); err != nil {
				writeCodexWebsocketError(conn, "upstream_unavailable", "Codex websocket send failed")
				return
			}
			turnResult, err = h.forwardCodexWebsocketTurn(ctx, conn, sessionID, maxMessageBytes)
			if err != nil {
				writeCodexWebsocketError(conn, "upstream_failed", err.Error())
				h.recordAndPrint(round, r, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(turnStarted), turnResult.usage, err.Error())
				h.writeArchiveMetadata(round, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(turnStarted), turnResult.usage, "websocket", err.Error(), "", "upstream_failed")
				return
			}
			canMigrate := turnNumber > 1 && migrationAttempts < maxCodexWebsocketTurnMigrations && !turnResult.downstreamWritten && turnResult.failure != nil && turnResult.failure.Kind == codexresponses.KindRateLimit
			if canMigrate {
				retryPayload, retrySafe, retryErr := buildCodexWebsocketRetryPayload(attemptPayload, turnReplay, maxMessageBytes)
				if retryErr != nil {
					writeCodexWebsocketError(conn, "upstream_failed", "Codex websocket replay could not be prepared")
					return
				}
				if retrySafe {
					h.codexResponses.CloseCodexWebsocket(context.WithoutCancel(ctx), sessionID)
					sessionID = ""
					opened, openErr := h.codexResponses.OpenCodexWebsocket(ctx, sessionOpenRequest)
					if openErr != nil {
						writeCodexWebsocketError(conn, "upstream_unavailable", "Codex websocket replacement account is unavailable")
						return
					}
					sessionID = opened.SessionID
					attemptPayload = retryPayload
					migrationAttempts++
					continue
				}
			}
			if len(turnResult.deferred) > 0 {
				if err := writeCodexWebsocketFrames(conn, turnResult.deferred); err != nil {
					return
				}
				turnResult.downstreamWritten = true
			}
			if turnResult.failure != nil && !turnResult.downstreamWritten {
				writeCodexWebsocketError(conn, "upstream_failed", "Codex websocket turn failed")
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
		}
		h.recordAndPrint(round, r, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(turnStarted), turnResult.usage, message)
		h.writeArchiveMetadata(round, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(turnStarted), turnResult.usage, "websocket", message, "", outcome)
		if !turnResult.reusable {
			return
		}
	}
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
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, "", codexRequestFeatures{}, fmt.Errorf("invalid JSON websocket message")
	}
	if typ, _ := envelope["type"].(string); typ != "response.create" {
		return nil, "", codexRequestFeatures{}, fmt.Errorf("message type must be response.create")
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
	normalized, err := json.Marshal(body)
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
	_ = json.Unmarshal(raw, &body)
	value, _ := body["prompt_cache_key"].(string)
	return value
}

type codexWebsocketForwardResult struct {
	terminal          bool
	reusable          bool
	downstreamWritten bool
	usage             tokenUsage
	failure           *codexresponses.Failure
	deferred          [][]byte
	outputItems       []json.RawMessage
}

func (h *Handler) forwardCodexWebsocketTurn(ctx context.Context, conn *websocket.Conn, sessionID string, deferredLimit int64) (codexWebsocketForwardResult, error) {
	result := codexWebsocketForwardResult{usage: tokenUsage{Estimated: true, Known: true}}
	collector := codexWebsocketOutputCollector{}
	deferredBytes := int64(0)
	for {
		update, err := h.codexResponses.PullCodexWebsocket(ctx, sessionID)
		if err != nil {
			result.outputItems = collector.result()
			return result, err
		}
		payload := update.Payload
		if len(payload) > 0 {
			payload = normalizeCodexWebsocketEvent(payload)
			result.usage = codexWebsocketUsage(payload, result.usage)
			collector.addEvent(payload)
			payload, _ = sanitizeCodexCapacityEventForClient(payload)
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
			return result, nil
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
		Response struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &event) == nil && (event.Response.Usage.InputTokens > 0 || event.Response.Usage.OutputTokens > 0) {
		return tokenUsage{PromptTokens: event.Response.Usage.InputTokens, CompletionTokens: event.Response.Usage.OutputTokens, TotalTokens: event.Response.Usage.InputTokens + event.Response.Usage.OutputTokens, Known: true}
	}
	return current
}

func codexWebsocketTerminal(payload []byte) bool {
	terminal, _ := codexWebsocketTerminalOutcome(payload)
	return terminal
}

func normalizeCodexWebsocketEvent(payload []byte) []byte {
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil || event["type"] != "response.done" {
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
	payload, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": code, "code": code, "message": message}})
	_ = conn.WriteMessage(websocket.TextMessage, payload)
}
