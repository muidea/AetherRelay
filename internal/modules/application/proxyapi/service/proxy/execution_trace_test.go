package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/usagetarget"
	trace "aetherrelay/internal/pkg/aetherrelaytrace"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
	"github.com/gorilla/websocket"
)

func TestExecutionTraceRecoveredAccountAndCredentialAttempts(t *testing.T) {
	for _, switched := range []bool{false, true} {
		t.Run(map[bool]string{false: "credential-reexecution", true: "account-switch"}[switched], func(t *testing.T) {
			executor := codexResponsesExecutorStub{complete: func(ctx context.Context, req codexresponses.Request) (codexresponses.Result, error) {
				usagetarget.Record(ctx, "codexoauth", "slot-1", "first@example.com")
				usagetarget.BeginCodexAttempt(ctx, req.Body)
				first := codexArchiveTestAttempt()
				first.Response.Status = 401
				first.Response.Transport = "http"
				failure := codexresponses.NewFailure(codexresponses.KindAuthentication, 0, errors.New("invalid token"))
				req.ObserveAttempt(first, failure)
				if switched {
					usagetarget.Record(ctx, "codexoauth", "slot-2", "second@example.com")
				} else {
					trace.FromContext(ctx).Action("credential_refresh", "authentication")
				}
				usagetarget.BeginCodexAttempt(ctx, req.Body)
				last := codexArchiveTestAttempt()
				last.Request.At = time.Now()
				last.Response.Transport = "http"
				last.Response.ReadObserved = true
				last.Response.TerminalEvent = "response.completed"
				req.ObserveAttempt(last, nil) // initial headers and terminal must deduplicate
				last.Finished = true
				req.ObserveAttempt(last, nil)
				return codexresponses.Result{Body: []byte(`{"id":"response","object":"response","usage":{"input_tokens":2,"output_tokens":1}}`), Attempt: last}, nil
			}}
			h := newCodexResponsesHandler(t, usage.NewMemoryStore(), executor)
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.2-codex","input":"hi"}`))
			r.Header.Set("Authorization", "Bearer test-client-key")
			h.ServeHTTP(w, r)
			events := usageEvents(t, h.usageStore)
			if w.Code != 200 || len(events) != 1 {
				t.Fatalf("status=%d body=%s events=%+v", w.Code, w.Body.String(), events)
			}
			e := events[0]
			tr, err := h.usageStore.ExecutionTrace(context.Background(), e.EventID)
			if err != nil || tr == nil || len(tr.Attempts) != 2 || !tr.Attempts[0].Failed || tr.Attempts[1].Failed || e.AttemptCount != 2 || e.FailedAttempts != 1 || !e.HadAnomaly || e.Outcome != "success" {
				t.Fatalf("event=%+v trace=%+v err=%v", e, tr, err)
			}
			want := "slot-1"
			if switched {
				want = "slot-2"
			}
			if e.AccountID != want || tr.Attempts[1].AccountID != want || tr.Attempts[0].AccountID != "slot-1" {
				t.Fatalf("account attribution: %+v %+v", e, tr)
			}
		})
	}
}

type failingTraceStore struct{ usage.Store }

func (s failingTraceStore) WriteTrace(context.Context, string, trace.Trace) error {
	return errors.New("checkpoint unavailable")
}
func TestExecutionTraceCheckpointFailureDoesNotChangeResponse(t *testing.T) {
	s := failingTraceStore{usage.NewMemoryStore()}
	h := newCodexResponsesHandler(t, s.Store, codexResponsesExecutorStub{complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
		return codexresponses.Result{Body: []byte(`{"id":"ok","object":"response"}`), Attempt: codexArchiveTestAttempt()}, nil
	}})
	h.usageStore = s
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.2-codex","input":"hello"}`))
	r.Header.Set("Authorization", "Bearer test-client-key")
	h.ServeHTTP(w, r)
	events := usageEvents(t, s)
	if w.Code != 200 || len(events) != 1 || events[0].Outcome != "success" {
		t.Fatalf("%d %s %+v", w.Code, w.Body.String(), events)
	}
	tr, err := s.ExecutionTrace(context.Background(), events[0].EventID)
	if err != nil || tr == nil || !tr.SaveFailed {
		t.Fatalf("trace=%+v err=%v", tr, err)
	}
}

func TestExecutionTraceLocalRejectionHasNoUpstreamAttempt(t *testing.T) {
	h := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
		t.Fatal("local rejection executed")
		return codexresponses.Result{}, nil
	}})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":"hi"}`))
	r.Header.Set("Authorization", "Bearer test-client-key")
	h.ServeHTTP(w, r)
	events := usageEvents(t, h.usageStore)
	if w.Code != 400 || len(events) != 1 {
		t.Fatalf("status=%d %+v", w.Code, events)
	}
	tr, err := h.usageStore.ExecutionTrace(context.Background(), events[0].EventID)
	if err != nil || tr == nil || len(tr.Attempts) != 0 {
		t.Fatalf("invented execution: %+v %v", tr, err)
	}
}

func TestExecutionTraceNativeWebsocketTurnsAndHandshake(t *testing.T) {
	attempt := codexArchiveTestAttempt()
	attempt.Request.Method = http.MethodGet
	attempt.Response.Transport = "websocket"
	attempt.Response.Status = 101
	executor := codexResponsesExecutorStub{
		wsOpen: func(context.Context, codexresponses.WebsocketOpenRequest) (codexresponses.WebsocketOpenResult, error) {
			return codexresponses.WebsocketOpenResult{SessionID: "session", Attempt: attempt}, nil
		},
		wsSend: func(ctx context.Context, _ string, b []byte) error {
			usagetarget.Record(ctx, "codexoauth", "slot", "native@example.com")
			usagetarget.BeginCodexAttempt(ctx, b)
			return nil
		},
		wsPullUpdate: func(context.Context, string) (codexresponses.WebsocketUpdate, error) {
			a := attempt
			a.Request.Method = "WS"
			a.Response.WebsocketRequestSent = true
			a.Response.ReadObserved = true
			a.Response.TerminalEvent = "response.completed"
			return codexresponses.WebsocketUpdate{Attempt: a, Payload: []byte(`{"type":"response.completed","response":{"id":"done","usage":{"input_tokens":1,"output_tokens":1}}}`), Done: true}, nil
		},
	}
	h := newCodexResponsesHandler(t, usage.NewMemoryStore(), executor)
	server := httptest.NewServer(h)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer test-client-key"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.2-codex","input":"hi"}`)); err != nil {
			t.Fatal(err)
		}
		_, p, e := conn.ReadMessage()
		if e != nil || !bytes.Contains(p, []byte("response.completed")) {
			t.Fatalf("%s %v", p, e)
		}
	}
	_ = conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	var events []usage.Event
	for time.Now().Before(deadline) {
		events = usageEvents(t, h.usageStore)
		if len(events) == 2 && events[0].State == usage.StateCompleted && events[1].State == usage.StateCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(events) != 2 {
		t.Fatal(events)
	}
	handshakes := 0
	for _, e := range events {
		tr, err := h.usageStore.ExecutionTrace(context.Background(), e.EventID)
		if err != nil || tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Failed || e.Outcome != "success" {
			t.Fatalf("event=%+v trace=%+v err=%v", e, tr, err)
		}
		handshakes += len(tr.Attempts[0].Handshakes)
	}
	if handshakes != 1 {
		t.Fatalf("opening handshake should belong to first send only, got %d", handshakes)
	}
}
