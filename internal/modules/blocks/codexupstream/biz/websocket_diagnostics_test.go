package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	transport "aetherrelay/internal/pkg/aetherrelaytransport"
	wsclient "github.com/bogdanfinn/websocket"
	"github.com/gorilla/websocket"
	"github.com/muidea/magicCommon/event"
)

func TestWebsocketDiagnosticsAcrossExecutionPaths(t *testing.T) {
	const delta = `{"type":"response.output_text.delta","delta":"ok"}`
	const completed = `{"type":"response.completed","response":{"id":"done","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`
	for _, flow := range []string{"complete", "stream", "native"} {
		for _, scenario := range []string{"completed", "concatenated", "close_without_terminal", "close_error", "abrupt", "message_limit", "invalid_json"} {
			t.Run(flow+"/"+scenario, func(t *testing.T) {
				transportServer(t, func(w http.ResponseWriter, r *http.Request) {
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					if scenario == "concatenated" {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(delta+completed))
						return
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(delta))
					switch scenario {
					case "completed":
						_ = conn.WriteMessage(websocket.TextMessage, []byte(completed))
					case "close_without_terminal", "close_error":
						code := websocket.CloseNormalClosure
						if scenario == "close_error" {
							code = websocket.CloseInternalServerErr
						}
						_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, "private-close-text"), time.Now().Add(time.Second))
					case "message_limit":
						_ = conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", 2048)))
					case "invalid_json":
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":private-frame-content}`))
					}
				})
				up := transportUpstream(t)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				send := func(topic string, command any) any {
					t.Helper()
					value, err := up.SendEvent(events.BindCommandLane(event.NewEventWithContext(topic, "test", up.ID(), nil, ctx, command))).Get()
					if err != nil {
						t.Fatal(err)
					}
					return value
				}
				var observed events.HTTPResponseObservation
				var class events.ErrorClass
				body := []byte(`{"model":"gpt-test","input":"hello"}`)
				if flow == "complete" {
					result := send(events.TopicComplete, events.CompleteCommand{PreferWebsocket: true, AccessToken: "test-token", Body: body, MaxMessageBytes: 1024}).(events.CompleteResult)
					observed, class = result.Attempt.Response, result.ErrorClass
				} else if flow == "stream" {
					start := send(events.TopicStart, events.StartCommand{PreferWebsocket: true, AccessToken: "test-token", Body: body, MaxMessageBytes: 1024}).(events.StartResult)
					for {
						update := send(events.TopicPull, events.PullCommand{StreamID: start.StreamID, TimeoutMillis: 100}).(events.PullResult)
						if update.Done {
							observed, class = update.Progress, update.ErrorClass
							break
						}
					}
				} else {
					start := send(events.TopicWSOpen, events.WSOpenCommand{AccessToken: "test-token", MaxMessageBytes: 1024}).(events.WSOpenResult)
					if start.ErrorClass != "" {
						t.Fatal(start.ErrorClass)
					}
					payload := []byte(`{"type":"response.create","model":"gpt-test","input":"hello"}`)
					send(events.TopicWSSend, events.WSSendCommand{SessionID: start.SessionID, Payload: payload})
					// Delay consumption until the reader has exited: buffered events
					// and its final diagnostic must remain addressable by session ID.
					finished := make(chan struct{})
					go func() { up.workWG.Wait(); close(finished) }()
					select {
					case <-finished:
					case <-ctx.Done():
						t.Fatal("native reader did not exit")
					}
					for {
						update := send(events.TopicWSPull, events.WSPullCommand{SessionID: start.SessionID, TimeoutMillis: 100}).(events.WSPullResult)
						observed, class = update.Attempt.Response, update.ErrorClass
						if update.Done || observed.TerminalEvent != "" || observed.WebsocketFailureStage == "decode" {
							break
						}
					}
					if scenario == "completed" || scenario == "concatenated" {
						end := send(events.TopicWSPull, events.WSPullCommand{SessionID: start.SessionID}).(events.WSPullResult)
						if !end.Done || end.Attempt.Response.WebsocketFailureStage != "" || up.websocket(start.SessionID) != nil {
							t.Fatalf("settled turn marked failed or closed reader leaked: %+v", end)
						}
					} else if scenario != "invalid_json" && up.websocket(start.SessionID) != nil {
						t.Fatal("consumed final failure did not reclaim session")
					}
				}
				if scenario == "completed" || scenario == "concatenated" {
					if class != "" || observed.TerminalEvent != "response.completed" || observed.WebsocketFailureStage != "" {
						t.Fatalf("normal completion changed: %+v, %s", observed, class)
					}
					return
				}
				if flow != "native" || scenario != "invalid_json" {
					if class != events.ErrorNetwork {
						t.Fatalf("business classification changed: %s", class)
					}
				} else if class != "" {
					t.Fatal("native invalid JSON forwarding policy changed")
				}
				d := observed.WebsocketDiagnostics
				if d.WebsocketMessageLimitBytes != 1024 || !d.WebsocketLastMessageObserved {
					t.Fatalf("lost message observations: %+v", d)
				}
				switch scenario {
				case "close_without_terminal", "close_error", "abrupt":
					code := 1000
					if scenario == "close_error" {
						code = 1011
					}
					if scenario == "abrupt" {
						code = 1006
					}
					if d.WebsocketFailureStage != "read" || d.WebsocketErrorType != "close" || d.WebsocketCloseCode != code || d.WebsocketCloseReceived != (code != 1006) || observed.TransportReason != transport.WebsocketClose {
						t.Fatalf("close facts: %+v, reason=%s", d, observed.TransportReason)
					}
				case "message_limit":
					if d.WebsocketErrorType != "message_limit" || d.WebsocketLastMessageBytes != int64(len(delta)) || observed.TransportReason != transport.WebsocketMessageLimit {
						t.Fatalf("oversize confused with last successful message: %+v", d)
					}
				case "invalid_json":
					if d.WebsocketFailureStage != "decode" || d.WebsocketErrorType != "json_syntax" || d.WebsocketJSONErrorOffset <= 0 || observed.TransportReason != transport.WebsocketInvalidJSON {
						t.Fatalf("parse failure facts: %+v", d)
					}
				}
				encoded, _ := json.Marshal(d)
				if strings.Contains(string(encoded), "private-") {
					t.Fatal("payload/close text leaked into diagnostics")
				}
			})
		}
	}
}

