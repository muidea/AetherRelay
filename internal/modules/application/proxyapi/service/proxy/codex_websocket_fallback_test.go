package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
	"github.com/gorilla/websocket"
)

func TestWebsocketHTTPSendRejectionMigrationBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first         bool
		native        bool
		stale         bool
		kind          codexresponses.ErrorKind
		status        int
		rejections    int
		wantOpens     int
		wantSends     int
		wantCompleted bool
	}{
		{name: "later 429 migrates", kind: codexresponses.KindRateLimit, status: 429, rejections: 1, wantOpens: 2, wantSends: 3, wantCompleted: true},
		{name: "migration bounded at two", kind: codexresponses.KindRateLimit, status: 429, rejections: 10, wantOpens: 3, wantSends: 4},
		{name: "first turn does not migrate", first: true, kind: codexresponses.KindRateLimit, status: 429, rejections: 10, wantOpens: 1, wantSends: 1},
		{name: "unknown HTTP outcome does not migrate", kind: codexresponses.KindNetwork, rejections: 1, wantOpens: 1, wantSends: 2},
		{name: "native send does not migrate", native: true, kind: codexresponses.KindRateLimit, status: 429, rejections: 1, wantOpens: 1, wantSends: 2},
		{name: "stale previous ID rejected before send", stale: true, kind: codexresponses.KindRateLimit, status: 429, rejections: 1, wantOpens: 1, wantSends: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := usage.NewMemoryStore()
			opens, sends, rejected := 0, 0, 0
			var replay []byte
			executor := codexResponsesExecutorStub{
				wsOpen: func(context.Context, codexresponses.WebsocketOpenRequest) (codexresponses.WebsocketOpenResult, error) {
					opens++
					return codexresponses.WebsocketOpenResult{SessionID: fmt.Sprint(opens), HTTPFallback: !tc.native}, nil
				},
				wsSend: func(_ context.Context, _ string, payload []byte) error {
					sends++
					if (tc.first || sends > 1) && rejected < tc.rejections {
						rejected++
						failure := codexresponses.NewFailure(tc.kind, 60, nil)
						failure.HTTPStatus = tc.status
						return failure
					}
					if sends > 1 {
						replay = append([]byte(nil), payload...)
					}
					return nil
				},
				wsPull: func(context.Context, string) ([]byte, bool, error) {
					return []byte(`{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first answer"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`), false, nil
				},
				wsClose: func(context.Context, string) {},
			}
			done := make(chan struct{})
			handler := newCodexResponsesHandler(t, store, executor)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer test-client-key"}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			bodies := []string{`{"type":"response.create","model":"gpt-5.2-codex","input":"first context"}`}
			if !tc.first {
				previous := "resp_1"
				if tc.stale {
					previous = "resp_unknown"
				}
				bodies = append(bodies, fmt.Sprintf(`{"type":"response.create","previous_response_id":%q,"input":"second"}`, previous))
			}
			var last []byte
			for _, body := range bodies {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(body)); err != nil {
					t.Fatal(err)
				}
				_, last, err = conn.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
			}
			_ = conn.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler did not finish")
			}
			if opens != tc.wantOpens || sends != tc.wantSends || strings.Contains(string(last), `"response.completed"`) != tc.wantCompleted {
				t.Fatalf("opens=%d sends=%d last=%s", opens, sends, last)
			}
			if tc.wantCompleted && (!strings.Contains(string(replay), "first context") || !strings.Contains(string(replay), "first answer") || strings.Contains(string(replay), "previous_response_id")) {
				t.Fatalf("incomplete replay: %s", replay)
			}
			if !tc.wantCompleted && tc.status == 429 && !tc.stale {
				var payload struct {
					Status int `json:"status"`
					Error  struct {
						Code       string `json:"code"`
						RetryAfter int    `json:"retry_after_seconds"`
					} `json:"error"`
				}
				if json.Unmarshal(last, &payload) != nil || payload.Status != 429 || payload.Error.Code != "rate_limit" || payload.Error.RetryAfter != 60 {
					t.Fatalf("lost typed rate limit: %s", last)
				}
				page, err := store.Events(context.Background(), usage.EventFilter{UsageFilter: usage.UsageFilter{AllTime: true}, PageSize: 20})
				if err != nil || len(page.Events) == 0 {
					t.Fatalf("events=%+v err=%v", page, err)
				}
				found := false
				for _, entry := range page.Events {
					found = found || entry.ErrorCode == string(codexresponses.KindRateLimit)
				}
				if !found {
					t.Fatalf("rate-limit failure not recorded: %+v", page.Events)
				}
			}
		})
	}
}

