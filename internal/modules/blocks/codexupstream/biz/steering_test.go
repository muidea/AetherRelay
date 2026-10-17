package biz

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"aetherrelay/internal/modules/blocks/codexupstream/pkg/steering"
	"github.com/gorilla/websocket"
	"github.com/muidea/magicCommon/event"
)

func startSteeringTest(t *testing.T, s *Upstream, deadline time.Time) {
	t.Helper()
	r := event.NewResult(events.TopicSteeringStart, "test", s.ID())
	s.handleSteeringStart(event.NewEvent(events.TopicSteeringStart, "test", s.ID(), nil, events.SteeringStartCommand{ProbeID: "probe_test", Model: "gpt-test", AccessToken: "test-token", SessionHash: "steering-test-session", Deadline: deadline}), r)
	if _, err := r.Get(); err != nil {
		t.Fatal(err)
	}
}
func closeSteeringTest(t *testing.T, s *Upstream) events.SteeringCloseResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := event.NewResult(events.TopicSteeringClose, "test", s.ID())
	s.handleSteeringClose(event.NewEventWithContext(events.TopicSteeringClose, "test", s.ID(), nil, ctx, events.SteeringCloseCommand{ProbeID: "probe_test"}), r)
	v, err := r.Get()
	if err != nil {
		t.Fatal(err)
	}
	return v.(events.SteeringCloseResult)
}

func TestSteeringWSProtocolAndCleanup(t *testing.T) {
	for _, mode := range []string{"completed", "pending", "upstream_error", "native_inflight", "disconnect", "oversize", "cumulative", "compressed", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			wire := make(chan []byte, 1)
			ready := make(chan struct{}, 1)
			transportServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("unexpected HTTP fallback")
					http.Error(w, "unexpected", 500)
					return
				}
				conn, err := (&websocket.Upgrader{EnableCompression: true}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if strings.HasSuffix(r.Header.Get("Session-Id"), ":client") {
					serveClientSteering(t, conn, r, "completed")
					return
				}
				if mode == "compressed" {
					conn.EnableWriteCompression(true)
				}
				_, create, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var c struct {
					Type, Model string
					Store       bool
					Metadata    map[string]string `json:"client_metadata"`
				}
				if json.Unmarshal(create, &c) != nil || c.Type != "response.create" || c.Model != "gpt-test" || c.Store {
					t.Error("invalid fixed create")
				}
				// Fingerprint off still uses the probe's synthetic session in
				// both the handshake and create body, without production state.
				if session := r.Header.Get("Session-Id"); session != "steering-test-session" || c.Metadata["session_id"] != session || c.Metadata["thread_id"] != r.Header.Get("Thread-Id") || c.Metadata["x-codex-window-id"] != r.Header.Get("X-Codex-Window-Id") {
					t.Error("probe create identity differs from handshake")
				}
				if mode == "native_inflight" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"codex.rate_limits"}`))
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"codex.response.metadata"}`))
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_parent","model":"gpt-test"}}`))
				_, steer, err := conn.ReadMessage()
				if err != nil {
					return
				}
				wire <- steer
				if mode == "native_inflight" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.in_progress"}`))
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_native_inflight_message","message":"The experimental native turn lane cannot accept stateful WebSocket messages while a native turn is running. Start a new independent response.create turn instead."}}`))
					// The client must terminate, never send an independent create
					// or replay the rejected steer on this connection.
					if _, next, err := conn.ReadMessage(); err == nil {
						t.Errorf("unexpected replay: %s", next)
					}
					return
				}
				if mode == "upstream_error" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"future_rejection","type":"future_error","param":"input","message":"this profile was rejected"}}`))
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.steer.accepted","steer":{"id":"steer_one","previous_response_id":"resp_parent"}}`))
				if mode == "cancel" {
					ready <- struct{}{}
					_, _, _ = conn.ReadMessage()
					return
				}
				if mode == "disconnect" {
					return
				}
				if mode == "oversize" || mode == "compressed" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.metadata","padding":"`+strings.Repeat("x", (1<<20)+1)+`"}`))
					return
				}
				if mode == "cumulative" {
					for i := 0; i < 10; i++ {
						if conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.metadata","padding":"`+strings.Repeat("x", (1<<20)-100)+`"}`)) != nil {
							return
						}
					}
					return
				}
				if mode == "pending" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.steer.pending","steer":{"id":"steer_one","previous_response_id":"resp_parent"},"required_input":[{"type":"function_call_output"}]}`))
					return
				}
				for _, payload := range []string{`{"type":"response.completed","response":{"id":"resp_parent","usage":{"input_tokens":1,"output_tokens":1}}}`, `{"type":"responsesapi.websocket_timing","output_tokens":999}`, `{"type":"response.created","response":{"id":"resp_next"}}`, `{"type":"response.completed","response":{"id":"resp_next","usage":{"input_tokens":3,"output_tokens":2}}}`} {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(payload))
				}
			})
			s := transportUpstream(t)
			startSteeringTest(t, s, time.Now().Add(15*time.Second))
			select {
			case b := <-wire:
				var fields map[string]json.RawMessage
				if json.Unmarshal(b, &fields) != nil || len(fields) != 3 || fields["client_metadata"] != nil {
					t.Fatalf("invalid actual steer: %s", b)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no steer")
			}
			p := s.steeringSession("probe_test")
			if mode == "cancel" {
				<-ready
			} else {
				select {
				case <-p.done:
				case <-time.After(10 * time.Second):
					t.Fatal("probe did not finish")
				}
			}
			closed := closeSteeringTest(t, s)
			e := closed.Snapshot.Evidence
			if !closed.Closed || !e.Done || s.steeringSession("probe_test") != nil {
				t.Fatal("probe leaked")
			}
			f := closed.Snapshot.Fallback
			switch mode {
			case "native_inflight", "upstream_error", "disconnect":
				if f == nil || e.Capability != steering.Supported || e.CapabilityTarget != steering.ClientTarget || e.SteerSent || !e.ContinuationWriteCompleted || !f.SteerSent || !f.SteerWriteCompleted {
					t.Fatal("fallback failed", e, f)
				}
				if mode == "native_inflight" && (f.ErrorCode != "unsupported_native_inflight_message" || f.UpstreamError == nil || f.UpstreamError.Code != f.ErrorCode || f.EventCount != 5 || len(f.Timeline) != 11 || f.Timeline[10].Type != "probe.finished") {
					t.Fatal("native diagnostic lost", f)
				}
				if mode == "upstream_error" && (f.ErrorCode != "upstream_error" || f.UpstreamError == nil || f.UpstreamError.Code != "future_rejection" || f.UpstreamError.Message != "this profile was rejected") {
					t.Fatal("unknown diagnostic lost", f)
				}
			default:
				if f != nil || e.CapabilityTarget != steering.NativeTarget || !e.CreateWriteCompleted || !e.SteerWriteCompleted {
					t.Fatal("unexpected fallback", e, f)
				}
				switch mode {
				case "completed":
					if e.Capability != steering.Supported || e.ExecutionOutcome != "completed" || e.ParentUsage == nil || e.SuccessorUsage == nil {
						t.Fatal(e)
					}
				case "pending":
					if e.Capability != steering.Pending || e.ExecutionOutcome != "waiting_for_required_input" {
						t.Fatal(e)
					}
				case "cancel":
					if e.Capability != steering.Unknown || e.ErrorCode != "canceled" {
						t.Fatal(e)
					}
				default:
					if e.Capability != steering.Unknown || e.ErrorCode != "receive_limit" {
						t.Fatal(e)
					}
				}
			}
		})
	}
}

