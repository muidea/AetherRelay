package biz

import (
	"context"
	"testing"
	"time"

	tempcommon "aetherrelay/internal/modules/application/chatgpttemporarychat/pkg/common"
	"aetherrelay/internal/modules/application/chatgpttemporarychat/pkg/events"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestFeatureExecutorPersistsCompletionAndPropagatesCancellation(t *testing.T) {
	for _, cancelTurn := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "cancelled"}[cancelTurn], func(t *testing.T) {
			hub := event.NewHub(16)
			background := task.NewBackgroundRoutine(8)
			defer hub.Terminate(context.Background())
			defer background.Shutdown(nil)
			wireBootstrap(hub, t)
			called := make(chan struct{})
			proxy := event.NewSimpleObserver(proxycommon.UnitID, hub)
			proxy.Subscribe(proxyevents.TopicExecuteFeatureText, func(ev event.Event, result event.Result) {
				command := ev.Data().(proxyevents.ExecuteFeatureTextCommand)
				if command.OwnerID != "ops" || command.Model != "gpt-5" || len(command.Messages) != 2 || command.Messages[0].Content != "be concise" || command.Messages[1].Content != "hello" {
					t.Errorf("command=%+v", command)
				}
				close(called)
				if cancelTurn {
					<-ev.Context().Done()
					result.Set(nil, cd.NewError(cd.Unexpected, "cancelled"))
					return
				}
				result.Set(proxyevents.ExecuteFeatureTextResult{Provider: "chatgptweb", Text: "answer", ActualModel: "gpt-5-actual"}, nil)
			})
			temporary, err := New(context.Background(), hub, background)
			if err != nil {
				t.Fatal(err)
			}
			defer temporary.Teardown(context.Background())
			conversation, createErr := temporary.store.CreateConversation("ops", "gpt-5", "", "be concise", "chatgptweb")
			if createErr != nil {
				t.Fatal(createErr)
			}
			value, err := temporary.SendEvent(event.NewEvent(events.TopicStartTurn, "test", tempcommon.UnitID, nil, events.StartTurnCommand{OwnerID: "ops", ConversationID: conversation.ID, Content: "hello"})).Get()
			if err != nil {
				t.Fatal(err)
			}
			started := value.(events.StartTurnResult)
			select {
			case <-called:
			case <-time.After(3 * time.Second):
				t.Fatal("current executor was not called")
			}
			if cancelTurn {
				if _, err := temporary.SendEvent(event.NewEvent(events.TopicCancelTurn, "test", tempcommon.UnitID, nil, events.CancelTurnCommand{OwnerID: "ops", ConversationID: conversation.ID, TurnID: started.TurnID})).Get(); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				value, err := temporary.SendEvent(event.NewEvent(events.TopicPullTurn, "test", tempcommon.UnitID, nil, events.PullTurnCommand{OwnerID: "ops", ConversationID: conversation.ID, TurnID: started.TurnID, TimeoutMillis: 250})).Get()
				if err != nil {
					t.Fatal(err)
				}
				update := value.(events.PullTurnResult)
				if !update.Done {
					continue
				}
				if update.Message == nil {
					t.Fatalf("terminal message missing: %+v", update)
				}
				if cancelTurn {
					if update.Message.Status != events.MessageStatusCancelled || update.ErrorClass != "cancelled" {
						t.Fatalf("cancellation not persisted: %+v", update)
					}
				} else if update.Message.Content != "answer" || update.Message.Status != events.MessageStatusCompleted || update.Message.ActualModel != "gpt-5-actual" {
					t.Fatalf("completion not persisted: %+v", update)
				}
				return
			}
			t.Fatal("turn did not finish")
		})
	}
}
