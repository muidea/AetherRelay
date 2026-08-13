package proxy

import (
	"bytes"
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
			return []byte(`{"type":"response.completed","response":{"id":"resp_ws","usage":{"input_tokens":1,"output_tokens":1}}}`), false, nil
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

func TestCodexWebsocketDoneNormalizesToCompletedTerminal(t *testing.T) {
	payload := normalizeCodexWebsocketEvent([]byte(`{"type":"response.done","response":{"id":"resp-1","usage":{"input_tokens":1,"output_tokens":1}}}`))
	if !codexWebsocketTerminal(payload) || !bytes.Contains(payload, []byte(`"type":"response.completed"`)) {
		t.Fatalf("normalized terminal=%s", payload)
	}
}

func TestCodexWebsocketFailureTerminalIsNotReusable(t *testing.T) {
	if terminal, reusable := codexWebsocketTerminalOutcome([]byte(`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`)); !terminal || reusable {
		t.Fatalf("CP-WS-010 failed terminal=%v reusable=%v", terminal, reusable)
	}
	if terminal, reusable := codexWebsocketTerminalOutcome([]byte(`{"type":"response.incomplete","response":{"status":"incomplete"}}`)); !terminal || !reusable {
		t.Fatalf("CP-STREAM-007 incomplete terminal=%v reusable=%v", terminal, reusable)
	}
}

func TestCodexWebsocketForwardsCustomNamespaceTools(t *testing.T) {
	var sent []byte
	executor := codexResponsesExecutorStub{
		wsOpen: func(context.Context, codexresponses.WebsocketOpenRequest) (codexresponses.WebsocketOpenResult, error) {
			return codexresponses.WebsocketOpenResult{SessionID: "session-tools"}, nil
		},
		wsSend: func(_ context.Context, _ string, payload []byte) error {
			sent = append([]byte(nil), payload...)
			return nil
		},
		wsPull: func(context.Context, string) ([]byte, bool, error) {
			return []byte(`{"type":"response.completed","response":{"id":"resp_ws_tools","usage":{"input_tokens":1,"output_tokens":1}}}`), false, nil
		},
	}
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), executor)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	headers := http.Header{"Authorization": []string{"Bearer test-client-key"}}
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	if err != nil {
		t.Fatalf("handshake response=%v err=%v", response, err)
	}
	defer conn.Close()
	request := `{"type":"response.create","model":"gpt-5.2-codex","parallel_tool_calls":true,"tools":[{"type":"custom","name":"exec"},{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent"}]}],"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"apply_patch"}]},{"type":"custom_tool_call","call_id":"call_exec","name":"exec","input":"pwd","namespace":"tools"},{"type":"custom_tool_call_output","call_id":"call_exec","output":"/tmp"}]}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sent), `"type":"namespace"`) || !strings.Contains(string(sent), `"type":"custom_tool_call"`) || !strings.Contains(string(sent), `"call_id":"call_exec"`) || !strings.Contains(string(sent), `"parallel_tool_calls":true`) {
		t.Fatalf("CP-REQ-008/011/023/024 websocket=%s", sent)
	}
}

func TestCodexWebsocketForwardsIncrementalToolContinuation(t *testing.T) {
	var sent [][]byte
	pulls := 0
	executor := codexResponsesExecutorStub{
		wsOpen: func(context.Context, codexresponses.WebsocketOpenRequest) (codexresponses.WebsocketOpenResult, error) {
			return codexresponses.WebsocketOpenResult{SessionID: "session-continuation"}, nil
		},
		wsSend: func(_ context.Context, _ string, payload []byte) error {
			sent = append(sent, append([]byte(nil), payload...))
			return nil
		},
		wsPull: func(context.Context, string) ([]byte, bool, error) {
			pulls++
			if pulls == 1 {
				return []byte(`{"type":"response.completed","response":{"id":"resp-1","usage":{"input_tokens":1,"output_tokens":1}}}`), false, nil
			}
			return []byte(`{"type":"response.completed","response":{"id":"resp-2","usage":{"input_tokens":1,"output_tokens":1}}}`), false, nil
		},
	}
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), executor)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	headers := http.Header{"Authorization": []string{"Bearer test-client-key"}}
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	if err != nil {
		t.Fatalf("handshake response=%v err=%v", response, err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.2-codex","tools":[{"type":"function","name":"lookup"}],"input":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","output":"done"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || !bytes.Contains(sent[1], []byte(`"previous_response_id":"resp-1"`)) || !bytes.Contains(sent[1], []byte(`"call_id":"call-1"`)) {
		t.Fatalf("CP-WS-007 sent=%q", sent)
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