func TestSteeringHandshakeFailureDoesNotFallback(t *testing.T) {
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("HTTP replay")
		}
		http.Error(w, "forbidden", 403)
	})
	s := transportUpstream(t)
	startSteeringTest(t, s, time.Now().Add(time.Second))
	p := s.steeringSession("probe_test")
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Fatal("handshake stuck")
	}
	e := closeSteeringTest(t, s).Snapshot.Evidence
	if e.HTTPStatus != 403 || e.AuthState == "rejected" || e.CreateSent || e.Capability != steering.Unknown {
		t.Fatal(e)
	}
	if e.UpstreamError == nil || e.UpstreamError.EventType != "websocket.handshake" || e.UpstreamError.Status != 403 || e.CreateWriteCompleted {
		t.Fatal("handshake diagnostics lost", e)
	}
}

func TestSteeringSnapshotDiagnosticsAreIndependent(t *testing.T) {
	p := &steeringSession{}
	m := steering.NewMachine()
	m.RecordUpstreamError(steering.UpstreamError{EventType: "error", Code: "original"})
	m.Trace("receive", "error", "")
	p.publish(m.Evidence)
	m.UpstreamError.Code = "writer mutation"
	m.Timeline[0].Type = "writer mutation"
	v := p.read()
	if v.Evidence.UpstreamError.Code != "original" || v.Evidence.Timeline[0].Type != "error" {
		t.Fatal(v)
	}
	v.Evidence.UpstreamError.Code = "reader mutation"
	v.Evidence.Timeline[0].Type = "reader mutation"
	if next := p.read(); next.Evidence.UpstreamError.Code != "original" || next.Evidence.Timeline[0].Type != "error" {
		t.Fatal(next)
	}
	// Race verification: the event reader keeps rolling its own timeline
	// while management readers receive, and may mutate, independent copies.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			m.Trace("receive", "response.metadata", "")
			p.publish(m.Evidence)
		}
	}()
	for range 200 {
		v := p.read()
		v.Evidence.UpstreamError.Code = "concurrent reader mutation"
		v.Evidence.Timeline[0].Type = "concurrent reader mutation"
	}
	<-done
}

