package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/pkg/aetherrelayusage"
	"github.com/gorilla/websocket"
)

// CP-WS-012: replay must reconstruct a complete tool-call transcript.
func TestBuildCodexWebsocketRetryPayloadReconstructsToolContext(t *testing.T) {
	state := newCodexWebsocketReplayState(1 << 20)
	first, err := state.prepare([]byte(`{"type":"response.create","model":"gpt-5.6-sol","input":[{"role":"user","content":"first"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	state.commit(first, []json.RawMessage{
		json.RawMessage(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"first-ok"}]}`),
		json.RawMessage(`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"}`),
	})
	currentPayload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","previous_response_id":"resp_first","input":[{"type":"function_call_output","call_id":"call_1","output":"second"}]}`)
	current, err := state.prepare(currentPayload)
	if err != nil {
		t.Fatal(err)
	}
	retry, safe, err := buildCodexWebsocketRetryPayload(currentPayload, current, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !safe || len(retry) == 0 || bytes.Contains(retry, []byte("previous_response_id")) {
		t.Fatalf("safe=%v retry=%s", safe, retry)
	}
	var body struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(retry, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Input) != 4 || strings.Count(string(retry), `"id":"fc_1"`) != 1 || strings.Count(string(retry), `"call_id":"call_1"`) != 2 {
		t.Fatalf("retry input=%s", retry)
	}
}

