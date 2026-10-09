package biz

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
)

// A pipe separates observed output and terminal even when the caller buffers
// SSE into a unary response. Metadata arrives well before generated output.
func TestBufferedCompletionObservesGenerationSeparatelyFromFirstEvent(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		defer writer.Close()
		io.WriteString(writer, "data: {\"type\":\"response.created\"}\n\n")
		time.Sleep(40 * time.Millisecond)
		io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		time.Sleep(10 * time.Millisecond)
		io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"output_tokens\":5}}}\n\n")
	}()
	observation := events.HTTPResponseObservation{}
	resp := &http.Response{Body: reader, Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
	body, class, _, _, err := completedResponse(resp, 1<<20, &observation)
	if err != nil || class != "" || !strings.Contains(string(body), "hello") {
		t.Fatal(string(body), class, err)
	}
	if observation.FirstOutputAt.IsZero() || observation.GenerationDuration <= 0 || observation.GenerationPartial {
		t.Fatal(observation)
	}
	if observation.ReadDurationMS-int64(observation.GenerationDuration/time.Millisecond) < 30 {
		t.Fatal("TTFT entered generation duration", observation)
	}
}
func TestJSONCompletionHasUnknownGenerationTiming(t *testing.T) {
	observation := events.HTTPResponseObservation{}
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(`{"id":"r","object":"response","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}`)), Header: http.Header{"Content-Type": []string{"application/json"}}}
	_, _, _, _, err := completedResponse(resp, 1<<20, &observation)
	if err != nil {
		t.Fatal(err)
	}
	if !observation.FirstOutputAt.IsZero() || observation.GenerationDuration != 0 {
		t.Fatal(observation)
	}
}
