package biz

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	accountproxy "aetherrelay/internal/pkg/aetherrelayproxy"
	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/gorilla/websocket"
	"github.com/muidea/magicCommon/event"
)

// Hold Err/Done open to deterministically reproduce the scheduler window in
// which a socket deadline has fired but the context's timer has not run yet.
type pendingDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c pendingDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func TestWebsocketDeadlineBeforeContextTimerDoesNotFallback(t *testing.T) {
	for _, flow := range []string{"responses", "native"} {
		t.Run(flow, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			var gets, posts atomic.Int32
			transportServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: " + transportCompleted + "\n\n"))
					return
				}
				gets.Add(1)
				<-release
			})
			upstream := transportUpstream(t)
			ctx := pendingDeadlineContext{Context: context.Background(), deadline: time.Now().Add(100 * time.Millisecond)}
			if flow == "responses" {
				response, attempt, class, _, err := performResponses(ctx, "test-token", "", "", []byte(`{"model":"gpt-test","input":[]}`), codexRequestProfile{}, true, 1024)
				if response != nil {
					_ = response.Body.Close()
				}
				if !errors.Is(err, context.DeadlineExceeded) || class != events.ErrorTimeout || response != nil || attempt.Response.FallbackReason != "" {
					t.Fatalf("expired handshake attempted another transport: class=%s attempt=%+v err=%v", class, attempt, err)
				}
			} else {
				result := event.NewResult(events.TopicWSOpen, "test", upstream.ID())
				upstream.handleWSOpen(event.NewEventWithContext(events.TopicWSOpen, "test", upstream.ID(), nil, ctx, events.WSOpenCommand{AccessToken: "test-token", AllowHTTPFallback: true}), result)
				opened, err := event.GetAs[events.WSOpenResult](result)
				if err != nil || opened.ErrorClass != events.ErrorTimeout || opened.SessionID != "" || opened.HTTPFallback {
					t.Fatalf("expired handshake created a fallback session: %+v err=%v", opened, err)
				}
			}
			if ctx.Err() != nil || gets.Load() != 1 || posts.Load() != 0 {
				t.Fatalf("deadline window not exercised or HTTP dispatched: err=%v gets=%d posts=%d", ctx.Err(), gets.Load(), posts.Load())
			}
		})
	}
}

func TestWebsocketFallbackChecksDeadlineWithoutSuppressingConnectTimeout(t *testing.T) {
	for _, opened := range []events.WSOpenResult{
		{ErrorClass: events.ErrorTimeout},
		{HTTPStatus: http.StatusUpgradeRequired, ErrorClass: events.ErrorUpstream},
		{HTTPStatus: http.StatusBadGateway, ErrorClass: events.ErrorUpstream},
	} {
		pending := pendingDeadlineContext{Context: context.Background(), deadline: time.Now().Add(-time.Second)}
		if reason := websocketFallbackReason(pending, opened); reason != "" {
			t.Fatalf("expired request allowed fallback: %s", reason)
		}
		pending.deadline = time.Now().Add(time.Minute)
		if reason := websocketFallbackReason(pending, opened); reason == "" {
			t.Fatal("handshake failure with remaining request budget lost fallback")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pending := pendingDeadlineContext{Context: ctx, deadline: time.Now().Add(-time.Second)}
	if !errors.Is(websocketContextError(pending), context.Canceled) {
		t.Fatal("explicit cancellation lost its classification")
	}
}

func TestWebsocketPendingHandshakeCancellation(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "shutdown", "rejection_body"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			var posts atomic.Int32
			transportServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					http.Error(w, "unexpected", 500)
					return
				}
				if mode == "rejection_body" {
					w.Header().Set("Content-Length", "1024")
					w.WriteHeader(http.StatusUpgradeRequired)
					w.(http.Flusher).Flush()
				}
				close(entered)
				<-release
				if mode != "rejection_body" {
					http.Error(w, "unsupported", 426)
				}
			})
			upstream := transportUpstream(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan struct{})
			result := event.NewResult(events.TopicWSOpen, "test", "upstream")
			var requestClass events.ErrorClass
			var requestErr error
			go func() {
				defer close(done)
				if mode == "shutdown" {
					upstream.commandHandler(upstream.handleWSOpen)(event.NewEventWithContext(events.TopicWSOpen, "test", "upstream", nil, context.Background(), events.WSOpenCommand{AccessToken: "test-token", AllowHTTPFallback: true}), result)
					return
				}
				response, _, class, _, err := performResponses(ctx, "test-token", "", "", []byte(`{"model":"gpt-test","input":[]}`), codexRequestProfile{}, true, 1024)
				requestClass, requestErr = class, err
				if response != nil {
					_ = response.Body.Close()
				}
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("handshake did not start")
			}
			if mode == "shutdown" {
				upstream.BeginShutdown(context.Background())
			} else if mode != "deadline" {
				cancel()
			} else {
				<-ctx.Done()
			}
			select {
			case <-done:
			case <-time.After(500 * time.Millisecond):
				t.Fatal("canceled handshake still waiting for response headers")
			}
			if mode == "shutdown" {
				opened, err := event.GetAs[events.WSOpenResult](result)
				if err != nil || opened.ErrorClass != events.ErrorCanceled || opened.SessionID != "" {
					t.Fatalf("shutdown opened a session: %+v err=%v", opened, err)
				}
				drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Second)
				defer drainCancel()
				if err := upstream.Quiesce(drainCtx); err != nil {
					t.Fatal(err)
				}
			} else {
				wantClass := events.ErrorCanceled
				if mode == "deadline" {
					wantClass = events.ErrorTimeout
				}
				if requestErr == nil || requestClass != wantClass {
					t.Fatalf("lost cancellation: class=%s err=%v", requestClass, requestErr)
				}
			}
			if posts.Load() != 0 {
				t.Fatal("canceled handshake started HTTP fallback")
			}
		})
	}
}

