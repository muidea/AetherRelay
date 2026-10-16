package biz

import (
	"context"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	"aetherrelay/internal/modules/application/proxyapi/pkg/usagetarget"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestCodexModelBlacklistPreventsRefreshResend(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, alternate := range []bool{false, true} {
			for _, stateful := range []bool{false, true} {
				t.Run(map[bool]string{false: "unary", true: "stream"}[streaming]+map[bool]string{false: "/denied", true: "/alternate"}[alternate]+map[bool]string{true: "/stateful"}[stateful], func(t *testing.T) {
					hub := event.NewHub(24)
					background := task.NewBackgroundRoutine(8)
					t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
					accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
					blocked := false
					attempts, releases, feedback, acquires := 0, 0, 0, 0
					accounts.Subscribe(accevents.TopicAcquire, func(e event.Event, r event.Result) {
						acquires++
						if e.Data().(accevents.AcquireCommand).Model != "gpt-sol" {
							t.Error("model changed")
						}
						if blocked && !alternate {
							r.Set(accevents.AcquireResult{UnavailableReason: "model_blocked"}, cd.NewError(cd.NotFound, "blocked"))
							return
						}
						id := "primary"
						if blocked {
							id = "alternate"
						}
						r.Set(accevents.AcquireResult{AccountID: id, AccessToken: id, LeaseID: id}, nil)
					})
					accounts.Subscribe(accevents.TopicRefreshToken, func(_ event.Event, r event.Result) {
						blocked = true
						r.Set(accevents.RefreshTokenResult{AccountID: "primary", AccessToken: "refreshed", Refreshed: true}, nil)
					})
					accounts.Subscribe(accevents.TopicCheckModelPolicy, func(e event.Event, r event.Result) {
						cmd := e.Data().(accevents.CheckModelPolicyCommand)
						if cmd.AccountID != "primary" || cmd.Model != "gpt-sol" {
							t.Error("wrong policy target", cmd)
						}
						r.Set(accevents.CheckModelPolicyResult{Reason: "model_blocked"}, nil)
					})
					accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, r event.Result) { releases++; r.Set(accevents.ReleaseResult{Released: true}, nil) })
					accounts.Subscribe(accevents.TopicRecordResult, func(e event.Event, r event.Result) {
						feedback++
						if e.Data().(accevents.RecordResultCommand).AccountID == "primary" {
							t.Error("policy denial penalized account")
						}
						r.Set(accevents.RecordResultResult{}, nil)
					})
					up := event.NewSimpleObserver(upcommon.UnitID, hub)
					up.Subscribe(upevents.TopicComplete, func(e event.Event, r event.Result) {
						attempts++
						token := e.Data().(upevents.CompleteCommand).AccessToken
						if token == "refreshed" {
							t.Error("blocked account resent")
						}
						if token == "primary" {
							r.Set(upevents.CompleteResult{ErrorClass: upevents.ErrorInvalidToken}, nil)
							return
						}
						r.Set(upevents.CompleteResult{Body: []byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)}, nil)
					})
					up.Subscribe(upevents.TopicStart, func(e event.Event, r event.Result) {
						attempts++
						token := e.Data().(upevents.StartCommand).AccessToken
						if token == "refreshed" {
							t.Error("blocked stream resent")
						}
						if token == "primary" {
							r.Set(upevents.StartResult{ErrorClass: upevents.ErrorInvalidToken}, nil)
							return
						}
						r.Set(upevents.StartResult{StreamID: "stream"}, nil)
					})
					up.Subscribe(upevents.TopicPull, func(_ event.Event, r event.Result) {
						r.Set(upevents.PullResult{Done: true, Data: []byte(`data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`)}, nil)
					})
					up.Subscribe(upevents.TopicCancel, func(_ event.Event, r event.Result) { r.Set(upevents.CancelResult{Cancelled: true}, nil) })
					proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
					req := codexresponses.Request{Model: "gpt-sol", Body: []byte(`{"model":"gpt-sol"}`)}
					if stateful {
						req.Body = []byte(`{"model":"gpt-sol","previous_response_id":"upstream-owned"}`)
					}
					var err error
					if streaming {
						err = proxy.StreamCodexResponses(context.Background(), req, nil, func([]byte) error { return nil })
					} else {
						_, err = proxy.CompleteCodexResponses(context.Background(), req)
					}
					if alternate && !stateful {
						if err != nil || attempts != 2 || releases != 2 || feedback != 1 {
							t.Fatalf("attempts=%d release=%d feedback=%d err=%v", attempts, releases, feedback, err)
						}
					} else {
						f, ok := codexresponses.AsFailure(err)
						if !ok || f.UnavailableReason != "model_blocked" || f.Retryable == nil || *f.Retryable || attempts != 1 || releases != 1 || feedback != 0 {
							t.Fatalf("denial attempts=%d release=%d feedback=%d err=%v", attempts, releases, feedback, err)
						}
					}
					expectedAcquires := 2
					if stateful {
						expectedAcquires = 1
					}
					if acquires != expectedAcquires {
						t.Fatalf("acquires=%d expected=%d", acquires, expectedAcquires)
					}
				})
			}
		}
	}
}