// CP-WS-012: an orphan tool output makes cross-account replay unsafe.
func TestBuildCodexWebsocketRetryPayloadRejectsOrphanToolOutput(t *testing.T) {
	state := newCodexWebsocketReplayState(1 << 20)
	payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","previous_response_id":"resp_missing","input":[{"type":"function_call_output","call_id":"missing_call","output":"done"}]}`)
	turn, err := state.prepare(payload)
	if err != nil {
		t.Fatal(err)
	}
	retry, safe, err := buildCodexWebsocketRetryPayload(payload, turn, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if safe || retry != nil {
		t.Fatalf("safe=%v retry=%s", safe, retry)
	}
}

// CP-WS-012: a later pre-output 429 migrates with full bounded history.
func TestCodexWebsocketLaterTurnRateLimitMigratesWithFullReplay(t *testing.T) {
	var mu sync.Mutex
	openCount := 0
	pulls := map[string]int{}
	var sends []struct {
		session string
		payload []byte
	}
	closed := make(chan string, 4)
	executor := codexResponsesExecutorStub{
		wsOpen: func(context.Context, codexresponses.WebsocketOpenRequest) (codexresponses.WebsocketOpenResult, error) {
			mu.Lock()
			defer mu.Unlock()
			openCount++
			return codexresponses.WebsocketOpenResult{SessionID: "session-" + string(rune('0'+openCount))}, nil
		},
		wsSend: func(_ context.Context, sessionID string, payload []byte) error {
			mu.Lock()
			defer mu.Unlock()
			sends = append(sends, struct {
				session string
				payload []byte
			}{session: sessionID, payload: append([]byte(nil), payload...)})
			return nil
		},
		wsPullUpdate: func(_ context.Context, sessionID string) (codexresponses.WebsocketUpdate, error) {
			mu.Lock()
			pulls[sessionID]++
			pull := pulls[sessionID]
			mu.Unlock()
			switch {
			case sessionID == "session-1" && pull == 1:
				return codexresponses.WebsocketUpdate{Payload: []byte(`{"type":"response.completed","response":{"id":"resp_first","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"first-ok"}]},{"id":"fc_1","type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}}`)}, nil
			case sessionID == "session-1" && pull == 2:
				return codexresponses.WebsocketUpdate{Payload: []byte(`{"type":"response.created","response":{"id":"resp_limited"}}`)}, nil
			case sessionID == "session-1" && pull == 3:
				return codexresponses.WebsocketUpdate{Payload: []byte(`{"type":"response.failed","response":{"id":"resp_limited","error":{"type":"usage_limit_reached","message":"limited"}}}`), Failure: codexresponses.NewQuotaFailure(codexresponses.KindRateLimit, 60, true, "", nil)}, nil
			case sessionID == "session-2" && pull == 1:
				return codexresponses.WebsocketUpdate{Payload: []byte(`{"type":"response.completed","response":{"id":"resp_second","output":[{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"output_text","text":"second-ok"}]}],"usage":{"input_tokens":4,"output_tokens":1}}}`)}, nil
			default:
				return codexresponses.WebsocketUpdate{Done: true}, nil
			}
		},
		wsClose: func(_ context.Context, sessionID string) { closed <- sessionID },
	}
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), executor)
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer test-client-key"}})
	if err != nil {
		t.Fatalf("handshake response=%v err=%v", response, err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.2-codex","input":[{"role":"user","content":"first"}]}`)); err != nil {
		t.Fatal(err)
	}
	_, firstResponse, err := conn.ReadMessage()
	if err != nil || !bytes.Contains(firstResponse, []byte(`"id":"resp_first"`)) {
		t.Fatalf("first response=%s err=%v", firstResponse, err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","previous_response_id":"resp_first","input":[{"type":"function_call_output","call_id":"call_1","output":"second"}]}`)); err != nil {
		t.Fatal(err)
	}
	_, secondResponse, err := conn.ReadMessage()
	if err != nil || !bytes.Contains(secondResponse, []byte(`"id":"resp_second"`)) || bytes.Contains(secondResponse, []byte(`resp_limited`)) {
		t.Fatalf("second response=%s err=%v", secondResponse, err)
	}

	mu.Lock()
	gotOpenCount := openCount
	gotSends := append([]struct {
		session string
		payload []byte
	}(nil), sends...)
	mu.Unlock()
	if gotOpenCount != 2 || len(gotSends) != 3 || gotSends[2].session != "session-2" {
		t.Fatalf("opens=%d sends=%+v", gotOpenCount, gotSends)
	}
	retry := gotSends[2].payload
	if bytes.Contains(retry, []byte("previous_response_id")) || strings.Count(string(retry), `"call_id":"call_1"`) != 2 || !bytes.Contains(retry, []byte("first-ok")) || !bytes.Contains(retry, []byte("second")) {
		t.Fatalf("retry payload=%s", retry)
	}
	select {
	case session := <-closed:
		if session != "session-1" {
			t.Fatalf("first closed session=%q", session)
		}
	case <-time.After(time.Second):
		t.Fatal("rate-limited websocket was not closed before migration")
	}
}

// CP-WS-012: any downstream business output permanently closes migration.
func TestCodexWebsocketDoesNotMigrateAfterDownstreamOutput(t *testing.T) {
	var mu sync.Mutex
	openCount, pullCount := 0, 0
	executor := codexResponsesExecutorStub{
		wsOpen: func(context.Context, codexresponses.WebsocketOpenRequest) (codexresponses.WebsocketOpenResult, error) {
			mu.Lock()
			defer mu.Unlock()
			openCount++
			return codexresponses.WebsocketOpenResult{SessionID: "session"}, nil
		},
		wsPullUpdate: func(context.Context, string) (codexresponses.WebsocketUpdate, error) {
			mu.Lock()
			pullCount++
			pull := pullCount
			mu.Unlock()
			switch pull {
			case 1:
				return codexresponses.WebsocketUpdate{Payload: []byte(`{"type":"response.completed","response":{"id":"resp_first","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)}, nil
			case 2:
				return codexresponses.WebsocketUpdate{Payload: []byte(`{"type":"response.output_text.delta","delta":"partial"}`)}, nil
			default:
				return codexresponses.WebsocketUpdate{Payload: []byte(`{"type":"response.failed","response":{"error":{"type":"rate_limit_error","message":"limited"}}}`), Failure: codexresponses.NewFailure(codexresponses.KindRateLimit, 60, nil)}, nil
			}
		},
	}
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), executor)
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer test-client-key"}})
	if err != nil {
		t.Fatalf("handshake response=%v err=%v", response, err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.2-codex","input":"first"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","previous_response_id":"resp_first","input":"second"}`)); err != nil {
		t.Fatal(err)
	}
	_, delta, err := conn.ReadMessage()
	if err != nil || !bytes.Contains(delta, []byte("partial")) {
		t.Fatalf("delta=%s err=%v", delta, err)
	}
	_, failed, err := conn.ReadMessage()
	if err != nil || !bytes.Contains(failed, []byte(`"type":"response.failed"`)) {
		t.Fatalf("failed=%s err=%v", failed, err)
	}
	mu.Lock()
	gotOpenCount := openCount
	mu.Unlock()
	if gotOpenCount != 1 {
		t.Fatalf("open count=%d, want no migration after downstream output", gotOpenCount)
	}
}