func TestSteeringGatewayTimeoutKeepsHandshakeDiagnostic(t *testing.T) {
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("unexpected fallback")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte(`{"error":{"code":"gateway_timeout","message":"upstream timed out"}}`))
	})
	s := transportUpstream(t)
	startSteeringTest(t, s, time.Now().Add(2*time.Second))
	select {
	case <-s.steeringSession("probe_test").done:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not finish")
	}
	e := closeSteeringTest(t, s).Snapshot.Evidence
	if e.HTTPStatus != 504 || e.UpstreamError == nil || e.UpstreamError.EventType != "websocket.handshake" || e.UpstreamError.Status != 504 || e.CreateSent {
		t.Fatal("timeout classification discarded its HTTP error", e)
	}
}

func TestSteeringCloseRemainsAvailableDuringShutdown(t *testing.T) {
	ready := make(chan struct{})
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		close(ready)
		_, _, _ = conn.ReadMessage()
	})
	s := transportUpstream(t)
	startSteeringTest(t, s, time.Now().Add(10*time.Second))
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("create not sent")
	}
	if err := s.BeginShutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	v, err := s.SendEvent(events.BindCommandLane(event.NewEventWithContext(events.TopicSteeringClose, "test", s.ID(), nil, ctx, events.SteeringCloseCommand{ProbeID: "probe_test"}))).Get()
	if err != nil || !v.(events.SteeringCloseResult).Closed {
		t.Fatal("shutdown blocked probe cleanup", err)
	}
	if err := s.Quiesce(ctx); err != nil {
		t.Fatal(err)
	}
}

