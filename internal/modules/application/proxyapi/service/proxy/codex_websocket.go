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
			opened, openErr := h.codexResponses.OpenCodexWebsocket(ctx, codexresponses.WebsocketOpenRequest{Model: model, SessionHash: codexSessionHash(r, model, map[string]any{"prompt_cache_key": websocketPromptCacheKey(normalized)}), RemoteCompactionV2: features.RemoteCompactionV2, ResponsesLite: features.ResponsesLite})
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
		if err := h.codexResponses.SendCodexWebsocket(ctx, sessionID, normalized); err != nil {
			writeCodexWebsocketError(conn, "upstream_unavailable", "Codex websocket send failed")
			return
		}
		turnStarted := time.Now()
		terminal, reusable, turnUsage, forwardErr := h.forwardCodexWebsocketTurn(ctx, conn, sessionID)
		if forwardErr != nil {
			writeCodexWebsocketError(conn, "upstream_failed", forwardErr.Error())
			h.recordAndPrint(round, r, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(turnStarted), turnUsage, forwardErr.Error())
			h.writeArchiveMetadata(round, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(turnStarted), turnUsage, "websocket", forwardErr.Error(), "", "upstream_failed")
			return
		}
		if !terminal {
			return
		}
		outcome := "success"
		message := ""
		if !reusable {
			outcome = "upstream_failed"
			message = "Codex websocket turn failed"
		}
		h.recordAndPrint(round, r, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(turnStarted), turnUsage, message)
		h.writeArchiveMetadata(round, effectivecatalog.CodexOAuthProviderID, model, true, http.StatusSwitchingProtocols, time.Since(turnStarted), turnUsage, "websocket", message, "", outcome)
		if !reusable {
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
		features.RemoteCompactionV2 = features.RemoteCompactionV2 || inherited.RemoteCompactionV2
		features.ResponsesLite = features.ResponsesLite || inherited.ResponsesLite
	}
	normalized, body, _, err := normalizeCodexRequestWithOptions(encoded, codexNormalizationOptions{
		allowPreviousID: currentModel != "", allowIncrementalOut: currentModel != "", responsesLite: features.ResponsesLite,
	})
	if err != nil {
		return nil, "", codexRequestFeatures{}, err
	}
	delete(body, "stream")
	body["type"] = "response.create"
	normalized, err = json.Marshal(body)
	return normalized, model, features, err
}

func websocketPromptCacheKey(raw []byte) string {
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	value, _ := body["prompt_cache_key"].(string)
	return value
}

func (h *Handler) forwardCodexWebsocketTurn(ctx context.Context, conn *websocket.Conn, sessionID string) (bool, bool, tokenUsage, error) {
	usage := tokenUsage{Estimated: true, Known: true}
	for {
		payload, done, err := h.codexResponses.PullCodexWebsocket(ctx, sessionID)
		if err != nil {
			return false, false, usage, err
		}
		if len(payload) > 0 {
			payload = normalizeCodexWebsocketEvent(payload)
			usage = codexWebsocketUsage(payload, usage)
			payload, _ = sanitizeCodexCapacityEventForClient(payload)
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return false, false, usage, err
			}
			if terminal, reusable := codexWebsocketTerminalOutcome(payload); terminal {
				return true, reusable, usage, nil
			}
		}
		if done {
			return false, false, usage, nil
		}
	}
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
