package biz

import (
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
)

func TestGenerationProjectionAndEmptyCancelPreserveTerminalSample(t *testing.T) {
	at := time.Now()
	up := upevents.HTTPAttempt{Response: upevents.HTTPResponseObservation{FirstOutputAt: at, GenerationDuration: time.Second, GenerationPartial: true, GenerationBuffered: true}}
	attempt := toCodexHTTPAttempt(up)
	if !attempt.Response.FirstOutputAt.Equal(at) || attempt.Response.GenerationDuration != time.Second || !attempt.Response.GenerationPartial || !attempt.Response.GenerationBuffered {
		t.Fatal(attempt)
	}
	mergeCodexStreamProgress(&attempt, upevents.HTTPResponseObservation{FirstOutputAt: at, GenerationDuration: 2 * time.Second})
	mergeCodexStreamProgress(&attempt, upevents.HTTPResponseObservation{}) // Cancel after terminal already removed the stream.
	if !attempt.Response.FirstOutputAt.Equal(at) || attempt.Response.GenerationDuration != 2*time.Second || attempt.Response.GenerationPartial || !attempt.Response.GenerationBuffered {
		t.Fatal(attempt)
	}
	var empty codexresponses.HTTPAttempt
	mergeCodexStreamProgress(&empty, upevents.HTTPResponseObservation{})
	if !empty.Response.FirstOutputAt.IsZero() {
		t.Fatal(empty)
	}
}
