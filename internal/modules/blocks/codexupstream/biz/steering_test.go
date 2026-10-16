package biz

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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
	for _, mode := range []string{"completed", "pending", "upstream_error", "disconnect", "oversize", "cumulative", "compressed", "cancel"} {
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
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_parent","model":"gpt-test"}}`))
				_, steer, err := conn.ReadMessage()
				if err != nil {
					return
				}
				wire <- steer
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
			if !e.CreateSent || !e.CreateWriteCompleted || !e.SteerSent || !e.SteerWriteCompleted {
				t.Fatal("successful writes lost", e)
			}
			switch mode {
			case "upstream_error":
				if e.Capability != steering.Unknown || e.ErrorCode != "upstream_error" || e.UpstreamError == nil || e.UpstreamError.Code != "future_rejection" || e.UpstreamError.Message != "this profile was rejected" || e.UpstreamError.Status != 0 || e.HTTPStatus != 101 || e.SteerID != "" {
					t.Fatal("unknown upstream failure lost", e)
				}
				want := []string{"websocket.handshake", "response.create", "response.create", "response.created", "response.steer", "response.steer", "error", "probe.finished"}
				if len(e.Timeline) != len(want) {
					t.Fatal(e.Timeline)
				}
				for i, kind := range want {
					if e.Timeline[i].Type != kind {
						t.Fatal(e.Timeline)
					}
				}
			case "completed":
				if e.Capability != steering.Supported || e.ExecutionOutcome != "completed" || e.ParentUsage == nil || e.SuccessorUsage == nil {
					t.Fatal(e)
				}
			case "pending":
				if e.Capability != steering.Pending || e.ExecutionOutcome != "waiting_for_required_input" {
					t.Fatal(e)
				}
			case "disconnect", "cancel":
				if e.Capability != steering.Unknown || e.ErrorCode == "" {
					t.Fatal(e)
				}
			default:
				if e.Capability != steering.Unknown || e.ErrorCode != "receive_limit" {
					t.Fatal(e)
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