func TestClosedWebsocketRetentionIsBounded(t *testing.T) {
	for _, mode := range []string{"expire", "close", "shutdown", "replaced"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			session := &websocketSession{cancel: cancel}
			up := &Upstream{websockets: map[string]*websocketSession{"test": session}}
			up.retainWebsocketUpdates("test", session, 10*time.Millisecond)
			switch mode {
			case "close":
				if !up.closeWebsocket("test") || ctx.Err() == nil {
					t.Fatal("explicit close did not reclaim session")
				}
			case "shutdown":
				up.BeginShutdown(context.Background())
			case "replaced":
				replacement := &websocketSession{cancel: func() {}}
				up.mu.Lock()
				up.websockets["test"] = replacement
				up.mu.Unlock()
				// Wait for the old timer callback without racing its map access.
				time.Sleep(30 * time.Millisecond)
				if up.websocket("test") != replacement {
					t.Fatal("expired reader removed a different session")
				}
				return
			}
			deadline := time.Now().Add(time.Second)
			for up.websocket("test") != nil && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if up.websocket("test") != nil {
				t.Fatal("abandoned closed session was not reclaimed")
			}
		})
	}
}

func TestResponsesWebsocketReadCancellationRetainsDiagnostics(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	const delta = `{"type":"response.output_text.delta","delta":"ok"}`
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(delta))
		<-release
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, _, _, _, err := performResponses(ctx, "test-token", "", "", []byte(`{"model":"gpt-test","input":"hello"}`), codexRequestProfile{}, true, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := response.Body.Read(make([]byte, 2048)); err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = response.Body.Read(make([]byte, 2048))
	d := websocketDiagnostic(err)
	if classifyRequestTransport(ctx, err) != events.ErrorCanceled || transportReason(err) != transport.Canceled || d.WebsocketFailureStage != "read" || d.WebsocketErrorType != "canceled" || d.WebsocketLastMessageBytes != int64(len(delta)) || d.WebsocketMessageLimitBytes != 1024 {
		t.Fatalf("canceled read lost facts: %+v err=%v", d, err)
	}
}