func TestCodexWebsocketChecksPolicyEveryTurnAndFailsClosed(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocked_next_turn", true: "policy_unavailable"}[unavailable], func(t *testing.T) {
			hub := event.NewHub(24)
			background := task.NewBackgroundRoutine(8)
			t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
			accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
			blocked := false
			sends, closes, releases, feedback := 0, 0, 0, 0
			accounts.Subscribe(accevents.TopicCheckModelPolicy, func(_ event.Event, r event.Result) {
				if blocked && unavailable {
					r.Set(nil, cd.NewError(cd.Unexpected, "policy unavailable"))
					return
				}
				r.Set(accevents.CheckModelPolicyResult{Allowed: !blocked, Reason: map[bool]string{true: "model_blocked"}[blocked]}, nil)
			})
			accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, r event.Result) { releases++; r.Set(accevents.ReleaseResult{Released: true}, nil) })
			accounts.Subscribe(accevents.TopicRecordResult, func(_ event.Event, r event.Result) { feedback++; r.Set(accevents.RecordResultResult{}, nil) })
			up := event.NewSimpleObserver(upcommon.UnitID, hub)
			up.Subscribe(upevents.TopicWSSend, func(_ event.Event, r event.Result) { sends++; r.Set(upevents.WSSendResult{Sent: true}, nil) })
			up.Subscribe(upevents.TopicWSClose, func(_ event.Event, r event.Result) { closes++; r.Set(upevents.WSCloseResult{}, nil) })
			proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background), codexWebsockets: map[string]codexWebsocketBinding{"ws": {accountID: "account", leaseID: "lease", model: "gpt-sol"}}}
			ctx := usagetarget.WithTracker(context.Background())
			payload := []byte(`{"type":"response.create","model":"gpt-sol"}`)
			if err := proxy.SendCodexWebsocket(ctx, "ws", payload); err != nil {
				t.Fatal(err)
			}
			usagetarget.ObserveCodex(ctx, []byte(`{"type":"response.completed","response":{"model":"gpt-luna"}}`))
			blocked = true
			err := proxy.SendCodexWebsocket(ctx, "ws", payload)
			f, ok := codexresponses.AsFailure(err)
			reason := "model_blocked"
			if unavailable {
				reason = "policy_check_unavailable"
			}
			if !ok || f.UnavailableReason != reason || sends != 1 || closes != 1 || releases != 1 || feedback != 0 || len(proxy.codexWebsockets) != 0 {
				t.Fatalf("sent=%d closed=%d released=%d feedback=%d failure=%+v", sends, closes, releases, feedback, f)
			}
			if target := usagetarget.Snapshot(ctx); target.AccountID != "" || target.ResponseModel != "" || target.UpstreamRequestModel != "" {
				t.Fatalf("denial reused previous turn evidence: %+v", target)
			}

		})
	}
}
