package biz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestStreamCancelClosesBlockedUpstreamBody(t *testing.T) {
	// CP-STREAM-013 / CP-ARCH-004: explicit owner cancellation must unblock
	// readLine even when the HTTP request's original context is still alive.
	for _, teardown := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "teardown"}[teardown], func(t *testing.T) {
			disconnected := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(disconnected)
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
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := event.NewResult(events.TopicStart, "test", up.ID())
			up.handleStart(event.NewEventWithContext(events.TopicStart, "test", up.ID(), nil, ctx, events.StartCommand{AccessToken: "test", Body: []byte(`{"model":"gpt-test"}`), MaxLineBytes: 1024}), r)
			value, err := r.Get()
			if err != nil {
				t.Fatal(err)
			}
			opened := value.(events.StartResult)
			if opened.StreamID == "" {
				t.Fatalf("open=%+v", opened)
			}
			if teardown {
				up.Teardown(context.Background())
			} else {
				r = event.NewResult(events.TopicCancel, "test", up.ID())
				up.handleCancel(event.NewEvent(events.TopicCancel, "test", up.ID(), nil, events.CancelCommand{StreamID: opened.StreamID}), r)
				if _, err := r.Get(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-disconnected:
			case <-time.After(2 * time.Second):
				cancel()
				t.Fatal("cancel left upstream reader blocked")
			}
			if len(up.streams) != 0 {
				t.Fatal("stream registry leaked")
			}
		})
	}
}
