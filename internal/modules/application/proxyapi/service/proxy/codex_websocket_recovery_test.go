package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
	"github.com/gorilla/websocket"
)

func TestCodexWebsocketRealAttemptsForwardWithoutFailure(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(fmt.Sprint(archived), func(t *testing.T) {
			executor := codexResponsesExecutorStub{wsPullUpdate: func(context.Context, string) (codexresponses.WebsocketUpdate, error) {
				return codexresponses.WebsocketUpdate{Attempt: codexArchiveTestAttempt(), Payload: []byte(`{"type":"response.completed","response":{"id":"resp_ok","usage":{"input_tokens":1,"output_tokens":2}}}`)}, nil
			}}
			h, root := websocketObservationHandler(t, archived, executor)
			conn, done := dialObservedWebsocket(t, h)
			for range 2 {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.2-codex","input":"hello"}`)); err != nil {
					t.Fatal(err)
				}
				if _, payload, err := conn.ReadMessage(); err != nil || !json.Valid(payload) {
					t.Fatalf("payload=%s err=%v", payload, err)
				}
			}
			_ = conn.Close()
			awaitWebsocketHandler(t, done)
			events := usageEvents(t, h.usageStore)
			if len(events) != 2 {
				t.Fatalf("events=%+v", events)
			}
			for _, e := range events {
				if e.Outcome != "success" || e.OutputTokens != 2 {
					t.Fatalf("event=%+v", e)
				}
				assertWebsocketArchive(t, root, e)
			}
			if archived {
				paths, _ := filepath.Glob(filepath.Join(root, "*", "*", "upstream_response.json"))
				if len(paths) != 2 {
					t.Fatalf("upstream archives=%v", paths)
				}
				for _, path := range paths {
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var response upstreamResponseDebugInfo
					if err := json.Unmarshal(data, &response); err != nil {
						t.Fatal(err)
					}
					if response.Error != "" || response.FailureClass != "" {
						t.Fatalf("normal attempt marked failed: %s", data)
					}
				}
			}
		})
	}
}

func TestCodexWebsocketPanicSettlesActiveTurnAndCloses(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, archived := range []bool{false, true} {
		for _, panicTurn := range []int{1, 2} {
			t.Run(fmt.Sprintf("archive=%v/turn=%d", archived, panicTurn), func(t *testing.T) {
				turn, closes := 0, 0
				executor := codexResponsesExecutorStub{
					wsSend: func(context.Context, string, []byte) error { turn++; return nil },
					wsPullUpdate: func(context.Context, string) (codexresponses.WebsocketUpdate, error) {
						if turn == panicTurn {
							panic("private-upstream-token")
						}
						return codexresponses.WebsocketUpdate{Payload: []byte(`{"type":"response.completed","response":{"id":"resp_ok","usage":{"output_tokens":2}}}`)}, nil
					},
					wsClose: func(context.Context, string) { closes++ },
				}
				h, root := websocketObservationHandler(t, archived, executor)
				store := &countingSettlementStore{Store: h.usageStore}
				h.usageStore = store
				conn, done := dialObservedWebsocket(t, h)
				for i := 1; i <= panicTurn; i++ {
					if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.2-codex","input":"hello"}`)); err != nil {
						t.Fatal(err)
					}
					_, _, err := conn.ReadMessage()
					if i == panicTurn {
						if !websocket.IsCloseError(err, websocket.CloseInternalServerErr) {
							t.Fatalf("close error=%v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				}
				awaitWebsocketHandler(t, done)
				if store.completions.Load() != int32(panicTurn) {
					t.Fatalf("settlement calls=%d", store.completions.Load())
				}
				if !strings.Contains(logs.String(), "private-upstream-token") {
					t.Fatal("panic diagnostic lost original value")
				}
				if closes != 1 {
					t.Fatalf("upstream closes=%d", closes)
				}
				events := usageEvents(t, h.usageStore)
				if len(events) != panicTurn {
					t.Fatalf("events=%+v", events)
				}
				failed := 0
				for _, e := range events {
					if e.Outcome != "success" {
						failed++
						if e.ErrorCode != ErrorCodeProxyInternalError || e.HTTPStatus != http.StatusSwitchingProtocols || e.State != "completed" {
							t.Fatalf("event=%+v", e)
						}
					}
					assertWebsocketArchive(t, root, e)
				}
				if failed != 1 {
					t.Fatalf("failed turns=%d", failed)
				}
			})
		}
	}
}

type countingSettlementStore struct {
	usage.Store
	completions atomic.Int32
}

func (s *countingSettlementStore) Complete(ctx context.Context, record usage.CompleteRecord) error {
	s.completions.Add(1)
	return s.Store.Complete(ctx, record)
}

func TestPendingUsageSettlesOnlyOnceAcrossCleanupPaths(t *testing.T) {
	store := &countingSettlementStore{Store: usage.NewMemoryStore()}
	defer store.Close()
	if err := store.Start(context.Background(), usage.StartRecord{EventID: "pending", APIKeyID: "client"}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{usageStore: store}
	r := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	r = r.WithContext(withUsageCompletion(withUsageEventID(r.Context(), "pending"), &usageCompletion{}))
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { h.completePendingUsage(r, nil) })
	}
	wg.Wait()
	if store.completions.Load() != 1 {
		t.Fatalf("fallback completions=%d", store.completions.Load())
	}
}
