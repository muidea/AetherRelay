package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

func TestShutdownInterruptsCommandWithLifecycleCause(t *testing.T) {
	up := &Upstream{}
	entered := make(chan struct{})
	done := make(chan struct{})
	result := event.NewResult(events.TopicWSPull, "test", "upstream")
	handler := up.commandHandler(func(ev event.Event, result event.Result) {
		close(entered)
		<-ev.Context().Done()
		result.Set(nil, cd.NewError(cd.Unexpected, "Codex websocket pull canceled"))
	})
	go func() {
		defer close(done)
		handler(event.NewEventWithContext(events.TopicWSPull, "test", "upstream", nil, context.Background(), events.WSPullCommand{}), result)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("command not entered")
	}
	if err := up.BeginShutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("command did not exit")
	}
	if !errors.Is(result.Error(), events.ErrServiceShutdown) {
		t.Fatalf("lost shutdown cause: %v", result.Error())
	}
	late := event.NewResult(events.TopicComplete, "test", "upstream")
	up.commandHandler(func(event.Event, event.Result) { t.Error("started generation while stopping") })(event.NewEventWithContext(events.TopicComplete, "test", "upstream", nil, context.Background(), events.CompleteCommand{}), late)
	if !errors.Is(late.Error(), events.ErrServiceShutdown) {
		t.Fatal("shutdown admission lost cause", late.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := up.Quiesce(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownResultPreservesSuccessAndUpstreamRejection(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(events.ErrServiceShutdown)
	for _, class := range []events.ErrorClass{"", events.ErrorRateLimit, events.ErrorInvalidToken, events.ErrorCanceled} {
		result := event.NewResult("test", "test", "test")
		shutdownResult{Result: result, ctx: ctx}.Set(events.WSPullResult{ErrorClass: class, Payload: []byte("evidence")}, nil)
		value, err := event.GetAs[events.WSPullResult](result)
		want := class
		if class == events.ErrorCanceled {
			want = events.ErrorServiceShutdown
		}
		if err != nil || value.ErrorClass != want || string(value.Payload) != "evidence" {
			t.Fatalf("class %s -> %+v %v", class, value, err)
		}
	}
	if got := classifyRequestTransport(ctx, context.Canceled); got != events.ErrorServiceShutdown {
		t.Fatal(got)
	}
}
