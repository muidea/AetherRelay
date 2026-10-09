package biz

import (
	"context"
	"time"

	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type Base struct {
	id                string
	eventHub          event.Hub
	simpleObserver    event.SimpleObserver
	backgroundRoutine task.BackgroundRoutine
}

type routineTask struct {
	funcPtr func()
}

func (s *routineTask) Run() {
	s.funcPtr()
}

func New(
	id string,
	eventHub event.Hub,
	backgroundRoutine task.BackgroundRoutine) Base {
	return Base{
		id:                id,
		eventHub:          eventHub,
		simpleObserver:    event.NewSimpleObserver(id, eventHub),
		backgroundRoutine: backgroundRoutine,
	}
}

func (s *Base) ID() string {
	return s.id
}

// EventHub exposes the owner Hub only to a Biz derived from Base. It exists for
// typed EventHub-backed contract helpers; Module roots and adapters must not
// retain the returned Hub.
func (s *Base) EventHub() event.Hub {
	return s.eventHub
}

func (s *Base) BackgroundRoutine() task.BackgroundRoutine {
	return s.backgroundRoutine
}

func (s *Base) Subscribe(eventID string, observer event.Observer) {
	if err := s.eventHub.Subscribe(eventID, observer); err != nil {
		panic(err)
	}
}

func (s *Base) Unsubscribe(eventID string, observer event.Observer) {
	if err := s.eventHub.Unsubscribe(eventID, observer); err != nil {
		panic(err)
	}
}

func (s *Base) SubscribeFunc(eventID string, observerFunc event.ObserverFunc) {
	if err := s.simpleObserver.Subscribe(eventID, observerFunc); err != nil {
		panic(err)
	}
}

func (s *Base) UnsubscribeFunc(eventID string) {
	if err := s.simpleObserver.Unsubscribe(eventID); err != nil {
		panic(err)
	}
}

func (s *Base) PostEvent(event event.Event) {
	s.eventHub.Post(event)
}

func (s *Base) SendEvent(event event.Event) event.Result {
	return s.eventHub.Send(event)
}

func (s *Base) SyncTask(funcPtr func()) {
	taskPtr := &routineTask{funcPtr: funcPtr}

	if err := s.backgroundRoutine.SyncTask(taskPtr); err != nil {
		panic(err)
	}
}

func (s *Base) AsyncTask(funcPtr func()) error {
	taskPtr := &routineTask{funcPtr: funcPtr}
	return s.backgroundRoutine.AsyncTask(taskPtr)
}

// AsyncTaskContext bounds admission; accepted work remains managed by the
// shared routine and owns its cancellation independently of the submitting request.
func (s *Base) AsyncTaskContext(ctx context.Context, funcPtr func()) error {
	return s.backgroundRoutine.AsyncTaskContext(ctx, &routineTask{funcPtr: funcPtr})
}

func (s *Base) Timer(ctx context.Context, intervalValue time.Duration, offsetValue time.Duration, funcPtr func()) {
	taskPtr := &routineTask{funcPtr: funcPtr}
	if err := s.backgroundRoutine.Timer(ctx, taskPtr, intervalValue, offsetValue); err != nil {
		panic(err)
	}
}
