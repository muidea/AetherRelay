package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/gorilla/websocket"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

const transportCompleted = `{"type":"response.done","response":{"id":"resp_test","object":"response","model":"gpt-test","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":2}}}`

func transportServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	previousHTTP, previousWS := responsesURL, responsesWebsocketURL
	responsesURL, responsesWebsocketURL = server.URL, "ws"+strings.TrimPrefix(server.URL, "http")
	t.Cleanup(func() { responsesURL, responsesWebsocketURL = previousHTTP, previousWS; server.Close() })
	return server
}

func transportUpstream(t *testing.T) *Upstream {
	t.Helper()
	hub, background := event.NewHub(32), task.NewBackgroundRoutine(16)
	upstream := New(hub, background)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		upstream.Teardown(ctx)
		background.Shutdown(context.Background())
		hub.Terminate(context.Background())
	})
	return upstream
}

func TestResponsesPreferWSForBufferedAndStreaming(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var posts atomic.Int32
			wire := make(chan []byte, 1)
			transportServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					http.Error(w, "unexpected HTTP", 500)
					return
				}
				if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("ChatGPT-Account-ID") != "test-account" {
					t.Error("handshake lost account identity")
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_, payload, err := conn.ReadMessage()
				if err != nil {
					return
				}
				wire <- payload
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.metadata","headers":{"x-codex-turn-state":"opaque"}}`))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(transportCompleted))
			})
			upstream := transportUpstream(t)
			body := []byte(`{"model":"gpt-test","input":"<hello>&"}`)
			var attempt events.HTTPAttempt
			if !stream {
				result := event.NewResult(events.TopicComplete, "test", "upstream")
				upstream.handleComplete(event.NewEvent(events.TopicComplete, "test", "upstream", nil, events.CompleteCommand{PreferWebsocket: true, AccessToken: "test-token", AccountIDHeader: "test-account", Body: body, ArchiveFullContent: true}), result)
				value, err := event.GetAs[events.CompleteResult](result)
				if err != nil || value.ErrorClass != "" || !bytes.Contains(value.Body, []byte(`"output_tokens":2`)) {
					t.Fatalf("result=%+v err=%v", value, err)
				}
				attempt = value.Attempt
			} else {
				result := event.NewResult(events.TopicStart, "test", "upstream")
				upstream.handleStart(event.NewEvent(events.TopicStart, "test", "upstream", nil, events.StartCommand{PreferWebsocket: true, AccessToken: "test-token", AccountIDHeader: "test-account", Body: body, ArchiveFullContent: true}), result)
				started, err := event.GetAs[events.StartResult](result)
				if err != nil || started.ErrorClass != "" || started.StreamID == "" {
					t.Fatalf("start=%+v err=%v", started, err)
				}
				attempt = started.Attempt
				var output bytes.Buffer
				for range 16 {
					pulled := event.NewResult(events.TopicPull, "test", "upstream")
					upstream.handlePull(event.NewEvent(events.TopicPull, "test", "upstream", nil, events.PullCommand{StreamID: started.StreamID}), pulled)
					update, err := event.GetAs[events.PullResult](pulled)
					if err != nil || update.ErrorClass != "" {
						t.Fatalf("update=%+v err=%v", update, err)
					}
					output.Write(update.Data)
					if update.Done {
						break
					}
				}
				if !strings.Contains(output.String(), `"response.metadata"`) || !strings.Contains(output.String(), `"response.completed"`) {
					t.Fatalf("output=%s", output.String())
				}
			}
			payload := <-wire
			if !bytes.Contains(payload, []byte("<hello>&")) {
				t.Fatalf("WS changed native JSON encoding: %s", payload)
			}
			var create map[string]any
			if err := json.Unmarshal(payload, &create); err != nil {
				t.Fatal(err)
			}
			if create["type"] != "response.create" || create["stream"] != nil || create["store"] != false {
				t.Fatalf("create=%s", payload)
			}
			if posts.Load() != 0 || attempt.Response.Status != 101 || attempt.Response.Transport != "websocket" || !attempt.Response.WebsocketRequestSent || !bytes.Equal(payload, attempt.Request.Body) {
				t.Fatalf("posts=%d attempt=%+v", posts.Load(), attempt)
			}
		})
	}
}

