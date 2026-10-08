package biz

import (
	"context"
	"testing"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestMandatorySubscriptionRejectsClosedHub(t *testing.T) {
	hub := event.NewHub(4)
	hub.Terminate(context.Background())
	base := New("owner", hub, nil)
	defer func() {
		value := recover()
		err, ok := value.(*cd.Error)
		if !ok || err.Code != cd.InvalidOperation {
			t.Fatalf("subscription failure was ignored or changed: %v", value)
		}
	}()
	base.SubscribeFunc("topic", func(event.Event, event.Result) {})
}
func TestAsyncTaskReturnsAdmissionFailure(t *testing.T) {
	background := task.NewBackgroundRoutine(4)
	background.Shutdown(context.Background())
	base := New("owner", event.NewHub(4), background)
	defer base.EventHub().Terminate(context.Background())
	if err := base.AsyncTask(func() { t.Error("rejected task executed") }); err == nil {
		t.Fatal("task admission error was ignored")
	}
}
