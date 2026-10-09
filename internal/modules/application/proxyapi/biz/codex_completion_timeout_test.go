package biz

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestCodexCompletionBudgetIncludesAdmissionAndRetries(t *testing.T) {
	for _, phase := range []string{"admission", "read", "retry", "refresh", "client_cancel", "adapter_deadline"} {
		t.Run(phase, func(t *testing.T) {
			hub := event.NewHub(24)
			background := task.NewBackgroundRoutine(8)
			t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
			accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
			upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
			var acquired, calls, released, feedback atomic.Int32
			var firstDeadline time.Time
			accounts.Subscribe(accevents.TopicAcquire, func(ev event.Event, result event.Result) {
				acquired.Add(1)
				if phase == "admission" {
					<-ev.Context().Done()
					result.Set(nil, nil)
					return
				}
				result.Set(accevents.AcquireResult{AccountID: "account", AccessToken: "test", LeaseID: "lease"}, nil)
			})
			accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, result event.Result) { released.Add(1); result.Set(accevents.ReleaseResult{}, nil) })
			accounts.Subscribe(accevents.TopicRecordResult, func(_ event.Event, result event.Result) {
				feedback.Add(1)
				result.Set(accevents.RecordResultResult{}, nil)
			})
			accounts.Subscribe(accevents.TopicRefreshToken, func(ev event.Event, result event.Result) {
				<-ev.Context().Done()
				result.Set(accevents.RefreshTokenResult{ErrorClass: accevents.ErrorTimeout}, nil)
			})
			upstream.Subscribe(upevents.TopicComplete, func(ev event.Event, result event.Result) {
				n := calls.Add(1)
				deadline, _ := ev.Context().Deadline()
				if n == 1 {
					firstDeadline = deadline
				} else if deadline != firstDeadline {
					t.Error("retry restarted request budget")
				}
				attempt := upevents.HTTPAttempt{Request: upevents.HTTPRequestObservation{At: time.Now(), URL: "https://example.test/responses"}, Response: upevents.HTTPResponseObservation{Observed: true, Status: 200, EventCount: 3, WireBytes: 512, LastEventAt: time.Now()}}
				if phase == "refresh" {
					result.Set(upevents.CompleteResult{Attempt: attempt, ErrorClass: upevents.ErrorInvalidToken}, nil)
					return
				}
				if phase == "retry" && n == 1 {
					result.Set(upevents.CompleteResult{Attempt: attempt, ErrorClass: upevents.ErrorNetwork}, nil)
					return
				}
				<-ev.Context().Done()
				result.Set(upevents.CompleteResult{Attempt: attempt, ErrorClass: upevents.ErrorTimeout}, nil)
			})
			proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background), config: config.Config{RequestTimeout: 80 * time.Millisecond}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := codexresponses.Request{Model: "gpt-test", Body: []byte(`{"model":"gpt-test","input":[]}`)}
			if phase == "adapter_deadline" {
				request.Deadline = time.Now().Add(30 * time.Millisecond)
			}
			if phase == "client_cancel" {
				timer := time.AfterFunc(20*time.Millisecond, cancel)
				defer timer.Stop()
			}
			observedKind := codexresponses.ErrorKind("")
			request.ObserveAttempt = func(_ codexresponses.HTTPAttempt, err error) {
				if f, ok := codexresponses.AsFailure(err); ok {
					observedKind = f.Kind
				}
			}
			_, err := proxy.CompleteCodexResponses(ctx, request)
			failure, ok := codexresponses.AsFailure(err)
			want := codexresponses.KindTimeout
			if phase == "client_cancel" {
				want = codexresponses.KindClientCanceled
			}
			if !ok || failure.Kind != want || (phase != "client_cancel" && !failure.RequestBudgetExceeded) {
				t.Fatalf("failure=%+v err=%v", failure, err)
			}
			if phase != "admission" && (failure.Attempt.Response.Status != 200 || failure.Attempt.Response.EventCount != 3 || (phase != "refresh" && observedKind != want) || released.Load() != acquired.Load()) {
				t.Fatalf("lost settlement: failure=%+v observed=%s released=%d acquired=%d", failure, observedKind, released.Load(), acquired.Load())
			}
			expectedFeedback := int32(0)
			if phase == "retry" {
				expectedFeedback = 1
				if calls.Load() != 2 {
					t.Fatalf("calls=%d", calls.Load())
				}
			}
			if feedback.Load() != expectedFeedback {
				t.Fatalf("local timeout/cancel penalized account: feedback=%d", feedback.Load())
			}
		})
	}
}
