package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/usagetarget"
	archive "aetherrelay/internal/pkg/aetherrelayarchive"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
	"github.com/gorilla/websocket"
)

func TestCodexWebsocketHandshakeObservation(t *testing.T) {
	for _, archived := range []bool{false, true} {
		for _, broken := range []string{"Connection", "Upgrade", "Sec-WebSocket-Version", "Sec-WebSocket-Key", "hijacker"} {
			t.Run(fmt.Sprintf("archive=%v/%s", archived, broken), func(t *testing.T) {
				h, root := websocketObservationHandler(t, archived, codexResponsesExecutorStub{})
				r := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
				r.Header = http.Header{
					"Authorization": {"Bearer test-client-key"}, "Connection": {"Upgrade"},
					"Upgrade": {"websocket"}, "Sec-Websocket-Version": {"13"},
					"Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="},
				}
				if broken != "hijacker" {
					r.Header.Del(broken)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				wantStatus, wantCode := http.StatusBadRequest, ErrorCodeInvalidRequest
				if broken == "hijacker" {
					wantStatus, wantCode = http.StatusInternalServerError, ErrorCodeProxyInternalError
				}
				var body APIErrorResponse
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err, w.Body.String())
				}
				if w.Code != wantStatus || body.Error.Code != wantCode {
					t.Fatalf("CP-WS-018 actual status=%d body=%s", w.Code, w.Body)
				}
				e := onlyWebsocketEvent(t, h)
				assertWebsocketLocalObservation(t, e, wantStatus, wantCode)
				if e.Model != "" || e.Stream {
					t.Fatalf("unaccepted handshake contains generation fields: %+v", e)
				}
				assertWebsocketArchive(t, root, e)
			})
		}
	}
}

func TestCodexWebsocketUnusedSessionObservation(t *testing.T) {
	for _, archived := range []bool{false, true} {
		for _, scenario := range []string{"close", "idle", "lifetime", "oversize"} {
			t.Run(fmt.Sprintf("archive=%v/%s", archived, scenario), func(t *testing.T) {
				opens := 0
				h, root := websocketObservationHandler(t, archived, codexResponsesExecutorStub{
					wsOpen: func(context.Context, codexresponses.WebsocketOpenRequest) (codexresponses.WebsocketOpenResult, error) {
						opens++
						return codexresponses.WebsocketOpenResult{}, nil
					},
				})
				cfg := h.currentConfig()
				wantCode := "client_canceled"
				switch scenario {
				case "idle":
					cfg.CodexOAuth.WebsocketIdleTimeout = 100 * time.Millisecond
					wantCode = "idle_timeout"
				case "lifetime":
					cfg.CodexOAuth.WebsocketMaxLifetime = 100 * time.Millisecond
					wantCode = "request_timeout"
				case "oversize":
					cfg.CodexOAuth.WebsocketMaxMessageBytes = 64
					wantCode = ErrorCodeRequestTooLarge
				}
				h.UpdateConfig(cfg)
				conn, done := dialObservedWebsocket(t, h)
				if scenario == "close" {
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
					_ = conn.Close()
				} else {
					if scenario == "oversize" {
						if err := conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", 128))); err != nil {
							t.Fatal(err)
						}
					}
					// Deliberately do not read/respond to pings in the idle case.
					awaitWebsocketHandler(t, done)
				}
				awaitWebsocketHandler(t, done)
				e := onlyWebsocketEvent(t, h)
				assertWebsocketLocalObservation(t, e, http.StatusSwitchingProtocols, wantCode)
				if opens != 0 || e.Model != "" || !e.Stream || e.Outcome == "success" {
					t.Fatalf("unused session claimed generation: opens=%d event=%+v", opens, e)
				}
				assertWebsocketArchive(t, root, e)
			})
		}
	}
}