func TestWebsocketProxyCONNECTCancellation(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secure], func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect {
					t.Error("expected CONNECT")
				}
				close(entered)
				<-release
				w.WriteHeader(http.StatusBadGateway)
			}))
			if secure {
				proxy.StartTLS()
			} else {
				proxy.Start()
			}
			defer proxy.Close()
			defer close(release)
			dialer, err := newWebsocketDialer(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			if secure {
				roots := x509.NewCertPool()
				roots.AddCert(proxy.Certificate())
				proxyURL, _ := url.Parse(proxy.URL)
				dialer.NetDialTLSContext = accountproxy.HTTPSProxyDialTLSContext(proxyURL, &tls.Config{RootCAs: roots}, dialer.HandshakeTimeout, nil)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				releaseHandshake := guardWebsocketHandshake(ctx, dialer)
				defer releaseHandshake()
				conn, _, _ := dialer.DialContext(ctx, "ws://upstream.invalid/responses", fhttp.Header{})
				if conn != nil {
					_ = conn.Close()
				}
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("CONNECT did not start")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(500 * time.Millisecond):
				t.Fatal("canceled proxy CONNECT remained blocked")
			}
		})
	}
}

func TestWebsocketSuccessfulHandshakeDetachesCommandCancellation(t *testing.T) {
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(transportCompleted))
	})
	upstream := transportUpstream(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := event.NewResult(events.TopicWSOpen, "test", "upstream")
	upstream.commandHandler(upstream.handleWSOpen)(event.NewEventWithContext(events.TopicWSOpen, "test", "upstream", nil, ctx, events.WSOpenCommand{AccessToken: "test-token"}), result)
	opened, err := event.GetAs[events.WSOpenResult](result)
	if err != nil || opened.SessionID == "" {
		t.Fatalf("open=%+v err=%v", opened, err)
	}
	cancel()
	send := event.NewResult(events.TopicWSSend, "test", "upstream")
	upstream.handleWSSend(event.NewEvent(events.TopicWSSend, "test", "upstream", nil, events.WSSendCommand{SessionID: opened.SessionID, Payload: []byte(`{"type":"response.create","model":"gpt-test","input":"hello"}`)}), send)
	sent, err := event.GetAs[events.WSSendResult](send)
	if err != nil || !sent.Sent {
		t.Fatalf("send after command cancel=%+v err=%v", sent, err)
	}
	pull := event.NewResult(events.TopicWSPull, "test", "upstream")
	upstream.handleWSPull(event.NewEvent(events.TopicWSPull, "test", "upstream", nil, events.WSPullCommand{SessionID: opened.SessionID, TimeoutMillis: 2000}), pull)
	update, err := event.GetAs[events.WSPullResult](pull)
	if err != nil || !strings.Contains(string(update.Payload), "response.done") {
		t.Fatalf("session closed with open command: %+v err=%v", update, err)
	}
}