func TestCodexWebsocketFailurePreservesSafeRejectionFields(t *testing.T) {
	retryable := true
	failure := &codexresponses.Failure{Kind: codexresponses.KindRateLimit, HTTPStatus: 429, RetryAfterSeconds: 42, Retryable: &retryable, UpstreamType: "rate_limit_error", UpstreamCode: "usage_limit_reached", UpstreamParam: "model", UpstreamMessage: "Please try again later"}
	var payload map[string]any
	if err := json.Unmarshal(codexWebsocketFailurePayload(failure, "upstream_failed", "failed"), &payload); err != nil {
		t.Fatal(err)
	}
	detail := payload["error"].(map[string]any)
	if payload["status"] != float64(429) || detail["type"] != failure.UpstreamType || detail["code"] != failure.UpstreamCode || detail["param"] != "model" || detail["message"] != failure.UpstreamMessage || detail["retryable"] != true || detail["retry_after_seconds"] != float64(42) {
		t.Fatalf("payload=%+v", payload)
	}
}

func TestNativeWebsocketUnknownHistoryCannotMigrate(t *testing.T) {
	for _, limitedTurn := range []int{2, 3} {
		t.Run(fmt.Sprint(limitedTurn), func(t *testing.T) {
			opens, turns := 0, 0
			executor := codexResponsesExecutorStub{
				wsOpen: func(context.Context, codexresponses.WebsocketOpenRequest) (codexresponses.WebsocketOpenResult, error) {
					opens++
					return codexresponses.WebsocketOpenResult{SessionID: "native"}, nil
				},
				wsSend: func(context.Context, string, []byte) error { turns++; return nil },
				wsPullUpdate: func(context.Context, string) (codexresponses.WebsocketUpdate, error) {
					if turns == limitedTurn {
						return codexresponses.WebsocketUpdate{Failure: codexresponses.NewFailure(codexresponses.KindRateLimit, 60, nil)}, nil
					}
					return codexresponses.WebsocketUpdate{Payload: []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`, turns))}, nil
				},
				wsClose: func(context.Context, string) {},
			}
			done := make(chan struct{})
			handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), executor)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer test-client-key"}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			bodies := []string{
				`{"type":"response.create","model":"gpt-5.2-codex","input":"first"}`,
				`{"type":"response.create","previous_response_id":"resp_external","input":"second"}`,
				`{"type":"response.create","previous_response_id":"resp_2","input":"third"}`,
			}
			var last []byte
			for _, body := range bodies[:limitedTurn] {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(body)); err != nil {
					t.Fatal(err)
				}
				_, last, err = conn.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
			}
			_ = conn.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler did not close")
			}
			if opens != 1 || turns != limitedTurn || !strings.Contains(string(last), `"rate_limit"`) {
				t.Fatalf("unknown history replayed: opens=%d turns=%d last=%s", opens, turns, last)
			}
		})
	}
}

func TestWebsocketHTTPFallbackRebuildsToolHistoryAndAccountsEveryCreate(t *testing.T) {
	store := usage.NewMemoryStore()
	sent := make(chan []byte, 2)
	closed := make(chan struct{})
	sends := 0
	executor := codexResponsesExecutorStub{
		wsOpen: func(context.Context, codexresponses.WebsocketOpenRequest) (codexresponses.WebsocketOpenResult, error) {
			return codexresponses.WebsocketOpenResult{SessionID: "http-fallback", HTTPFallback: true}, nil
		},
		wsSend: func(_ context.Context, _ string, payload []byte) error {
			sends++
			sent <- append([]byte(nil), payload...)
			return nil
		},
		wsPull: func(context.Context, string) ([]byte, bool, error) {
			if sends == 1 {
				return []byte(`{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}],"usage":{"input_tokens":3,"output_tokens":2}}}`), false, nil
			}
			return []byte(`{"type":"response.completed","response":{"id":"resp_2","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":4,"output_tokens":3}}}`), false, nil
		},
		wsClose: func(context.Context, string) { close(closed) },
	}
	handler := newCodexResponsesHandler(t, store, executor)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer test-client-key"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for _, body := range []string{
		`{"type":"response.create","model":"gpt-5.2-codex","input":"lookup"}`,
		`{"type":"response.create","previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"call_1","output":"result"}]}`,
	} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(body)); err != nil {
			t.Fatal(err)
		}
		_, payload, err := conn.ReadMessage()
		if err != nil || !strings.Contains(string(payload), `"response.completed"`) {
			t.Fatalf("payload=%s err=%v", payload, err)
		}
	}
	_ = conn.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("fallback session did not close")
	}
	<-sent
	second := <-sent
	var envelope struct {
		Previous string            `json:"previous_response_id"`
		Input    []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(second, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Previous != "" || len(envelope.Input) != 3 || !strings.Contains(string(envelope.Input[1]), `"function_call"`) {
		t.Fatalf("replay=%s", second)
	}
	page, err := store.Events(context.Background(), usage.EventFilter{UsageFilter: usage.UsageFilter{AllTime: true}, PageSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[0].EventID == page.Events[1].EventID {
		t.Fatalf("events=%+v", page.Events)
	}
	var inputs, outputs int64
	for _, event := range page.Events {
		if event.Outcome != "success" {
			t.Fatalf("event=%+v", event)
		}
		inputs += event.InputTokens
		outputs += event.OutputTokens
	}
	if inputs != 7 || outputs != 5 {
		t.Fatalf("tokens=%d/%d events=%+v", inputs, outputs, page.Events)
	}
}

func TestWebsocketHTTPFallbackRejectsUnknownPreviousResponse(t *testing.T) {
	state := newCodexWebsocketReplayState(1 << 20)
	first, err := state.prepare([]byte(`{"input":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	state.commit(first, nil)
	payload := []byte(`{"previous_response_id":"resp_stale","input":[{"role":"user","content":"next"}]}`)
	replay, err := state.prepare(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, safe, err := buildCodexWebsocketHTTPPayload(payload, replay, "resp_current", 1<<20); err != nil || safe {
		t.Fatalf("safe=%v err=%v", safe, err)
	}
	if _, safe, err := buildCodexWebsocketHTTPPayload(payload, replay, "resp_stale", 1<<20); err != nil || !safe {
		t.Fatalf("safe=%v err=%v", safe, err)
	}
}

func TestWebsocketHTTPFallbackRejectsIncrementalTextWithoutHistory(t *testing.T) {
	for _, evicted := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "evicted"}[evicted], func(t *testing.T) {
			state := newCodexWebsocketReplayState(256)
			if evicted {
				first, err := state.prepare([]byte(`{"input":[{"role":"user","content":"hello"}]}`))
				if err != nil {
					t.Fatal(err)
				}
				output := projectionJSON(projectionMessage("final_answer", strings.Repeat("x", 512)))
				state.commit(first, []json.RawMessage{output})
			}
			payload := []byte(`{"previous_response_id":"resp_current","input":[{"role":"user","content":"next"}]}`)
			replay, err := state.prepare(payload)
			if err != nil {
				t.Fatal(err)
			}
			if _, safe, err := buildCodexWebsocketHTTPPayload(payload, replay, "resp_current", 256); err != nil || safe {
				t.Fatalf("incomplete history accepted: safe=%v err=%v", safe, err)
			}
		})
	}
}