func serveClientSteering(t *testing.T, conn *websocket.Conn, r *http.Request, mode string) {
	t.Helper()
	_, initial, err := conn.ReadMessage()
	if err != nil {
		return
	}
	var first map[string]any
	if json.Unmarshal(initial, &first) != nil || first["type"] != "response.create" || first["previous_response_id"] != nil {
		t.Error("invalid client initial create")
		return
	}
	metadata, _ := first["client_metadata"].(map[string]any)
	if metadata["session_id"] != r.Header.Get("Session-Id") || metadata["thread_id"] != r.Header.Get("Thread-Id") {
		t.Error("client identity differs from handshake")
	}
	_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_client_parent"}}`))
	next := make(chan []byte, 1)
	go func() { _, b, _ := conn.ReadMessage(); next <- b }()
	select {
	case b := <-next:
		t.Errorf("client sent before parent terminal: %s", b)
		return
	case <-time.After(15 * time.Millisecond):
	}
	terminal := `{"type":"response.completed","response":{"id":"resp_client_parent","usage":{"input_tokens":4,"output_tokens":2}}}`
	if mode == "pending" {
		terminal = `{"type":"response.completed","response":{"id":"resp_client_parent","output":[{"type":"function_call"}]}}`
	}
	_ = conn.WriteMessage(websocket.TextMessage, []byte(terminal))
	var b []byte
	select {
	case b = <-next:
	case <-time.After(2 * time.Second):
		t.Error("no client continuation or close")
		return
	}
	if mode == "pending" {
		if len(b) > 0 {
			t.Errorf("continued through required tool input: %s", b)
		}
		return
	}
	var continuation map[string]any
	if json.Unmarshal(b, &continuation) != nil || continuation["type"] != "response.create" || continuation["previous_response_id"] != "resp_client_parent" || continuation["model"] != first["model"] || continuation["instructions"] != first["instructions"] || continuation["store"] != false {
		t.Errorf("invalid client continuation: %s", b)
		return
	}
	a, _ := json.Marshal(first["client_metadata"])
	c, _ := json.Marshal(continuation["client_metadata"])
	if string(a) != string(c) {
		t.Error("client continuation identity changed")
	}
	if mode == "rejected" {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"unsupported_native_inflight_message","message":"stateful request rejected"}}`))
		return
	}
	for _, payload := range []string{`{"type":"response.created","response":{"id":"resp_client_next","previous_response_id":"resp_client_parent"}}`, `{"type":"response.completed","response":{"id":"resp_client_next","usage":{"input_tokens":7,"output_tokens":1}}}`} {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(payload))
	}
	if _, b, err := conn.ReadMessage(); err == nil {
		t.Errorf("unexpected extra client request: %s", b)
	}
}

// CP-PROBE-001/002: native completion stops; only an inconclusive native
// invocation switches once, and the final result belongs to the selected path.
func TestSteeringNativeFirstFallback(t *testing.T) {
	for _, mode := range []string{"native_completed", "native_pending", "native_successor_failed", "native_auth", "native_rejected", "native_unknown", "client_rejected", "client_pending"} {
		t.Run(mode, func(t *testing.T) {
			var connections atomic.Int32
			transportServer(t, func(w http.ResponseWriter, r *http.Request) {
				connections.Add(1)
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if strings.HasSuffix(r.Header.Get("Session-Id"), ":client") {
					cm := "completed"
					if mode == "client_rejected" {
						cm = "rejected"
					}
					if mode == "client_pending" {
						cm = "pending"
					}
					serveClientSteering(t, conn, r, cm)
					return
				}
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_parent"}}`))
				_, steer, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var fields map[string]any
				_ = json.Unmarshal(steer, &fields)
				if fields["type"] != "response.steer" || len(fields) != 3 {
					t.Errorf("native changed: %s", steer)
				}
				if mode == "native_auth" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"invalid_token"}}`))
					return
				}
				if mode == "native_unknown" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"unknown_future_error"}}`))
					return
				}
				if mode == "native_rejected" || strings.HasPrefix(mode, "client_") {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"unsupported_native_inflight_message"}}`))
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.steer.accepted","steer":{"id":"steer_one","previous_response_id":"resp_parent"}}`))
				if mode == "native_pending" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.steer.pending","steer":{"id":"steer_one","previous_response_id":"resp_parent"},"required_input":[{"type":"function_call_output"}]}`))
					return
				}
				terminal := `{"type":"response.completed","response":{"id":"resp_next"}}`
				if mode == "native_successor_failed" {
					terminal = `{"type":"response.failed","response":{"id":"resp_next","error":{"code":"server_error"}}}`
				}
				for _, payload := range []string{`{"type":"response.completed","response":{"id":"resp_parent"}}`, `{"type":"response.created","response":{"id":"resp_next"}}`, terminal} {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(payload))
				}
			})
			s := transportUpstream(t)
			startSteeringTest(t, s, time.Now().Add(5*time.Second))
			p := s.steeringSession("probe_test")
			select {
			case <-p.done:
			case <-time.After(6 * time.Second):
				t.Fatal("probe stuck")
			}
			v := closeSteeringTest(t, s).Snapshot
			e := v.Evidence
			switched := mode == "native_rejected" || mode == "native_unknown" || strings.HasPrefix(mode, "client_")
			count := int32(1)
			target := steering.NativeTarget
			want := steering.Supported
			if switched {
				count = 2
				target = steering.ClientTarget
			}
			if mode == "native_pending" || mode == "client_pending" {
				want = steering.Pending
			}
			if mode == "native_auth" {
				want = steering.Unknown
			}
			if mode == "client_rejected" {
				want = steering.Unsupported
			}
			if connections.Load() != count || !e.Done || e.Capability != want || e.CapabilityTarget != target || (v.Fallback != nil) != switched {
				t.Fatalf("connections=%d report=%+v fallback=%+v", connections.Load(), e, v.Fallback)
			}
			if switched && (e.SteerSent || !e.InputQueued || v.Fallback.SteerSent != true || v.Fallback.UpstreamError == nil) {
				t.Fatal("fallback evidence lost", v)
			}
			if mode == "native_successor_failed" && e.ExecutionOutcome != "successor_failed" {
				t.Fatal(e)
			}
		})
	}
}