func TestNativeWebsocketWriteFailureRetainsDiagnostics(t *testing.T) {
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
	})
	up := transportUpstream(t)
	openedResult := event.NewResult(events.TopicWSOpen, "test", up.ID())
	up.handleWSOpen(event.NewEvent(events.TopicWSOpen, "test", up.ID(), nil, events.WSOpenCommand{AccessToken: "test-token", MaxMessageBytes: 1024}), openedResult)
	opened, err := event.GetAs[events.WSOpenResult](openedResult)
	if err != nil || opened.SessionID == "" {
		t.Fatalf("open=%+v err=%v", opened, err)
	}
	// Force a deterministic local write failure after the reader exits.
	_ = up.websocket(opened.SessionID).conn.Close()
	up.workWG.Wait()
	sentResult := event.NewResult(events.TopicWSSend, "test", up.ID())
	up.handleWSSend(event.NewEvent(events.TopicWSSend, "test", up.ID(), nil, events.WSSendCommand{SessionID: opened.SessionID, Payload: []byte(`{"type":"response.create","model":"gpt-test","input":"hello"}`)}), sentResult)
	sent, err := event.GetAs[events.WSSendResult](sentResult)
	if err != nil || sent.Sent || sent.ErrorClass != events.ErrorNetwork || sent.Attempt.Response.WebsocketFailureStage != "write" || sent.Attempt.Response.WebsocketMessageLimitBytes != 1024 {
		t.Fatalf("write failure lost or reclassified: %+v err=%v", sent, err)
	}
}

func TestNativeWebsocketDiagnosticDoesNotAmplifyBodyOrLeakAcrossTurns(t *testing.T) {
	session := &websocketSession{handshake: events.HTTPAttempt{Request: events.HTTPRequestObservation{URL: "ws://example.test"}, Response: events.HTTPResponseObservation{WebsocketDiagnostics: transport.WebsocketDiagnostics{WebsocketMessageLimitBytes: 1024}}}, archiveFullContent: true}
	request := []byte(`{"type":"response.create","model":"gpt-test"}`)
	session.beginObservation(request)
	session.observePayload([]byte(`{"type":"response.created"}`), false)
	var value any
	err := json.Unmarshal([]byte(`{"broken":`), &value)
	session.noteWebsocketFailure(context.Background(), "decode", err)
	first := session.observePayload([]byte(`{"broken":`), false)
	if first.Response.WebsocketFailureStage != "decode" || len(first.Request.Body) == 0 {
		t.Fatal("first decode diagnostic not published")
	}
	if next := session.observePayload([]byte(`{"type":"response.output_text.delta","delta":"ok"}`), false); next.Request.URL != "" {
		t.Fatal("each later event duplicated the archived request body")
	}
	session.beginObservation(request)
	session.noteWebsocketMessage([]byte(`{"type":"response.completed"}`))
	settled := session.observePayload([]byte(`{"type":"response.completed"}`), false)
	session.noteWebsocketFailure(context.Background(), "read", &wsclient.CloseError{Code: 1000})
	closed := session.observePayload(nil, true)
	if settled.Response.WebsocketFailureStage != "" || closed.Response.WebsocketFailureStage != "" || closed.Response.TransportReason != "" {
		t.Fatal("previous turn or idle connection close contaminated a settled turn")
	}
}

func TestWebsocketDiagnosticWrapperPreservesBusinessErrorAndContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		ctx    context.Context
		err    error
		stage  string
		reason transport.Reason
		kind   string
	}{
		{context.Background(), wsclient.ErrReadLimit, "read", transport.WebsocketMessageLimit, "message_limit"},
		{ctx, fmt.Errorf("closed socket"), "read", transport.Canceled, "canceled"},
		{context.Background(), context.DeadlineExceeded, "read", transport.Timeout, "timeout"},
		{context.Background(), fmt.Errorf("private-write-error"), "write", transport.WebsocketWriteError, "write_error"},
	} {
		err := websocketFailure(tc.ctx, tc.stage, tc.err, transport.WebsocketDiagnostics{})
		if classifyTransport(err) != classifyTransport(tc.err) || transportReason(err) != tc.reason || err.diagnostic.WebsocketErrorType != tc.kind {
			t.Fatalf("wrapped classification changed: %+v", err)
		}
	}
}