func TestResponsesHandshakeFallbackPreservesHTTPAndRejections(t *testing.T) {
	for _, tc := range []struct {
		status   int
		body     string
		fallback bool
	}{
		{426, `{"error":{"message":"upgrade unsupported"}}`, true},
		{404, `{"error":{"message":"route unavailable"}}`, true},
		{405, `{}`, true}, {501, `{}`, true}, {502, `{}`, true},
		{400, `{}`, false}, {401, `{}`, false}, {403, `<html>denied</html>`, false},
		{429, `{"error":{"type":"usage_limit_reached","code":"usage_limit_reached"}}`, false},
		{404, `{"error":{"code":"model_not_found"}}`, false},
	} {
		t.Run(fmt.Sprintf("%d-%v", tc.status, tc.fallback), func(t *testing.T) {
			var gets, posts atomic.Int32
			transportServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets.Add(1)
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				posts.Add(1)
				if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("ChatGPT-Account-ID") != "test-account" {
					t.Error("fallback changed account")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", strings.Replace(transportCompleted, "response.done", "response.completed", 1))
			})
			upstream := transportUpstream(t)
			result := event.NewResult(events.TopicComplete, "test", "upstream")
			upstream.handleComplete(event.NewEvent(events.TopicComplete, "test", "upstream", nil, events.CompleteCommand{PreferWebsocket: true, AccessToken: "test-token", AccountIDHeader: "test-account", Body: []byte(`{"model":"gpt-test","input":"<hello>&"}`)}), result)
			completed, err := event.GetAs[events.CompleteResult](result)
			if err != nil {
				t.Fatal(err)
			}
			if gets.Load() != 1 || (posts.Load() == 1) != tc.fallback {
				t.Fatalf("gets=%d posts=%d result=%+v", gets.Load(), posts.Load(), completed)
			}
			if tc.fallback {
				if completed.ErrorClass != "" || completed.Attempt.Response.Transport != "http" || completed.Attempt.Response.FallbackReason == "" || len(completed.Attempt.Prior) != 1 || completed.Attempt.Prior[0].Response.Status != tc.status {
					t.Fatalf("fallback=%+v", completed)
				}
			} else if completed.ErrorClass == "" || completed.HTTPStatus != tc.status {
				t.Fatalf("rejection=%+v", completed)
			}
		})
	}
}

func TestResponsesWSDisconnectAfterCreateNeverUsesHTTP(t *testing.T) {
	var posts, creates atomic.Int32
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			http.Error(w, "unexpected", 500)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err = conn.ReadMessage(); err == nil {
			creates.Add(1)
		}
	})
	upstream := transportUpstream(t)
	result := event.NewResult(events.TopicComplete, "test", "upstream")
	upstream.handleComplete(event.NewEvent(events.TopicComplete, "test", "upstream", nil, events.CompleteCommand{PreferWebsocket: true, AccessToken: "test-token", Body: []byte(`{"model":"gpt-test","input":"hello"}`)}), result)
	completed, err := event.GetAs[events.CompleteResult](result)
	if err != nil || completed.ErrorClass == "" || !completed.Attempt.Response.WebsocketRequestSent || posts.Load() != 0 || creates.Load() != 1 {
		t.Fatalf("result=%+v err=%v posts=%d creates=%d", completed, err, posts.Load(), creates.Load())
	}
}

func TestResponsesIncrementalInputCannotFallback(t *testing.T) {
	var posts atomic.Int32
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		http.Error(w, "unsupported", 426)
	})
	_, _, class, _, err := performResponses(context.Background(), "test-token", "", "", []byte(`{"model":"gpt-test","previous_response_id":"resp_previous","input":[]}`), codexRequestProfile{}, true, 1024)
	// A rejected upgrade is returned as an HTTP-shaped error observation, not a
	// successful HTTP fallback. No prompt is sent to the HTTP endpoint.
	if posts.Load() != 0 || (err != nil && class == "") {
		t.Fatalf("posts=%d class=%q err=%v", posts.Load(), class, err)
	}
}

