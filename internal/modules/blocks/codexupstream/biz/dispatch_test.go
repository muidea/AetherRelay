package biz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestBlockedCompleteDoesNotBlockStreamOrCancel(t *testing.T) {
	slowEntered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		if body.Model == "slow" {
			close(slowEntered)
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		if body.Model == "waiting" {
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"output_text\":\"ok\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))
	}))
	defer server.Close()
	previous := responsesURL
	responsesURL = server.URL
	t.Cleanup(func() { responsesURL = previous })
	hub := event.NewHub(32)
	background := task.NewBackgroundRoutine(8)
	up := New(hub, background)
	defer func() {
		close(release)
		up.Teardown(context.Background())
		background.Shutdown(context.Background())
		hub.Terminate(context.Background())
	}()
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		hub.Send(events.BindCommandLane(event.NewEventWithContext(events.TopicComplete, "test", common.UnitID, nil, context.Background(), events.CompleteCommand{AccessToken: "test", Body: []byte(`{"model":"slow","input":[]}`)})))
	}()
	select {
	case <-slowEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Complete did not reach upstream")
	}
	requestCtx, cancelRequests := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRequests()
	send := func(topic string, cmd any) event.Result {
		ctx := requestCtx
		return hub.Send(events.BindCommandLane(event.NewEventWithContext(topic, "test", common.UnitID, nil, ctx, cmd)))
	}
	value, err := send(events.TopicStart, events.StartCommand{AccessToken: "test", Body: []byte(`{"model":"fast","input":[]}`)}).Get()
	if err != nil {
		t.Fatal(err)
	}
	started := value.(events.StartResult)
	if started.StreamID == "" {
		t.Fatalf("start=%+v", started)
	}
	var progress events.HTTPResponseObservation
	for range 12 {
		value, err = send(events.TopicPull, events.PullCommand{StreamID: started.StreamID, TimeoutMillis: 100}).Get()
		if err != nil {
			t.Fatal(err)
		}
		pulled := value.(events.PullResult)
		progress = pulled.Progress
		if pulled.Done {
			if pulled.ErrorClass != "" {
				t.Fatalf("pull=%+v", pulled)
			}
			break
		}
	}
	if progress.EventCount < 2 || progress.WireBytes == 0 {
		t.Fatalf("upstream progress=%+v", progress)
	}
	// Start's command context must survive handler completion until stream cancel.
	value, err = send(events.TopicStart, events.StartCommand{AccessToken: "test", Body: []byte(`{"model":"waiting","input":[]}`)}).Get()
	if err != nil {
		t.Fatal(err)
	}
	waiting := value.(events.StartResult)
	pullDone := make(chan struct{})
	go func() {
		defer close(pullDone)
		send(events.TopicPull, events.PullCommand{StreamID: waiting.StreamID, TimeoutMillis: 1000})
	}()
	value, err = send(events.TopicCancel, events.CancelCommand{StreamID: waiting.StreamID}).Get()
	if err != nil || !value.(events.CancelResult).Cancelled {
		t.Fatalf("cancel=%v err=%v", value, err)
	}
	select {
	case <-pullDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Cancel did not unblock Pull")
	}
	select {
	case <-slowDone:
		t.Fatal("slow Complete was interrupted by unrelated stream")
	default:
	}
}

func TestCompleteDeadlineIsTimeoutAndTeardownCancelsNetwork(t *testing.T) {
	entered := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	previous := responsesURL
	responsesURL = server.URL
	t.Cleanup(func() { responsesURL = previous })
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(4)
	up := New(hub, background)
	defer func() {
		up.Teardown(context.Background())
		background.Shutdown(context.Background())
		hub.Terminate(context.Background())
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	value, err := hub.Send(events.BindCommandLane(event.NewEventWithContext(events.TopicComplete, "test", common.UnitID, nil, ctx, events.CompleteCommand{AccessToken: "test", Body: []byte(`{"model":"slow","input":[]}`)}))).Get()
	if err != nil {
		t.Fatal(err)
	}
	completed := value.(events.CompleteResult)
	if completed.ErrorClass != events.ErrorTimeout || !completed.Attempt.Response.Observed {
		t.Fatalf("complete=%+v", completed)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Send(events.BindCommandLane(event.NewEventWithContext(events.TopicComplete, "test", common.UnitID, nil, context.Background(), events.CompleteCommand{AccessToken: "test", Body: []byte(`{"model":"slow","input":[]}`)})))
	}()
	<-entered
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second request not running")
	}
	up.Teardown(context.Background())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown left network command running")
	}
}

func TestExpiredQueuedCommandDoesNotExecute(t *testing.T) {
	hub := event.NewHub(16)
	defer hub.Terminate(context.Background())
	observer := event.NewSimpleObserver(common.UnitID, hub)
	entered := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	if err := observer.Subscribe(events.TopicPull, func(ev event.Event, result event.Result) {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		result.Set(events.PullResult{}, nil)
	}); err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		hub.Send(events.BindCommandLane(event.NewEventWithContext(events.TopicPull, "test", common.UnitID, nil, context.Background(), events.PullCommand{StreamID: "same-stream"})))
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := hub.Send(events.BindCommandLane(event.NewEventWithContext(events.TopicPull, "test", common.UnitID, nil, ctx, events.PullCommand{StreamID: "same-stream"}))).Get(); err == nil {
		t.Error("expired queued command was accepted")
	}
	close(release)
	<-firstDone
	if err := hub.(event.DrainingHub).Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("executed %d commands, want only admitted command", calls)
	}
}

func TestLocalCapacityDoesNotBlockCancellation(t *testing.T) {
	up := &Upstream{active: map[string]context.CancelFunc{}, streams: map[string]*responseStream{}, websockets: map[string]*websocketSession{}}
	for i := 0; i < maxConcurrentCommands; i++ {
		up.active[string(rune(i))] = func() {}
	}
	ran := false
	handler := up.commandHandler(func(_ event.Event, result event.Result) { ran = true; result.Set(true, nil) })
	result := event.NewResult(events.TopicComplete, "test", common.UnitID)
	handler(event.NewEventWithContext(events.TopicComplete, "test", common.UnitID, nil, context.Background(), events.CompleteCommand{}), result)
	if result.Error() == nil || ran {
		t.Fatal("capacity did not reject new network operation")
	}
	result = event.NewResult(events.TopicCancel, "test", common.UnitID)
	handler(event.NewEventWithContext(events.TopicCancel, "test", common.UnitID, nil, context.Background(), events.CancelCommand{StreamID: "stream"}), result)
	if result.Error() != nil || !ran {
		t.Fatal("local capacity blocked cancellation")
	}
}
