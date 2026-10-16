package biz

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/gorilla/websocket"
	"github.com/muidea/magicCommon/event"
)

const timingObservation = `{"type":"responsesapi.websocket_timing","timing_scope":"logical_turn","timing_metrics":{"engine_total_prompt_tokens_total":99031,"num_sampled_tokens_total":74,"critical_path":{"coverage":"complete","boundary_type":"actionable_output_item_done","future":[null,true]}},"usage":{"output_tokens":74},"error":{"code":"observational"}}`

func TestAncillaryEventsAcrossResponsesTransports(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, terminal := range []string{"completed", "incomplete", "failed", "disconnect"} {
				t.Run(fmt.Sprintf("fallback=%t/stream=%t/%s", fallback, stream, terminal), func(t *testing.T) {
					var gets, posts atomic.Int32
					frames := []string{
						`{"type":"response.created","response":{"id":"resp_test"}}`,
						`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_test","name":"Edit","arguments":""}}`,
						`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`,
						`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{}"}`,
						`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call_test","name":"Edit","arguments":"{}"}}`,
						`{"type":"response.metadata","metadata":{"future":{"nested":null}}}`,
						timingObservation,
					}
					if terminal != "disconnect" {
						frames = append(frames, fmt.Sprintf(`{"type":"response.%s","response":{"id":"resp_test","object":"response","status":%q,"output":[],"usage":{"input_tokens":3,"output_tokens":2},"error":{"code":"server_error"}}}`, terminal, terminal))
					}
					transportServer(t, func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodGet {
							gets.Add(1)
							if fallback {
								w.WriteHeader(http.StatusUpgradeRequired)
								return
							}
							conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
							if err != nil {
								return
							}
							defer conn.Close()
							if _, _, err := conn.ReadMessage(); err != nil {
								return
							}
							for _, frame := range frames {
								if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
									return
								}
							}
							return
						}
						posts.Add(1)
						w.Header().Set("Content-Type", "text/event-stream")
						for _, frame := range frames {
							_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
						}
					})
					up := transportUpstream(t)
					body := []byte(`{"model":"gpt-test","input":"hello"}`)
					var class events.ErrorClass
					if !stream {
						r := event.NewResult(events.TopicComplete, "test", up.ID())
						up.handleComplete(event.NewEvent(events.TopicComplete, "test", up.ID(), nil, events.CompleteCommand{PreferWebsocket: true, AccessToken: "test-token", Body: body}), r)
						got, err := event.GetAs[events.CompleteResult](r)
						if err != nil {
							t.Fatal(err)
						}
						class = got.ErrorClass
						if class == "" && (!bytes.Contains(got.Body, []byte(`"output_tokens":2`)) || bytes.Contains(got.Body, []byte("timing_metrics")) || !bytes.Contains(got.Body, []byte(`"function_call"`))) {
							t.Fatalf("bad completion: %s", got.Body)
						}
					} else {
						r := event.NewResult(events.TopicStart, "test", up.ID())
						up.handleStart(event.NewEvent(events.TopicStart, "test", up.ID(), nil, events.StartCommand{PreferWebsocket: true, AccessToken: "test-token", Body: body}), r)
						got, err := event.GetAs[events.StartResult](r)
						if err != nil || got.StreamID == "" {
							t.Fatalf("start=%+v err=%v", got, err)
						}
						var wire bytes.Buffer
						done := false
						for range 64 {
							r := event.NewResult(events.TopicPull, "test", up.ID())
							up.handlePull(event.NewEvent(events.TopicPull, "test", up.ID(), nil, events.PullCommand{StreamID: got.StreamID}), r)
							pulled, err := event.GetAs[events.PullResult](r)
							if err != nil {
								t.Fatal(err)
							}
							wire.Write(pulled.Data)
							if pulled.Done {
								class, done = pulled.ErrorClass, true
								break
							}
						}
						if !done || !strings.Contains(wire.String(), timingObservation) {
							t.Fatalf("lost timing or terminal: %s", wire.String())
						}
					}
					wantFailure := terminal == "failed" || terminal == "disconnect"
					if (class != "") != wantFailure || gets.Load() != 1 || (posts.Load() == 1) != fallback {
						t.Fatalf("class=%s gets=%d posts=%d", class, gets.Load(), posts.Load())
					}
				})
			}
		}
	}
}

func TestObservationCannotMakeEmptyResponseSuccessful(t *testing.T) {
	for _, typ := range []string{"response.metadata", "codex.response.metadata", "codex.rate_limits", "responsesapi.websocket_timing"} {
		payload := fmt.Sprintf(`{"type":%q,"delta":"not output","item":{"text":"not output"},"usage":{"output_tokens":74},"error":{"code":"not a failure"}}`, typ)
		line := []byte("data: " + payload + "\n\n")
		if semantic, terminal := codexStreamSemanticsWithOutput(line, false, false); semantic || terminal || codexStreamOutputEvidence(line) {
			t.Fatalf("%s counted as output/completion", typ)
		}
		for _, end := range []string{"", `data: {"type":"response.completed","response":{"id":"empty","output":[]}}` + "\n\n"} {
			resp := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(string(line) + end))}
			if body, class, _, _, err := completedResponse(resp, 1<<20); err == nil || class == "" || len(body) != 0 {
				t.Fatalf("observation became success: %s class=%s err=%v", body, class, err)
			}
		}
	}
}

func TestWebsocketObservationIsolatedAcrossTurns(t *testing.T) {
	session := &websocketSession{}
	for _, terminal := range []string{"response.done", "response.completed", "response.incomplete", "response.failed"} {
		session.beginObservation([]byte(`{"model":"gpt-test"}`))
		first := session.observePayload([]byte(timingObservation), false)
		if !first.Response.FirstOutputAt.IsZero() || first.Response.TerminalEvent != "" {
			t.Fatal("previous turn or telemetry polluted first output", first)
		}
		session.observePayload([]byte(`{"type":"response.function_call_arguments.delta","delta":"{}"}`), false)
		time.Sleep(time.Millisecond)
		session.observePayload([]byte(timingObservation), false)
		last := session.observePayload([]byte(fmt.Sprintf(`{"type":%q}`, terminal)), false)
		if last.Response.FirstOutputAt.IsZero() || last.Response.GenerationDuration <= 0 || last.Response.TerminalEvent != terminal || last.Response.GenerationPartial != (terminal == "response.incomplete" || terminal == "response.failed") {
			t.Fatalf("bad terminal timing: %+v", last.Response)
		}
	}
	// Cleanup after a telemetry-only turn must not inherit earlier output.
	session.beginObservation([]byte(`{"model":"gpt-test"}`))
	session.observePayload([]byte(timingObservation), false)
	if last := session.observePayload(nil, true); !last.Response.FirstOutputAt.IsZero() {
		t.Fatal(last)
	}
}
