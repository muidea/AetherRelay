package biz

import (
	"context"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	events "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	generation "aetherrelay/internal/pkg/aetherrelaygeneration"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestFinishedPartialGenerationStaysFrozen(t *testing.T) {
	at := time.Now().Add(-time.Second)
	stream := &textStream{}
	sample := generation.Sample{FirstOutputAt: at, Duration: 20 * time.Millisecond, Partial: true}
	stream.setGeneration(sample, true)
	stream.setGeneration(generation.Sample{}, true)
	if got := stream.generationSnapshot(at.Add(time.Hour)); got != sample {
		t.Fatalf("finished partial timing grew: %+v", got)
	}
}
func TestTextCancelReturnsUpstreamGenerationProgress(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)
	upstream, err := New(context.Background(), hub, background)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Teardown(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &textStream{cancel: cancel}
	at := time.Now().Add(-time.Second)
	stream.setGeneration(generation.Sample{FirstOutputAt: at, Partial: true}, false)
	upstream.streams["stream"] = stream
	value, eventErr := upstream.SendEvent(event.NewEvent(events.TopicCancelText, "test", common.UnitID, nil, events.CancelTextCommand{StreamID: "stream"})).Get()
	if eventErr != nil {
		t.Fatal(eventErr)
	}
	out, ok := value.(events.CancelTextResult)
	if !ok || !out.Cancelled || !out.Generation.FirstOutputAt.Equal(at) || out.Generation.Duration < time.Second || !out.Generation.Partial || ctx.Err() != context.Canceled {
		t.Fatal(value, ctx.Err())
	}
}