type disconnectedUpgradeWriter struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (w *disconnectedUpgradeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func TestCodexWebsocketHandshakeWriteFailureObservation(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(fmt.Sprint(archived), func(t *testing.T) {
			h, root := websocketObservationHandler(t, archived, codexResponsesExecutorStub{})
			conn, peer := net.Pipe()
			defer conn.Close()
			_ = peer.Close()
			w := &disconnectedUpgradeWriter{ResponseRecorder: httptest.NewRecorder(), conn: conn}
			r := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			r.Header = http.Header{
				"Authorization": {"Bearer test-client-key"}, "Connection": {"Upgrade"},
				"Upgrade": {"websocket"}, "Sec-Websocket-Version": {"13"},
				"Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="},
			}
			h.ServeHTTP(w, r)
			if w.Body.Len() != 0 {
				t.Fatalf("HTTP error written after hijack: %s", w.Body)
			}
			e := onlyWebsocketEvent(t, h)
			assertWebsocketLocalObservation(t, e, http.StatusSwitchingProtocols, "client_write")
			assertWebsocketArchive(t, root, e)
		})
	}
}

func TestCodexWebsocketLocalFrameObservation(t *testing.T) {
	for _, archived := range []bool{false, true} {
		for _, scenario := range []struct {
			name, payload, code string
			binary, priorTurn   bool
		}{
			{name: "invalid JSON", payload: `{`, code: "invalid_request"},
			{name: "binary", payload: `{}`, code: "invalid_request", binary: true},
			{name: "missing model", payload: `{"type":"response.create","input":"hello"}`, code: "invalid_request"},
			{name: "unknown model", payload: `{"type":"response.create","model":"unknown","input":"hello"}`, code: "model_not_found"},
			{name: "changed model", payload: `{"type":"response.create","model":"another-model","input":"hello"}`, code: "invalid_request", priorTurn: true},
			{name: "invalid second JSON", payload: `{`, code: "invalid_request", priorTurn: true},
		} {
			t.Run(fmt.Sprintf("archive=%v/%s", archived, scenario.name), func(t *testing.T) {
				sends := 0
				h, root := websocketObservationHandler(t, archived, codexResponsesExecutorStub{
					wsSend: func(ctx context.Context, _ string, payload []byte) error {
						sends++
						usagetarget.Record(ctx, "codexoauth", "first-account", "first-label")
						usagetarget.BeginCodexAttempt(ctx, payload)
						usagetarget.ObserveCodex(ctx, []byte(`{"object":"response","model":"first-response-model"}`))
						observeGenerationSSE(ctx, []byte(`data: {"type":"response.output_text.delta","delta":"first output"}`), nil)
						observeGenerationSSE(ctx, []byte(`data: {"type":"response.completed"}`), nil)
						return nil
					},
					wsPull: func(context.Context, string) ([]byte, bool, error) {
						return []byte(`{"type":"response.completed","response":{"id":"resp_first","usage":{"input_tokens":7,"output_tokens":4}}}`), false, nil
					},
				})
				conn, done := dialObservedWebsocket(t, h)
				if scenario.priorTurn {
					if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.2-codex","input":"hello"}`)); err != nil {
						t.Fatal(err)
					}
					if _, _, err := conn.ReadMessage(); err != nil {
						t.Fatal(err)
					}
				}
				kind := websocket.TextMessage
				if scenario.binary {
					kind = websocket.BinaryMessage
				}
				if err := conn.WriteMessage(kind, []byte(scenario.payload)); err != nil {
					t.Fatal(err)
				}
				_, raw, err := conn.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				var frame struct {
					Type  string   `json:"type"`
					Error APIError `json:"error"`
				}
				if err := json.Unmarshal(raw, &frame); err != nil || frame.Type != "error" || frame.Error.Code != scenario.code {
					t.Fatalf("frame=%s err=%v", raw, err)
				}
				awaitWebsocketHandler(t, done)
				events := usageEvents(t, h.usageStore)
				wantEvents := 1
				if scenario.priorTurn {
					wantEvents = 2
				}
				if len(events) != wantEvents || sends != wantEvents-1 {
					t.Fatalf("events=%+v sends=%d", events, sends)
				}
				for _, e := range events {
					if e.Outcome == "success" {
						if e.AccountID != "first-account" || e.OutputTokens != 4 || e.ResponseModel != "first-response-model" || e.FirstOutputAt == nil {
							t.Fatalf("first turn lost its observation: %+v", e)
						}
						continue
					}
					assertWebsocketLocalObservation(t, e, http.StatusSwitchingProtocols, scenario.code)
					assertWebsocketArchive(t, root, e)
				}
			})
		}
	}
}

