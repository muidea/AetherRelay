package biz

import (
	"context"
	"testing"

	"ai-proxy/internal/modules/application/proxyapi/pkg/codexresponses"
	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	basebiz "ai-proxy/internal/modules/base/biz"
	acccommon "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "ai-proxy/internal/modules/blocks/codexupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/codexupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestCompleteCodexResponsesRefreshesOnceThenRetries(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() {
		background.Shutdown(nil)
		hub.Terminate(context.Background())
	})

	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireResult{AccountID: "account-1", AccessToken: "old-token", AccountIDHeader: "chatgpt-account-1", Proxy: "http://old-proxy.invalid:8080"}, nil)
	})
	refreshes := 0
	accounts.Subscribe(accevents.TopicRefreshToken, func(ev event.Event, result event.Result) {
		refreshes++
		if command := ev.Data().(accevents.RefreshTokenCommand); command.AccountID != "account-1" {
			t.Errorf("refresh command=%+v", command)
		}
		result.Set(accevents.RefreshTokenResult{AccountID: "account-1", AccessToken: "new-token", AccountIDHeader: "chatgpt-account-1", Proxy: "http://new-proxy.invalid:8080", Refreshed: true}, nil)
	})
	recorded := make(chan accevents.RecordResultCommand, 2)
	accounts.Subscribe(accevents.TopicRecordResult, func(ev event.Event, result event.Result) {
		recorded <- ev.Data().(accevents.RecordResultCommand)
		result.Set(accevents.RecordResultResult{}, nil)
	})

	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	attempts := 0
	upstream.Subscribe(upevents.TopicComplete, func(ev event.Event, result event.Result) {
		attempts++
		command := ev.Data().(upevents.CompleteCommand)
		if attempts == 1 {
			if command.AccessToken != "old-token" || command.Proxy != "http://old-proxy.invalid:8080" {
				t.Errorf("first upstream command=%+v", command)
			}
			result.Set(upevents.CompleteResult{ErrorClass: upevents.ErrorInvalidToken}, nil)
			return
		}
		if command.AccessToken != "new-token" || command.Proxy != "http://new-proxy.invalid:8080" {
			t.Errorf("retry upstream command=%+v", command)
		}
		result.Set(upevents.CompleteResult{Body: []byte(`{"object":"response","id":"resp_recovered"}`)}, nil)
	})

	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	completed, err := proxy.CompleteCodexResponses(context.Background(), codexresponses.Request{Model: "gpt-5.2-codex", Body: []byte(`{"model":"gpt-5.2-codex"}`)})
	if err != nil || string(completed.Body) != `{"object":"response","id":"resp_recovered"}` || attempts != 2 || refreshes != 1 {
		t.Fatalf("completed=%+v err=%v attempts=%d refreshes=%d", completed, err, attempts, refreshes)
	}
	select {
	case command := <-recorded:
		if !command.Success || command.ErrorClass != "" || command.AccountID != "account-1" {
			t.Fatalf("final account result=%+v", command)
		}
	default:
		t.Fatal("successful retry did not report account feedback")
	}
}