func TestNativeWebsocketHTTPFallbackKeepsSessionAndFrames(t *testing.T) {
	var gets, posts atomic.Int32
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			http.Error(w, "unsupported", 426)
			return
		}
		posts.Add(1)
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"previous_response_id"`)) || bytes.Contains(body, []byte(`"type":"response.create"`)) {
			t.Errorf("HTTP payload=%s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", strings.Replace(transportCompleted, "response.done", "response.completed", 1))
	})
	upstream := transportUpstream(t)
	result := event.NewResult(events.TopicWSOpen, "test", "upstream")
	upstream.handleWSOpen(event.NewEvent(events.TopicWSOpen, "test", "upstream", nil, events.WSOpenCommand{AccessToken: "test-token", AllowHTTPFallback: true, MaxMessageBytes: 1 << 20}), result)
	opened, err := event.GetAs[events.WSOpenResult](result)
	if err != nil || !opened.HTTPFallback || opened.SessionID == "" {
		t.Fatalf("open=%+v err=%v", opened, err)
	}
	for range 2 {
		sent := event.NewResult(events.TopicWSSend, "test", "upstream")
		upstream.handleWSSend(event.NewEvent(events.TopicWSSend, "test", "upstream", nil, events.WSSendCommand{SessionID: opened.SessionID, Payload: []byte(`{"type":"response.create","model":"gpt-test","input":[{"role":"user","content":"hello"}],"store":false}`)}), sent)
		value, err := event.GetAs[events.WSSendResult](sent)
		if err != nil || !value.Sent {
			t.Fatalf("send=%+v err=%v", value, err)
		}
		seen := false
		for range 8 {
			pulled := event.NewResult(events.TopicWSPull, "test", "upstream")
			upstream.handleWSPull(event.NewEvent(events.TopicWSPull, "test", "upstream", nil, events.WSPullCommand{SessionID: opened.SessionID}), pulled)
			update, err := event.GetAs[events.WSPullResult](pulled)
			if err != nil || update.ErrorClass != "" {
				t.Fatalf("pull=%+v err=%v", update, err)
			}
			if bytes.Contains(update.Payload, []byte(`"response.completed"`)) {
				seen = true
				if update.Attempt.Response.Transport != "http" || len(update.Attempt.Prior) != 1 {
					t.Fatalf("attempt=%+v", update.Attempt)
				}
				break
			}
		}
		if !seen {
			t.Fatal("missing WS terminal frame")
		}
	}
	if gets.Load() != 1 || posts.Load() != 2 {
		t.Fatalf("gets=%d posts=%d", gets.Load(), posts.Load())
	}
}

func TestResponsesConnectFailureAndCancellation(t *testing.T) {
	var posts atomic.Int32
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", transportCompleted)
	})
	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatal(listenErr)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	responsesWebsocketURL = "ws://" + address
	response, attempt, _, _, err := performResponses(context.Background(), "test-token", "", "", []byte(`{"model":"gpt-test","input":[]}`), codexRequestProfile{}, true, 1024)
	if err != nil || response == nil || attempt.Response.FallbackReason != "ws_connect_failed" || posts.Load() != 1 {
		t.Fatalf("attempt=%+v err=%v posts=%d", attempt, err, posts.Load())
	}
	_ = response.Body.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, _, _ = performResponses(ctx, "test-token", "", "", []byte(`{"model":"gpt-test","input":[]}`), codexRequestProfile{}, true, 1024)
	if posts.Load() != 1 {
		t.Fatal("canceled request fell back")
	}
}

func TestNativeHTTPFallbackPrewarmDoesNotGenerate(t *testing.T) {
	var posts atomic.Int32
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "unsupported", 426)
			return
		}
		posts.Add(1)
		http.Error(w, "unexpected inference", 500)
	})
	upstream := transportUpstream(t)
	openResult := event.NewResult(events.TopicWSOpen, "test", "upstream")
	upstream.handleWSOpen(event.NewEvent(events.TopicWSOpen, "test", "upstream", nil, events.WSOpenCommand{AccessToken: "test-token", AllowHTTPFallback: true}), openResult)
	opened, err := event.GetAs[events.WSOpenResult](openResult)
	if err != nil || !opened.HTTPFallback {
		t.Fatalf("open=%+v err=%v", opened, err)
	}
	sent := event.NewResult(events.TopicWSSend, "test", "upstream")
	upstream.handleWSSend(event.NewEvent(events.TopicWSSend, "test", "upstream", nil, events.WSSendCommand{SessionID: opened.SessionID, Payload: []byte(`{"type":"response.create","model":"gpt-test","input":[],"generate":false}`)}), sent)
	value, err := event.GetAs[events.WSSendResult](sent)
	if err != nil || !value.Sent {
		t.Fatalf("send=%+v err=%v", value, err)
	}
	completed := false
	for range 2 {
		pulled := event.NewResult(events.TopicWSPull, "test", "upstream")
		upstream.handleWSPull(event.NewEvent(events.TopicWSPull, "test", "upstream", nil, events.WSPullCommand{SessionID: opened.SessionID}), pulled)
		update, err := event.GetAs[events.WSPullResult](pulled)
		if err != nil || update.ErrorClass != "" {
			t.Fatalf("update=%+v err=%v", update, err)
		}
		completed = completed || bytes.Contains(update.Payload, []byte(`"response.completed"`))
	}
	if !completed || posts.Load() != 0 {
		t.Fatalf("completed=%v posts=%d", completed, posts.Load())
	}
}

func TestNativeHTTPFallbackCloseCancelsReader(t *testing.T) {
	requestClosed := make(chan struct{})
	transportServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "unsupported", 426)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.metadata\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(requestClosed)
	})
	upstream := transportUpstream(t)
	openResult := event.NewResult(events.TopicWSOpen, "test", "upstream")
	upstream.handleWSOpen(event.NewEvent(events.TopicWSOpen, "test", "upstream", nil, events.WSOpenCommand{AccessToken: "test-token", AllowHTTPFallback: true}), openResult)
	opened, err := event.GetAs[events.WSOpenResult](openResult)
	if err != nil || !opened.HTTPFallback {
		t.Fatalf("open=%+v err=%v", opened, err)
	}
	sent := event.NewResult(events.TopicWSSend, "test", "upstream")
	upstream.handleWSSend(event.NewEvent(events.TopicWSSend, "test", "upstream", nil, events.WSSendCommand{SessionID: opened.SessionID, Payload: []byte(`{"type":"response.create","model":"gpt-test","input":[]}`)}), sent)
	value, err := event.GetAs[events.WSSendResult](sent)
	if err != nil || !value.Sent {
		t.Fatalf("send=%+v err=%v", value, err)
	}
	upstream.closeWebsocket(opened.SessionID)
	select {
	case <-requestClosed:
	case <-time.After(time.Second):
		t.Fatal("HTTP fallback body survived session close")
	}
}
