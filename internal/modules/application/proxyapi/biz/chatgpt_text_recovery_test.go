package biz

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	acccommon "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	"aetherrelay/internal/modules/application/proxyapi/pkg/usagetarget"
	basebiz "aetherrelay/internal/modules/base/biz"
	upcommon "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

// CP-FAIL-021, CP-OBS-017: actual execution and feedback, through both EventHub paths.
func TestTextRecoveryBeforeGenerationOnly(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, tc := range []struct {
			name, operation                                                                                             string
			status                                                                                                      int
			class                                                                                                       upevents.ErrorClass
			notSent, partial, noAlternate, secondFails, shortBudget, refreshedFirst, cancelDuring, budgetWhileSelecting bool
			want                                                                                                        int
		}{
			{name: "bootstrap403", operation: "bootstrap", status: 403, class: upevents.ErrClassUpstream, notSent: true, want: 2},
			{name: "prepare502", operation: "requirements_prepare", status: 502, class: upevents.ErrClassUpstream, notSent: true, want: 2},
			{name: "prepareTLS", operation: "bootstrap", class: upevents.ErrClassTLS, notSent: true, want: 2},
			{name: "unknownSubmission", operation: "bootstrap", status: 403, class: upevents.ErrClassUpstream, want: 1},
			{name: "sentWithoutOutput", operation: "text_conversation", status: 403, class: upevents.ErrClassUpstream, want: 1},
			{name: "unknownStage", status: 403, class: upevents.ErrClassUpstream, notSent: true, want: 1},
			{name: "entitlementDenial", operation: "requirements_prepare", status: 403, class: upevents.ErrClassUpstream, notSent: true, want: 1},
			{name: "rateLimit", operation: "bootstrap", status: 429, class: upevents.ErrClassRateLimit, notSent: true, want: 1},
			{name: "partialOutput", operation: "bootstrap", status: 403, class: upevents.ErrClassUpstream, notSent: true, partial: true, want: 1},
			{name: "noAlternate", operation: "bootstrap", status: 403, class: upevents.ErrClassUpstream, notSent: true, noAlternate: true, want: 1},
			{name: "secondFailureStops", operation: "bootstrap", status: 403, class: upevents.ErrClassUpstream, notSent: true, secondFails: true, want: 2},
			{name: "canceledDuringPreparation", operation: "bootstrap", status: 403, class: upevents.ErrClassUpstream, notSent: true, cancelDuring: true, want: 1},
			{name: "budgetShrinksDuringSelection", operation: "bootstrap", status: 403, class: upevents.ErrClassUpstream, notSent: true, budgetWhileSelecting: true, want: 1},
			{name: "budgetTooShort", operation: "bootstrap", status: 403, class: upevents.ErrClassUpstream, notSent: true, shortBudget: true, want: 1},
			{name: "refreshConsumesBudget", operation: "bootstrap", status: 403, class: upevents.ErrClassUpstream, notSent: true, refreshedFirst: true, secondFails: true, want: 2},
		} {
			t.Run(fmt.Sprintf("%t/%s", streaming, tc.name), func(t *testing.T) {
				hub := event.NewHub(8)
				background := task.NewBackgroundRoutine(8)
				defer hub.Terminate(context.Background())
				defer background.Shutdown(nil)
				accountOwner := event.NewSimpleObserver(acccommon.UnitID, hub)
				acquisitions, calls, refreshes, removals := 0, 0, 0, 0
				var feedback []accevents.RecordTextResultCommand
				var cancelAttempt context.CancelFunc
				accountOwner.Subscribe(accevents.TopicAcquireTextToken, func(ev event.Event, result event.Result) {
					cmd := ev.Data().(accevents.AcquireTextTokenCommand)
					if cmd.Model != "gpt-6" {
						t.Errorf("model changed: %s", cmd.Model)
					}
					acquisitions++
					id := "first"
					proxy := "http://first.invalid:8080"
					if acquisitions > 1 {
						if len(feedback) == 0 {
							t.Error("alternate acquired before recording first failure")
						}
						if tc.budgetWhileSelecting {
							time.Sleep(600 * time.Millisecond)
						}
						if len(cmd.Exclude) == 0 || cmd.Exclude[0] != "first" {
							t.Errorf("missing failed token exclusion: %+v", cmd)
						}
						if tc.noAlternate {
							result.Set(nil, cd.NewError(cd.NotFound, "no candidate"))
							return
						}
						id, proxy = "second", "http://second.invalid:8080"
					}
					result.Set(accevents.AcquireTextTokenResult{AccessToken: id, Account: accevents.AccountView{ID: id, Email: id + "@example.com", Proxy: proxy}}, nil)
				})
				accountOwner.Subscribe(accevents.TopicRecordTextResult, func(ev event.Event, result event.Result) {
					feedback = append(feedback, ev.Data().(accevents.RecordTextResultCommand))
					result.Set(accevents.RecordTextResultResult{}, nil)
				})
				accountOwner.Subscribe(accevents.TopicRefreshTextToken, func(ev event.Event, result event.Result) {
					refreshes++
					result.Set(accevents.RefreshTextTokenResult{Refreshed: true, AccessToken: "refreshed", Account: accevents.AccountView{ID: "first", Email: "first@example.com"}}, nil)
				})
				accountOwner.Subscribe(accevents.TopicRemoveInvalid, func(ev event.Event, result event.Result) {
					removals++
					result.Set(accevents.RemoveInvalidResult{}, nil)
				})
				upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
				response := func() upevents.CompleteTextResult {
					calls++
					if tc.cancelDuring {
						cancelAttempt()
					}
					if calls == 2 && !tc.secondFails {
						return upevents.CompleteTextResult{Text: "recovered", ActualModel: "gpt-6", UpstreamStatus: 206}
					}
					r := upevents.CompleteTextResult{ErrorClass: tc.class, ErrorOperation: tc.operation, UpstreamStatus: tc.status, GenerationNotSent: tc.notSent}
					if tc.partial {
						r.Text = "already emitted"
						r.ConversationID = "existing"
					}
					if tc.refreshedFirst && calls == 1 {
						r.ErrorClass = upevents.ErrClassInvalidToken
						r.UpstreamStatus = 401
					}
					return r
				}
				upstream.Subscribe(upevents.TopicCompleteText, func(ev event.Event, result event.Result) {
					cmd := ev.Data().(upevents.CompleteTextCommand)
					if cmd.Model != "gpt-6" {
						t.Error("model changed")
					}
					r := response()
					if r.ErrorClass != "" {
						result.Set(r, cd.NewError(cd.Unexpected, "classified failure"))
					} else {
						result.Set(r, nil)
					}
				})
				upstream.Subscribe(upevents.TopicStartText, func(ev event.Event, result event.Result) {
					result.Set(upevents.StartTextResult{StreamID: "stream"}, nil)
				})
				upstream.Subscribe(upevents.TopicPullText, func(ev event.Event, result event.Result) {
					r := response()
					result.Set(upevents.PullTextResult{Done: true, Delta: r.Text, ConversationID: r.ConversationID, ErrorClass: r.ErrorClass, ErrorOperation: r.ErrorOperation, UpstreamStatus: r.UpstreamStatus, GenerationNotSent: r.GenerationNotSent}, nil)
				})
				upstream.Subscribe(upevents.TopicCancelText, func(ev event.Event, result event.Result) { result.Set(upevents.CancelTextResult{}, nil) })
				proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
				baseCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				cancelAttempt = cancel
				ctx := usagetarget.WithTracker(baseCtx)
				if tc.budgetWhileSelecting {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 1500*time.Millisecond)
					defer cancel()
				}
				if tc.shortBudget {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 800*time.Millisecond)
					defer cancel()
				}
				request := chatgpttext.Request{Model: "gpt-6", Messages: []chatgpttext.Message{{Role: "user", Content: "hi"}}}
				var out chatgpttext.Result
				var err error
				if streaming {
					out, err = proxy.Stream(ctx, request, func(chatgpttext.Delta) error { return nil })
				} else {
					out, err = proxy.Complete(ctx, request)
				}
				if calls != tc.want {
					t.Fatalf("calls=%d want=%d err=%v", calls, tc.want, err)
				}
				recovered := tc.want == 2 && !tc.secondFails
				if recovered != (err == nil) {
					t.Fatalf("unexpected result=%+v err=%v", out, err)
				}
				if recovered && (out.Text != "recovered" || out.UpstreamStatus != 206) {
					t.Fatalf("out=%+v", out)
				}
				if tc.noAlternate {
					var f *chatgptfail.Failure
					if !errors.As(err, &f) || f.UpstreamStatus != 403 {
						t.Fatalf("original failure replaced: %v", err)
					}
				}
				if removals != 0 {
					t.Fatalf("403 credential removed: %d", removals)
				}
				wantRefresh := 0
				if tc.refreshedFirst {
					wantRefresh = 1
				}
				if refreshes != wantRefresh {
					t.Fatalf("refreshes=%d", refreshes)
				}
				wantFeedback := calls
				if tc.refreshedFirst {
					wantFeedback--
				}
				if tc.cancelDuring {
					wantFeedback = 0
				}
				if len(feedback) != wantFeedback {
					t.Fatalf("feedback=%+v", feedback)
				}
				target := usagetarget.Snapshot(ctx)
				if len(target.TextAttempts) != calls {
					t.Fatalf("trace=%+v", target.TextAttempts)
				}
				if recovered && (target.AccountID != "second" || !feedback[len(feedback)-1].Success) {
					t.Fatalf("final target/feedback=%+v %+v", target, feedback)
				}
				if calls == 2 && !tc.refreshedFirst && (feedback[0].Success || feedback[0].AccountID != "first") {
					t.Fatalf("lost first failure: %+v", feedback)
				}
			})
		}
	}
}