// CP-PROBE-002/003: cancellation in the fallback preserves its own result
// alongside the original diagnostic, without resurrecting the native result.
func TestSteeringFallbackCancellation(t *testing.T) {
	ready := make(chan struct{})
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_parent"}}`))
		if strings.HasSuffix(r.Header.Get("Session-Id"), ":client") {
			close(ready)
			if _, b, err := conn.ReadMessage(); err == nil {
				t.Errorf("continued before terminal: %s", b)
			}
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"unsupported_native_inflight_message"}}`))
	})
	s := transportUpstream(t)
	startSteeringTest(t, s, time.Now().Add(5*time.Second))
	p := s.steeringSession("probe_test")
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("no fallback")
	}
	deadline := time.Now().Add(time.Second)
	for !p.read().Evidence.InputQueued && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	before := p.read()
	if before.Evidence.Done || before.Fallback == nil || !before.Evidence.InputQueued {
		t.Fatal(before)
	}
	v := closeSteeringTest(t, s).Snapshot
	if !v.Evidence.Done || v.Evidence.Capability != steering.Unknown || v.Evidence.CapabilityTarget != steering.ClientTarget || v.Evidence.ExecutionOutcome != "canceled" || v.Evidence.ContinuationSent || v.Fallback.ErrorCode != "unsupported_native_inflight_message" {
		t.Fatal(v)
	}
}
func TestSteeringFallbackSnapshotClone(t *testing.T) {
	p := &steeringSession{snapshot: events.SteeringSnapshotResult{Headers: []events.Header{{Name: "native", Value: "original"}}, ClientHeaders: []events.Header{{Name: "client", Value: "original"}}}}
	e := steering.NewEvidence()
	token := int64(3)
	e.ParentUsage = &steering.Usage{InputTokens: &token}
	e.UpstreamError = &steering.UpstreamError{Code: "original"}
	e.Timeline = []steering.TraceEvent{{Type: "original"}}
	p.beginFallback(e, "native_protocol_inconclusive")
	p.publish(steering.NewClientEvidence())
	if !p.read().Evidence.UpstreamRequestSent {
		t.Fatal("native request attempt lost before fallback send")
	}
	e.UpstreamError.Code = "writer"
	*e.ParentUsage.InputTokens = 99
	e.Timeline[0].Type = "writer"
	v := p.read()
	if v.Fallback.UpstreamError.Code != "original" || *v.Fallback.ParentUsage.InputTokens != 3 || v.Fallback.Timeline[0].Type != "original" {
		t.Fatal(v)
	}
	v.Headers[0].Value = "reader"
	v.ClientHeaders[0].Value = "reader"
	v.Fallback.UpstreamError.Code = "reader"
	*v.Fallback.ParentUsage.InputTokens = 99
	v.Fallback.Timeline[0].Type = "reader"
	next := p.read()
	if next.Headers[0].Value != "original" || next.ClientHeaders[0].Value != "original" || next.Fallback.UpstreamError.Code != "original" || *next.Fallback.ParentUsage.InputTokens != 3 || next.Fallback.Timeline[0].Type != "original" {
		t.Fatal(next)
	}
}

// CP-PROBE-002: changing invocation never resets the receive budget.
func TestSteeringFallbackSharedReceiveBudget(t *testing.T) {
	var connections atomic.Int32
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_parent"}}`))
		client := strings.HasSuffix(r.Header.Get("Session-Id"), ":client")
		count := 4
		if client {
			count = 8
		} else {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
		payload := []byte(`{"type":"response.metadata","padding":"` + strings.Repeat("x", (1<<20)-256) + `"}`)
		for range count {
			if conn.WriteMessage(websocket.TextMessage, payload) != nil {
				return
			}
		}
		if !client {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"unsupported_native_inflight_message"}}`))
		}
	})
	s := transportUpstream(t)
	startSteeringTest(t, s, time.Now().Add(10*time.Second))
	p := s.steeringSession("probe_test")
	select {
	case <-p.done:
	case <-time.After(11 * time.Second):
		t.Fatal("receive budget did not stop")
	}
	v := closeSteeringTest(t, s).Snapshot
	if connections.Load() != 2 || v.Fallback == nil || v.Evidence.ErrorCode != "receive_limit" || v.Evidence.Capability != steering.Unknown || v.Evidence.CapabilityTarget != steering.ClientTarget || v.Evidence.ContinuationSent || v.Fallback.ReceivedBytes <= 3<<20 || v.Evidence.ReceivedBytes >= 6<<20 || v.Fallback.ReceivedBytes+v.Evidence.ReceivedBytes > 8<<20+1 {
		t.Fatal("receive budget reset", v)
	}
}

func TestSteeringDeadlinePreventsFallback(t *testing.T) {
	var connections atomic.Int32
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_parent"}}`))
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_, _, _ = conn.ReadMessage()
	})
	s := transportUpstream(t)
	startSteeringTest(t, s, time.Now().Add(300*time.Millisecond))
	p := s.steeringSession("probe_test")
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadline ignored")
	}
	v := closeSteeringTest(t, s).Snapshot
	if connections.Load() != 1 || v.Fallback != nil || v.Evidence.ErrorCode != "timeout" || v.Evidence.Capability != steering.Unknown {
		t.Fatal("deadline switched invocation", v)
	}
}
