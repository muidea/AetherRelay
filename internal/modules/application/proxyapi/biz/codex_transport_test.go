package biz

import (
	"context"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestUncertainWebsocketResponseCannotRetryAccount(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "buffered"
		if streaming {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			hub, background := event.NewHub(32), task.NewBackgroundRoutine(8)
			t.Cleanup(func() { background.Shutdown(context.Background()); hub.Terminate(context.Background()) })
			accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
			acquires, releases := 0, 0
			accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, result event.Result) {
				acquires++
				if acquires > 1 {
					result.Set(accevents.AcquireResult{UnavailableReason: "accounts_busy"}, nil)
					return
				}
				result.Set(accevents.AcquireResult{AccountID: "account", AccessToken: "test-token", LeaseID: "lease"}, nil)
			})
			accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, result event.Result) {
				releases++
				result.Set(accevents.ReleaseResult{Released: true}, nil)
			})
			accounts.Subscribe(accevents.TopicRecordResult, func(_ event.Event, result event.Result) { result.Set(accevents.RecordResultResult{}, nil) })
			upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
			upstream.Subscribe(upevents.TopicComplete, func(ev event.Event, result event.Result) {
				cmd := ev.Data().(upevents.CompleteCommand)
				if !cmd.PreferWebsocket {
					t.Error("responses did not prefer WS")
				}
				result.Set(upevents.CompleteResult{ErrorClass: upevents.ErrorNetwork, Attempt: upevents.HTTPAttempt{Response: upevents.HTTPResponseObservation{Transport: "websocket", WebsocketRequestSent: true}}}, nil)
			})
			upstream.Subscribe(upevents.TopicStart, func(ev event.Event, result event.Result) {
				if !ev.Data().(upevents.StartCommand).PreferWebsocket {
					t.Error("stream did not prefer WS")
				}
				result.Set(upevents.StartResult{StreamID: "stream", Attempt: upevents.HTTPAttempt{Request: upevents.HTTPRequestObservation{URL: "wss://upstream.test/responses"}, Response: upevents.HTTPResponseObservation{Transport: "websocket", WebsocketRequestSent: true}}}, nil)
			})
			upstream.Subscribe(upevents.TopicPull, func(_ event.Event, result event.Result) {
				result.Set(upevents.PullResult{Done: true, ErrorClass: upevents.ErrorNetwork}, nil)
			})
			upstream.Subscribe(upevents.TopicCancel, func(_ event.Event, result event.Result) { result.Set(upevents.CancelResult{Cancelled: true}, nil) })
			proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
			request := codexresponses.Request{Model: "gpt-test", Body: []byte(`{"model":"gpt-test","input":[]}`)}
			var err error
			if streaming {
				err = proxy.StreamCodexResponses(context.Background(), request, nil, nil)
			} else {
				_, err = proxy.CompleteCodexResponses(context.Background(), request)
			}
			failure, ok := codexresponses.AsFailure(err)
			if !ok || failure.Retryable == nil || *failure.Retryable || acquires != 1 || releases != 1 {
				t.Fatalf("failure=%+v acquires=%d releases=%d", failure, acquires, releases)
			}
		})
	}
}
