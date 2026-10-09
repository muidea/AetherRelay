package biz

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	transport "aetherrelay/internal/pkg/aetherrelaytransport"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestCustomToolTimingAndBufferingCrossNativeContracts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Safety-Buffering-Enabled", "true")
		io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(25 * time.Millisecond)
		io.WriteString(w, "data: {\"type\":\"response.custom_tool_call_input.delta\",\"delta\":\"patch\"}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(10 * time.Millisecond)
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"output\":[{\"type\":\"custom_tool_call\",\"input\":\"patch\"}],\"usage\":{\"output_tokens\":5}}}\n\n")
	}))
	defer server.Close()
	previous := responsesURL
	responsesURL = server.URL
	defer func() { responsesURL = previous }()
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(4)
	up := New(hub, background)
	defer func() {
		up.Teardown(context.Background())
		background.Shutdown(context.Background())
		hub.Terminate(context.Background())
	}()
	for _, mode := range []string{"stream", "complete"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			body := []byte(`{"model":"gpt-test","input":[]}`)
			var observation events.HTTPResponseObservation
			if mode == "complete" {
				r := event.NewResult(events.TopicComplete, "test", up.ID())
				up.handleComplete(event.NewEventWithContext(events.TopicComplete, "test", up.ID(), nil, ctx, events.CompleteCommand{AccessToken: "test", Body: body}), r)
				value, err := r.Get()
				if err != nil {
					t.Fatal(err)
				}
				got := value.(events.CompleteResult)
				if got.ErrorClass != "" || !strings.Contains(string(got.Body), "custom_tool_call") {
					t.Fatal(got)
				}
				observation = got.Attempt.Response
			} else {
				r := event.NewResult(events.TopicStart, "test", up.ID())
				up.handleStart(event.NewEventWithContext(events.TopicStart, "test", up.ID(), nil, ctx, events.StartCommand{AccessToken: "test", Body: body}), r)
				value, err := r.Get()
				if err != nil {
					t.Fatal(err)
				}
				got := value.(events.StartResult)
				if got.ErrorClass != "" || got.StreamID == "" || !got.Attempt.Response.GenerationBuffered {
					t.Fatal(got)
				}
				for {
					r := event.NewResult(events.TopicPull, "test", up.ID())
					up.handlePull(event.NewEventWithContext(events.TopicPull, "test", up.ID(), nil, ctx, events.PullCommand{StreamID: got.StreamID}), r)
					value, err := r.Get()
					if err != nil {
						t.Fatal(err)
					}
					pulled := value.(events.PullResult)
					if pulled.ErrorClass != "" {
						t.Fatal(pulled)
					}
					if pulled.Done {
						observation = pulled.Progress
						break
					}
				}
			}
			if observation.FirstOutputAt.IsZero() || observation.GenerationDuration <= 0 || observation.GenerationPartial || !observation.GenerationBuffered || observation.TerminalEvent != "response.completed" {
				t.Fatal(observation)
			}
			if observation.ReadDurationMS-observation.GenerationDuration.Milliseconds() < 15 {
				t.Fatal("prelude wait entered generation timing", observation)
			}
		})
	}
}

func TestSSELineLimitIsProtocolFailureThroughReadPaths(t *testing.T) {
	for _, mode := range []string{"stream", "complete"} {
		t.Run(mode, func(t *testing.T) {
			body := io.NopCloser(strings.NewReader("data: " + strings.Repeat("x", 1<<20) + "\n"))
			if mode == "complete" {
				_, class, _, _, err := completedResponse(&http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, 2<<20)
				if class != events.ErrorProtocol || completedTransportReason(class, err) != transport.SSELineLimit {
					t.Fatal(class, err)
				}
				return
			}
			stream := &responseStream{updates: make(chan streamUpdate, 8)}
			(&Upstream{}).runStream(context.Background(), "test", stream, body, 1<<20)
			last := <-stream.updates
			if !last.done || last.errorClass != events.ErrorProtocol || last.transportReason != transport.SSELineLimit {
				t.Fatal(last)
			}
		})
	}
}

func TestBufferingSignalRequiresExplicitTrue(t *testing.T) {
	for _, value := range []string{"true", " TRUE ", "false", "", "unknown"} {
		h := map[string][]string{"x-codex-safety-buffering-enabled": {value}}
		got := observedHTTPResponse(200, -1, nil, h, time.Now(), false)
		want := value == "true" || value == " TRUE "
		if got.GenerationBuffered != want {
			t.Fatal(value, got.GenerationBuffered)
		}
	}
}