func TestCompleteCodexResponsesUsesRefreshFailureClassForCooldownAndSwitchesAccount(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() {
		background.Shutdown(nil)
		hub.Terminate(context.Background())
	})

	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquire, func(ev event.Event, result event.Result) {
		command := ev.Data().(accevents.AcquireCommand)
		if len(command.Exclude) == 0 {
			result.Set(accevents.AcquireResult{AccountID: "account-1", AccessToken: "old-token"}, nil)
			return
		}
		if len(command.Exclude) != 1 || command.Exclude[0] != "account-1" {
			t.Errorf("unexpected exclusion=%+v", command.Exclude)
		}
		result.Set(accevents.AcquireResult{AccountID: "account-2", AccessToken: "healthy-token"}, nil)
	})
	accounts.Subscribe(accevents.TopicRefreshToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.RefreshTokenResult{AccountID: "account-1", ErrorClass: accevents.ErrorTimeout}, cd.NewError(cd.Unexpected, "OAuth timeout"))
	})
	recorded := make(chan accevents.RecordResultCommand, 2)
	accounts.Subscribe(accevents.TopicRecordResult, func(ev event.Event, result event.Result) {
		recorded <- ev.Data().(accevents.RecordResultCommand)
		result.Set(accevents.RecordResultResult{}, nil)
	})

	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicComplete, func(ev event.Event, result event.Result) {
		command := ev.Data().(upevents.CompleteCommand)
		if command.AccessToken == "old-token" {
			result.Set(upevents.CompleteResult{ErrorClass: upevents.ErrorInvalidToken}, nil)
			return
		}
		if command.AccessToken != "healthy-token" {
			t.Errorf("unexpected upstream command=%+v", command)
		}
		result.Set(upevents.CompleteResult{Body: []byte(`{"object":"response","id":"resp_fallback"}`)}, nil)
	})

	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	if _, err := proxy.CompleteCodexResponses(context.Background(), codexresponses.Request{Model: "gpt-5.2-codex", Body: []byte(`{"model":"gpt-5.2-codex"}`)}); err != nil {
		t.Fatalf("fallback account did not complete: %v", err)
	}
	first := <-recorded
	second := <-recorded
	if first.AccountID != "account-1" || first.Success || first.ErrorClass != accevents.ErrorTimeout {
		t.Fatalf("temporary refresh feedback=%+v", first)
	}
	if second.AccountID != "account-2" || !second.Success || second.ErrorClass != "" {
		t.Fatalf("fallback success feedback=%+v", second)
	}
}

func TestCompleteCodexResponsesRecordsObservedQuotaExhaustion(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() {
		background.Shutdown(nil)
		hub.Terminate(context.Background())
	})
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquire, func(ev event.Event, result event.Result) {
		if len(ev.Data().(accevents.AcquireCommand).Exclude) > 0 {
			result.Set(nil, cd.NewError(cd.NotFound, "no fallback account"))
			return
		}
		result.Set(accevents.AcquireResult{AccountID: "account-1", AccessToken: "token"}, nil)
	})
	recorded := make(chan accevents.RecordResultCommand, 1)
	accounts.Subscribe(accevents.TopicRecordResult, func(ev event.Event, result event.Result) {
		recorded <- ev.Data().(accevents.RecordResultCommand)
		result.Set(accevents.RecordResultResult{}, nil)
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicComplete, func(_ event.Event, result event.Result) {
		result.Set(upevents.CompleteResult{
			ErrorClass:        upevents.ErrorRateLimit,
			RetryAfterSeconds: 120,
			RateLimit:         upevents.RateLimitObservation{UsageLimited: true, ResetAt: "2026-08-03T12:00:00Z"},
		}, nil)
	})
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	if _, err := proxy.CompleteCodexResponses(context.Background(), codexresponses.Request{Model: "gpt-5.2-codex", Body: []byte(`{"model":"gpt-5.2-codex"}`)}); err == nil {
		t.Fatal("quota-limited account without fallback must fail")
	}
	command := <-recorded
	if command.Success || !command.QuotaExhausted || command.QuotaResetAt != "2026-08-03T12:00:00Z" || command.ErrorClass != string(codexresponses.KindRateLimit) {
		t.Fatalf("quota feedback=%+v", command)
	}
}

func TestFailureFromUpstreamDistinguishesQuotaExhaustion(t *testing.T) {
	quotaFailure := failureFromUpstream(upevents.ErrorRateLimit, 120, upevents.RateLimitObservation{UsageLimited: true, ResetAt: "2026-08-03T12:00:00Z"}, 429)
	if !quotaFailure.QuotaExhausted || quotaFailure.QuotaResetAt != "2026-08-03T12:00:00Z" {
		t.Fatalf("quota failure=%+v", quotaFailure)
	}
	genericFailure := failureFromUpstream(upevents.ErrorRateLimit, 120, upevents.RateLimitObservation{}, 429)
	if genericFailure.QuotaExhausted || genericFailure.QuotaResetAt != "" {
		t.Fatalf("generic failure=%+v", genericFailure)
	}
}
