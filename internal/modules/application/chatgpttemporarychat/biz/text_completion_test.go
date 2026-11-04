package biz

import (
	acccommon "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	tempcommon "aetherrelay/internal/modules/application/chatgpttemporarychat/pkg/common"
	tempevents "aetherrelay/internal/modules/application/chatgpttemporarychat/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	usagecommon "aetherrelay/internal/modules/blocks/usageruntime/pkg/common"
	usageevents "aetherrelay/internal/modules/blocks/usageruntime/pkg/events"
	completion "aetherrelay/internal/pkg/aetherrelaycompletion"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
	"context"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
	"sync/atomic"
	"testing"
	"time"
)

func textEvidence(text string) completion.Text {
	return completion.Text{Completed: true, Reason: completion.Stop, Terminal: "done", TextBytes: len(text), OutputBytes: len(text), TextSHA256: completion.Hash(text)}
}

func TestTemporaryTextCompletionPersistenceAndNoReplay(t *testing.T) {
	for _, state := range []string{"complete", "unknown", "protocol", "budget", "incomplete", "submitted_auth", "unknown_auth", "partial_auth", "length", "missing_anchors"} {
		t.Run(state, func(t *testing.T) {
			hub := event.NewHub(16)
			background := task.NewBackgroundRoutine(8)
			defer hub.Terminate(context.Background())
			defer background.Shutdown(nil)
			_ = wireBootstrap(hub, t)
			completed := make(chan usage.CompleteRecord, 1)
			usageObserver := event.NewSimpleObserver(usagecommon.UnitID, hub)
			usageObserver.Subscribe(usageevents.TopicAcquire, func(_ event.Event, r event.Result) { r.Set(usageevents.AcquireResult{}, nil) })
			usageObserver.Subscribe(usageevents.TopicStart, func(_ event.Event, r event.Result) { r.Set(nil, nil) })
			usageObserver.Subscribe(usageevents.TopicComplete, func(ev event.Event, r event.Result) {
				completed <- ev.Data().(usageevents.CompleteCommand).Record
				r.Set(nil, nil)
			})
			var starts, refreshes, successFeedback, failedFeedback atomic.Int32
			accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
			accounts.Subscribe(accevents.TopicAcquireTextAccount, func(_ event.Event, r event.Result) {
				r.Set(accevents.AcquireTextAccountResult{AccessToken: "test", Account: accevents.AccountView{ID: "account", Status: "正常"}}, nil)
			})
			accounts.Subscribe(accevents.TopicAcquireTextToken, func(_ event.Event, r event.Result) {
				r.Set(accevents.AcquireTextTokenResult{AccessToken: "test", Account: accevents.AccountView{ID: "account", Status: "正常"}}, nil)
			})
			accounts.Subscribe(accevents.TopicRefreshTextToken, func(_ event.Event, r event.Result) { refreshes.Add(1); r.Set(accevents.RefreshTextTokenResult{}, nil) })
			accounts.Subscribe(accevents.TopicRecordTextResult, func(ev event.Event, r event.Result) {
				if ev.Data().(accevents.RecordTextResultCommand).Success {
					successFeedback.Add(1)
				} else {
					failedFeedback.Add(1)
				}
				r.Set(accevents.RecordTextResultResult{}, nil)
			})
			upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
			upstream.Subscribe(upevents.TopicStartText, func(_ event.Event, r event.Result) {
				starts.Add(1)
				r.Set(upevents.StartTextResult{StreamID: "stream"}, nil)
			})
			wantText := "AB"
			if state == "submitted_auth" || state == "unknown_auth" {
				wantText = ""
			}
			var pulls atomic.Int32
			upstream.Subscribe(upevents.TopicPullText, func(_ event.Event, r event.Result) {
				if wantText != "" && pulls.Add(1) == 1 {
					delta := upevents.PullTextResult{Delta: wantText, ConversationID: "c", AssistantMessageID: "a"}
					if state == "missing_anchors" {
						delta.ConversationID = ""
						delta.AssistantMessageID = ""
					}
					r.Set(delta, nil)
					return
				}
				out := upevents.PullTextResult{Done: true, ConversationID: "c", AssistantMessageID: "a"}
				switch state {
				case "missing_anchors":
					out.Completion = textEvidence(wantText)
					out.ConversationID = ""
					out.AssistantMessageID = ""
				case "length":
					out.ErrorClass = upevents.ErrClassTextModelLength
				case "complete":
					out.Completion = textEvidence(wantText)
				case "protocol":
					out.ErrorClass = upevents.ErrClassTextProtocol
				case "budget":
					out.ErrorClass = upevents.ErrClassTextBudget
				case "incomplete":
					out.ErrorClass = upevents.ErrClassTextIncomplete
				case "submitted_auth", "partial_auth":
					out.ErrorClass = upevents.ErrClassInvalidToken
				case "unknown_auth":
					out.ErrorClass = upevents.ErrClassInvalidToken
					out.ConversationID = ""
					out.AssistantMessageID = ""
				}
				r.Set(out, nil)
			})
			upstream.Subscribe(upevents.TopicCancelText, func(_ event.Event, r event.Result) { r.Set(upevents.CancelTextResult{}, nil) })
			temporary, e := New(context.Background(), hub, background)
			if e != nil {
				t.Fatal(e)
			}
			defer temporary.Teardown(context.Background())
			conversation, err := temporary.store.CreateConversation("ops", "gpt-5", "", "", "chatgptweb", "account")
			if err != nil {
				t.Fatal(err)
			}
			value, eventErr := temporary.SendEvent(event.NewEvent(tempevents.TopicStartTurn, "test", tempcommon.UnitID, nil, tempevents.StartTurnCommand{OwnerID: "ops", ConversationID: conversation.ID, Content: "hi"})).Get()
			if eventErr != nil {
				t.Fatal(eventErr)
			}
			started := value.(tempevents.StartTurnResult)
			var record usage.CompleteRecord
			select {
			case record = <-completed:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not finish")
			}
			detail, err := temporary.store.GetConversation("ops", conversation.ID, nil, 20)
			if err != nil || len(detail.Messages) != 2 {
				t.Fatal(detail, err)
			}
			message := detail.Messages[1]
			success := state == "complete"
			if message.Content != wantText || (message.Status == tempcommon.MessageStatusCompleted) != success || (record.Outcome == "success") != success {
				t.Fatalf("message=%+v usage=%+v", message, record)
			}
			if !success {
				if detail.Conversation.Status != tempcommon.StatusRecoveryRequired || record.ErrorCode == "process_interrupted" {
					t.Fatalf("lost recovery or misclassified failure: %+v %+v", detail.Conversation, record)
				}
				if _, err := temporary.store.StartTurn("ops", conversation.ID, "must require recovery"); err == nil {
					t.Fatal("allowed normal continuation")
				}
			}
			var wantSuccessFeedback, wantFailedFeedback int32
			if success {
				wantSuccessFeedback = 1
			}
			if state == "submitted_auth" || state == "unknown_auth" || state == "partial_auth" {
				wantFailedFeedback = 1
			}
			if starts.Load() != 1 || refreshes.Load() != 0 || successFeedback.Load() != wantSuccessFeedback || failedFeedback.Load() != wantFailedFeedback {
				t.Fatal("unsafe replay or feedback", starts.Load(), refreshes.Load(), successFeedback.Load(), failedFeedback.Load())
			}
			// Polling must agree with the persisted assistant message, including after
			// worker cleanup removes the in-memory runtime.
			for i := 0; i < 3; i++ {
				value, eventErr = temporary.SendEvent(event.NewEvent(tempevents.TopicPullTurn, "test", tempcommon.UnitID, nil, tempevents.PullTurnCommand{OwnerID: "ops", ConversationID: conversation.ID, TurnID: started.TurnID, TimeoutMillis: 250})).Get()
				if eventErr != nil {
					t.Fatal(eventErr)
				}
				update := value.(tempevents.PullTurnResult)
				if update.Done {
					if update.Message == nil || update.Message.Status != message.Status || update.Message.Content != wantText {
						t.Fatal(update)
					}
					return
				}
			}
			t.Fatal("no terminal turn update")
		})
	}
}
