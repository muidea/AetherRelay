package biz

import (
	"context"
	"testing"
	"time"

	tempcommon "aetherrelay/internal/modules/application/chatgpttemporarychat/pkg/common"
	events "aetherrelay/internal/modules/application/chatgpttemporarychat/pkg/events"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	usagecommon "aetherrelay/internal/modules/blocks/usageruntime/pkg/common"
	usageevents "aetherrelay/internal/modules/blocks/usageruntime/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestFeatureTurnPreservesSafeTLSFailureClass(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	_ = wireBootstrap(hub, t)
	usageObserver := event.NewSimpleObserver(usagecommon.UnitID, hub)
	usageObserver.Subscribe(usageevents.TopicAcquire, func(_ event.Event, result event.Result) {
		result.Set(usageevents.AcquireResult{}, nil)
	})
	proxyObserver := event.NewSimpleObserver(proxycommon.UnitID, hub)
	proxyObserver.Subscribe(proxyevents.TopicExecuteFeatureText, func(ev event.Event, result event.Result) {
		command := ev.Data().(proxyevents.ExecuteFeatureTextCommand)
		if !command.WebSearch {
			t.Fatalf("command=%+v", command)
		}
		result.Set(proxyevents.ExecuteFeatureTextResult{Provider: "chatgptweb", ErrorClass: "tls"}, cd.NewError(cd.Unexpected, "upstream request failed"))
	})

	temporary, err := New(context.Background(), hub, background)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer temporary.Teardown(context.Background())
	conversation, createErr := temporary.store.CreateConversation("ops", "gpt-5", "", "", "chatgptweb", "")
	if createErr != nil {
		t.Fatal(createErr)
	}
	value, err := temporary.SendEvent(event.NewEvent(events.TopicStartTurn, "test", tempcommon.UnitID, nil, events.StartTurnCommand{
		OwnerID: "ops", ConversationID: conversation.ID, Content: "latest news", WebSearch: true,
	})).Get()
	if err != nil {
		t.Fatal(err)
	}
	started, ok := value.(events.StartTurnResult)
	if !ok {
		t.Fatalf("start result=%+v", value)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		value, err = temporary.SendEvent(event.NewEvent(events.TopicPullTurn, "test", tempcommon.UnitID, nil, events.PullTurnCommand{
			OwnerID: "ops", ConversationID: conversation.ID, TurnID: started.TurnID, TimeoutMillis: 250,
		})).Get()
		if err != nil {
			t.Fatal(err)
		}
		update, ok := value.(events.PullTurnResult)
		if !ok {
			t.Fatalf("pull result=%+v", value)
		}
		if !update.Done {
			continue
		}
		if update.ErrorClass != "tls" || update.ErrorMessage != "upstream TLS connection failed" || update.Message == nil || update.Message.ErrorClass != "tls" {
			t.Fatalf("update=%+v", update)
		}
		return
	}
	t.Fatal("feature turn did not complete")
}
