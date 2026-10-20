package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestModelTraceSampleHTTPCompletionAndDrain(t *testing.T) {
	for _, scenario := range []string{"complete", "cancel", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			cancelEarly := scenario == "cancel"
			oversize := scenario == "oversize"
			entered := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Instructions *string         `json:"instructions"`
					Stream       bool            `json:"stream"`
					Metadata     json.RawMessage `json:"client_metadata"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || !request.Stream || len(request.Metadata) == 0 {
					t.Error("missing effective request projection")
				}
				if request.Instructions == nil || *request.Instructions != "" {
					t.Error("CP-REQ-003: final outbound sample must preserve empty string instructions")
				}
				if r.Header.Get("Authorization") != "Bearer test-secret" {
					t.Error("missing owner credential")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				close(entered)
				if cancelEarly {
					<-r.Context().Done()
					return
				}
				if oversize {
					_, _ = fmt.Fprint(w, strings.Repeat(": keepalive\n\n", 100000))
					w.(http.Flusher).Flush()
				}
				fmt.Fprintln(w, `data: {"type":"response.completed","response":{"id":"resp_test","status":"completed","model":"claimed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"1,2,3"}]}],"usage":{"input_tokens":1,"output_tokens":3,"total_tokens":4}}}`)
				fmt.Fprintln(w)
			}))
			defer server.Close()
			old := responsesURL
			responsesURL = server.URL
			defer func() { responsesURL = old }()
			hub := event.NewHub(32)
			bg := task.NewBackgroundRoutine(8)
			s := New(hub, bg)
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				s.Teardown(ctx)
				bg.Shutdown(ctx)
				hub.Terminate(ctx)
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			send := func(topic string, data interface{}) interface{} {
				t.Helper()
				v, err := s.SendEvent(events.BindCommandLane(event.NewEventWithContext(topic, "test", s.ID(), event.NewHeader(), ctx, data))).Get()
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
			send(events.TopicSampleStart, events.SampleStartCommand{ProbeID: "sample", Deadline: time.Now().Add(2 * time.Second), Request: events.CompleteCommand{AccessToken: "test-secret", Body: []byte(`{"model":"gpt-test","input":"numbers","instructions":""}`), SessionHash: "fresh"}})
			<-entered
			if !cancelEarly {
				for {
					snapshot := send(events.TopicSampleSnapshot, events.SampleSnapshotCommand{ProbeID: "sample"}).(events.SampleSnapshotResult)
					if snapshot.Done {
						if oversize {
							if snapshot.ErrorClass != events.ErrorProtocol || len(snapshot.Body) != 0 {
								t.Fatal("wire limit not enforced")
							}
							break
						}
						if snapshot.ErrorClass != "" || !strings.Contains(string(snapshot.Body), "1,2,3") || len(snapshot.EffectiveRequestSHA256) != 64 {
							t.Fatal(snapshot.ErrorClass, string(snapshot.Body))
						}
						snapshot.Body[0] = 'x'
						break
					}
					time.Sleep(time.Millisecond)
				}
			}
			if cancelEarly {
				s.BeginShutdown(ctx)
			}
			closed := send(events.TopicSampleClose, events.SampleCloseCommand{ProbeID: "sample"}).(events.SampleCloseResult)
			if !closed.Closed || !closed.Snapshot.Done || s.sample("sample") != nil {
				t.Fatal("false drain acknowledgement")
			}
			if closed.Snapshot.RequestStartedAt.IsZero() || (!cancelEarly && closed.Snapshot.ResponseReceivedAt.IsZero()) {
				t.Fatal("sample must carry the actual HTTP attempt times", closed.Snapshot)
			}
			// Cancellation may win before response headers reach the client.
			if at := closed.Snapshot.ResponseReceivedAt; !at.IsZero() && (at.Before(closed.Snapshot.RequestStartedAt) || at.After(time.Now())) {
				t.Fatal("invalid HTTP response observation time", at)
			}
			if !cancelEarly && !oversize && closed.Snapshot.Body[0] == 'x' {
				t.Fatal("snapshot mutation escaped")
			}
			again := send(events.TopicSampleClose, events.SampleCloseCommand{ProbeID: "sample"}).(events.SampleCloseResult)
			if !again.Closed {
				t.Fatal("close not idempotent")
			}
		})
	}
}
func TestModelTraceSampleNilResult(t *testing.T) {
	s := &Upstream{}
	ev := event.NewEvent("test", "test", "test", nil, nil)
	s.handleSampleStart(ev, nil)
	s.handleSampleSnapshot(ev, nil)
	s.handleSampleClose(ev, nil)
}