func TestCodexWebsocketCompletedSessionHasNoEmptyFailure(t *testing.T) {
	for _, turns := range []int{1, 2} {
		t.Run(fmt.Sprint(turns), func(t *testing.T) {
			h, _ := websocketObservationHandler(t, true, codexResponsesExecutorStub{})
			conn, done := dialObservedWebsocket(t, h)
			for range turns {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.2-codex","input":"hello"}`)); err != nil {
					t.Fatal(err)
				}
				if _, _, err := conn.ReadMessage(); err != nil {
					t.Fatal(err)
				}
			}
			_ = conn.Close()
			awaitWebsocketHandler(t, done)
			events := usageEvents(t, h.usageStore)
			if len(events) != turns {
				t.Fatalf("closing a completed session added an empty event: %+v", events)
			}
			for _, e := range events {
				if e.Outcome != "success" || e.HTTPStatus != http.StatusSwitchingProtocols {
					t.Fatalf("completed turn changed after close: %+v", e)
				}
			}
		})
	}
}

func websocketObservationHandler(t *testing.T, archived bool, executor codexResponsesExecutorStub) (*Handler, string) {
	t.Helper()
	if archived {
		return newArchivedCodexResponsesHandler(t, executor)
	}
	return newCodexResponsesHandler(t, usage.NewMemoryStore(), executor), ""
}

func dialObservedWebsocket(t *testing.T, h *Handler) (*websocket.Conn, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer test-client-key"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); awaitWebsocketHandler(t, done) })
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	return conn, done
}

func awaitWebsocketHandler(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("websocket handler did not finish")
	}
}

func onlyWebsocketEvent(t *testing.T, h *Handler) usage.Event {
	t.Helper()
	events := usageEvents(t, h.usageStore)
	if len(events) != 1 {
		t.Fatalf("expected one completed event: %+v", events)
	}
	return events[0]
}

func assertWebsocketLocalObservation(t *testing.T, e usage.Event, status int, code string) {
	t.Helper()
	if e.State != "completed" || e.HTTPStatus != status || e.ErrorCode != code || e.ClientEndpoint != "/v1/responses" || e.Operation != "responses" || e.ClientProtocol != "openai" {
		t.Fatalf("CP-WS-018 incorrect local settlement: %+v", e)
	}
	if e.Provider != "" || e.AccountID != "" || e.AccountLabel != "" || e.UpstreamStatus != 0 || e.UpstreamRequestModel != "" || e.ResponseModel != "" || e.TotalTokens != 0 || e.OutputTokensKnown || e.FirstOutputAt != nil || e.GenerationPartial {
		t.Fatalf("CP-WS-018 fabricated/inherited upstream or output evidence: %+v", e)
	}
}

func assertWebsocketArchive(t *testing.T, root string, e usage.Event) {
	t.Helper()
	if root == "" {
		return
	}
	paths, err := filepath.Glob(filepath.Join(root, "*", "*", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var meta archive.Metadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatal(err)
		}
		if meta.EventID != e.EventID {
			continue
		}
		if meta.HTTPStatus != e.HTTPStatus || meta.ErrorCode != e.ErrorCode || meta.Outcome != e.Outcome || meta.Provider != e.Provider || meta.AccountID != e.AccountID || meta.ResponseModel != e.ResponseModel {
			t.Fatalf("archive and usage disagree: meta=%+v event=%+v", meta, e)
		}
		names := []string{"request.meta.json"}
		// Only the first round owns an HTTP handshake response. Subsequent
		// turns record their result in metadata and WS frames on that connection.
		if e.RoundID == 1 {
			names = append(names, "response.meta.json")
		}
		for _, name := range names {
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), name)); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	t.Fatalf("missing completed archive for %s in %s", e.EventID, root)
}
