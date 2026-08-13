package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/pkg/aetherrelayusage"
	"github.com/gorilla/websocket"
)

func TestCodexWebsocketAuthenticatesAndForwardsFirstTurn(t *testing.T) {
	var opened codexresponses.WebsocketOpenRequest
	var sent []byte
	closed := make(chan struct{}, 1)
	executor := codexResponsesExecutorStub{
		wsOpen: func(_ context.Context, request codexresponses.WebsocketOpenRequest) (codexresponses.WebsocketOpenResult, error) {
			opened = request
			return codexresponses.WebsocketOpenResult{SessionID: "session-1"}, nil
		},
		wsSend: func(_ context.Context, sessionID string, payload []byte) error {
			if sessionID != "session-1" {
				t.Errorf("session=%q", sessionID)
			}
			sent = append([]byte(nil), payload...)
			return nil
		},
		wsPull: func(context.Context, string) ([]byte, bool, error) {
			return []byte(`{"type":"response.completed","response":{"id":"resp_ws"}}`), false, nil
		},
		wsClose: func(context.Context, string) { closed <- struct{}{} },
	}
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), executor)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	headers := http.Header{"Authorization": []string{"Bearer test-client-key"}, "Session-Id": []string{"raw-session"}}
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	if err != nil {
		t.Fatalf("CP-WS-001 handshake status=%v err=%v", response, err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.2-codex","input":"hello","metadata":{"secret":"drop"}}`)); err != nil {
		t.Fatal(err)
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"type":"response.completed"`) {
		t.Fatalf("payload=%s", payload)
	}
	_ = conn.Close()
	if opened.Model != "gpt-5.2-codex" || opened.SessionHash == "" || strings.Contains(opened.SessionHash, "raw-session") {
		t.Fatalf("CP-WS-001/CP-SCHED-003 opened=%+v", opened)
	}
	var normalized map[string]any
	if err := json.Unmarshal(sent, &normalized); err != nil {
		t.Fatal(err)
	}
	if normalized["type"] != "response.create" || normalized["model"] != "gpt-5.2-codex" || normalized["metadata"] != nil || normalized["store"] != false {
		t.Fatalf("CP-REQ websocket normalized=%s", sent)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("CP-SCHED-005 websocket lease was not closed")
	}
}

func TestCodexWebsocketRejectsUnauthenticatedUpgrade(t *testing.T) {
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	_, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err == nil {
		t.Fatal("CP-WS-001 unauthenticated websocket was accepted")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("response=%v err=%v", response, err)
	}
}

func TestCodexWebsocketRejectsSessionAboveConfiguredLimit(t *testing.T) {
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{})
	cfg := handler.ConfigSnapshot()
	cfg.CodexOAuth.WebsocketMaxSessions = 1
	if err := handler.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	headers := http.Header{"Authorization": []string{"Bearer test-client-key"}}
	first, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	if err != nil {
		t.Fatalf("first handshake response=%v err=%v", response, err)
	}
	defer first.Close()
	_, response, err = websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("CP-WS-006 second handshake response=%v err=%v", response, err)
	}
}

func TestCodexWebsocketEnforcesMaximumLifetime(t *testing.T) {
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{})
	cfg := handler.ConfigSnapshot()
	cfg.CodexOAuth.WebsocketMaxLifetime = 20 * time.Millisecond
	cfg.CodexOAuth.WebsocketIdleTimeout = time.Second
	if err := handler.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	headers := http.Header{"Authorization": []string{"Bearer test-client-key"}}
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	if err != nil {
		t.Fatalf("handshake response=%v err=%v", response, err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("CP-WS-006 websocket exceeded configured maximum lifetime")
	}
}
