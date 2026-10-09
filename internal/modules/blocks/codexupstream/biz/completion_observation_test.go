package biz

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	transport "aetherrelay/internal/pkg/aetherrelaytransport"
)

func TestBufferedCompletionTracksProgressAndTerminalPresence(t *testing.T) {
	prefix := `data: {"type":"response.created","response":{"id":"test"}}` + "\n\n" + `data: {"type":"response.output_text.delta","delta":"private generated content"}` + "\n\n"
	for _, terminal := range []bool{false, true} {
		body := prefix
		if terminal {
			body += `data: {"type":"response.completed","response":{"object":"response","output":[],"usage":{"input_tokens":1,"output_tokens":2}}}` + "\n\n"
		}
		response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
		var observation events.HTTPResponseObservation
		_, class, _, _, err := completedResponse(response, 4096, &observation)
		if observation.LastEventAt.IsZero() || !observation.ReadObserved || observation.WireBytes == 0 || observation.EventCount < 2 {
			t.Fatalf("lost progress: %+v", observation)
		}
		if terminal {
			if err != nil || observation.TerminalEvent != "response.completed" {
				t.Fatalf("terminal=%+v err=%v", observation, err)
			}
		} else if class != events.ErrorProtocol || completedTransportReason(class, err) != transport.EOF || observation.TerminalEvent != "" {
			t.Fatalf("missing terminal: observation=%+v class=%s err=%v", observation, class, err)
		}
	}
}

func TestStreamObservationKeepsBusinessProgressAcrossKeepalives(t *testing.T) {
	stream := &responseStream{readStarted: time.Now()}
	stream.observe([]byte(`data: {"type":"response.output_text.delta","delta":"private"}`))
	before := stream.observation()
	stream.observe([]byte(": heartbeat\n"))
	stream.observe([]byte(`data: {"type":"keepalive"}`))
	after := stream.observation()
	if !after.LastEventAt.Equal(before.LastEventAt) || after.EventCount != before.EventCount || after.WireBytes <= before.WireBytes {
		t.Fatalf("heartbeat changed business progress: before=%+v after=%+v", before, after)
	}
	stream.observe([]byte(`data: {"type":"response.failed","error":{"message":"private"}}`))
	if got := stream.observation(); got.TerminalEvent != "response.failed" || got.EventCount != 2 {
		t.Fatalf("terminal=%+v", got)
	}
}
