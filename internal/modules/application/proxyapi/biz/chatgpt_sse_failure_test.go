package biz

import (
	"context"
	"errors"
	"strings"
	"testing"

	acccommon "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptsearch"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	basebiz "aetherrelay/internal/modules/base/biz"
	upcommon "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

// Exercise the existing account feedback and recovery policy, not just the
// parser: HTTP-200 SSE limits must cool the requested model without replay.
func TestSSELimitKeepsMessageAndDoesNotReplay(t *testing.T) {
	for _, flow := range []string{"complete", "stream", "search"} {
		for _, partial := range []bool{false, true} {
			name := flow + "/before_output"
			if partial {
				name = flow + "/after_output"
			}
			t.Run(name, func(t *testing.T) {
				hub := event.NewHub(8)
				background := task.NewBackgroundRoutine(8)
				defer hub.Terminate(context.Background())
				defer background.Shutdown(nil)
				acquires, calls, refreshes, removals := 0, 0, 0, 0
				var feedback []accevents.RecordTextResultCommand
				accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
				accounts.Subscribe(accevents.TopicAcquireTextToken, func(_ event.Event, result event.Result) {
					acquires++
					result.Set(accevents.AcquireTextTokenResult{AccessToken: "token", Account: accevents.AccountView{ID: "account", DeviceID: "stable-device"}}, nil)
				})
				accounts.Subscribe(accevents.TopicRefreshTextToken, func(_ event.Event, result event.Result) {
					refreshes++
					result.Set(accevents.RefreshTextTokenResult{}, cd.NewError(cd.Unexpected, "must not refresh a quota failure"))
				})
				accounts.Subscribe(accevents.TopicRemoveInvalid, func(_ event.Event, result event.Result) {
					removals++
					result.Set(accevents.RemoveInvalidResult{}, nil)
				})
				accounts.Subscribe(accevents.TopicRecordTextResult, func(ev event.Event, result event.Result) {
					feedback = append(feedback, ev.Data().(accevents.RecordTextResultCommand))
					result.Set(accevents.RecordTextResultResult{}, nil)
				})
				const message = "text_conversation: rate_limit: usage_limit: 你已达到限额。请稍后重试。"
				text := ""
				if partial {
					text = "partial"
				}
				upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
				upstream.Subscribe(upevents.TopicCompleteText, func(_ event.Event, result event.Result) {
					calls++
					result.Set(upevents.CompleteTextResult{ConversationID: "conversation", Text: text, ErrorClass: upevents.ErrClassRateLimit, ErrorOperation: "text_conversation", UpstreamStatus: 200}, cd.NewError(cd.Unexpected, message))
				})
				upstream.Subscribe(upevents.TopicSearch, func(_ event.Event, result event.Result) {
					calls++
					result.Set(upevents.SearchResult{ConversationID: "conversation", Text: text, ErrorClass: upevents.ErrClassRateLimit, ErrorOperation: "search_conversation"}, cd.NewError(cd.Unexpected, message))
				})
				upstream.Subscribe(upevents.TopicStartText, func(_ event.Event, result event.Result) {
					calls++
					result.Set(upevents.StartTextResult{StreamID: "stream"}, nil)
				})
				pulls := 0
				upstream.Subscribe(upevents.TopicPullText, func(_ event.Event, result event.Result) {
					pulls++
					if partial && pulls == 1 {
						result.Set(upevents.PullTextResult{ConversationID: "conversation", Delta: text}, nil)
						return
					}
					result.Set(upevents.PullTextResult{Done: true, ConversationID: "conversation", ErrorClass: upevents.ErrClassRateLimit, ErrorMessage: message, ErrorOperation: "text_conversation", UpstreamStatus: 200}, nil)
				})
				upstream.Subscribe(upevents.TopicCancelText, func(_ event.Event, result event.Result) { result.Set(upevents.CancelTextResult{}, nil) })
				proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
				request := chatgpttext.Request{Model: "gpt-6", Messages: []chatgpttext.Message{{Role: "user", Content: "hello"}}}
				var err error
				var gotText, conversationID string
				emitted := ""
				if flow == "search" {
					out, failure := proxy.Search(context.Background(), chatgptsearch.Request{Model: "gpt-6", Query: "search"})
					err, gotText, conversationID = failure, out.Text, out.ConversationID
				} else {
					var out chatgpttext.Result
					if flow == "stream" {
						out, err = proxy.Stream(context.Background(), request, func(d chatgpttext.Delta) error { emitted += d.Text; return nil })
					} else {
						out, err = proxy.Complete(context.Background(), request)
					}
					gotText, conversationID = out.Text, out.ConversationID
					if out.UpstreamStatus != 200 {
						t.Fatalf("fabricated upstream status: %+v", out)
					}
				}
				var failure *chatgptfail.Failure
				if !errors.As(err, &failure) || failure.Kind != chatgptfail.KindRateLimit || !strings.Contains(err.Error(), "usage_limit") || !strings.Contains(err.Error(), "你已达到限额") {
					t.Fatalf("lost classification or cause: %v", err)
				}
				if gotText != text || conversationID != "conversation" || (flow == "stream" && emitted != text) {
					t.Fatalf("lost partial state: text=%q emitted=%q conversation=%s", gotText, emitted, conversationID)
				}
				if calls != 1 || acquires != 1 || refreshes != 0 || removals != 0 {
					t.Fatalf("unsafe recovery: calls=%d acquires=%d refreshes=%d removals=%d", calls, acquires, refreshes, removals)
				}
				if len(feedback) != 1 || feedback[0].Success || feedback[0].Model != "gpt-6" || feedback[0].ErrorClass != "rate_limit" {
					t.Fatalf("incorrect cooldown feedback: %+v", feedback)
				}
			})
		}
	}
}