// CP-FAIL-021: candidate scanning is bounded and rotating credentials cannot reuse the failed slot.
func TestTextAlternativeSelectionBoundedAndAccountScoped(t *testing.T) {
	for _, sameAccount := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameAccount=%t", sameAccount), func(t *testing.T) {
			hub := event.NewHub(8)
			background := task.NewBackgroundRoutine(8)
			defer hub.Terminate(context.Background())
			defer background.Shutdown(nil)
			owner := event.NewSimpleObserver(acccommon.UnitID, hub)
			scans := 0
			owner.Subscribe(accevents.TopicAcquireTextToken, func(ev event.Event, result event.Result) {
				scans++
				id := fmt.Sprintf("candidate-%d", scans)
				if sameAccount {
					id = "failed"
				}
				command := ev.Data().(accevents.AcquireTextTokenCommand)
				if len(command.Exclude) != scans {
					t.Errorf("exclusion list failed to grow: %+v", command)
				}
				result.Set(accevents.AcquireTextTokenResult{AccessToken: fmt.Sprintf("token-%d", scans), Account: accevents.AccountView{ID: id, Proxy: "http://shared.invalid:8080"}}, nil)
			})
			proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
			got, err := proxy.acquireAlternativeTextAccount(context.Background(), "gpt-6", accevents.AcquireTextTokenResult{AccessToken: "failed-token", Account: accevents.AccountView{ID: "failed", Proxy: "http://shared.invalid:8080"}})
			if scans != 8 {
				t.Fatalf("unbounded/unexpected scans: %d", scans)
			}
			if sameAccount && err == nil {
				t.Fatalf("failed slot reused: %+v", got)
			}
			if !sameAccount && (err != nil || got.Account.ID != "candidate-1") {
				t.Fatalf("same-exit fallback lost: %+v %v", got, err)
			}
		})
	}
}
