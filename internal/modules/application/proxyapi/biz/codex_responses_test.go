package biz

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestCodexWebsocketTurnOutcomeRequiresNonEmptyCompleted(t *testing.T) {
	if success, terminal, class := codexWebsocketTurnOutcome([]byte(`{"type":"response.completed","response":{"output":[]}}`)); success || !terminal || class != accevents.ErrorUpstream {
		t.Fatalf("CP-STREAM-006 empty success=%v terminal=%v class=%q", success, terminal, class)
	}
	if success, terminal, class := codexWebsocketTurnOutcome([]byte(`{"type":"response.completed","response":{"output":[],"usage":{"input_tokens":1,"output_tokens":0}}}`)); !success || !terminal || class != "" {
		t.Fatalf("CP-FAIL-014 usage success=%v terminal=%v class=%q", success, terminal, class)
	}
	if success, terminal, class := codexWebsocketTurnOutcome([]byte(`{"type":"response.failed","response":{"error":{"message":"failed"}}}`)); success || !terminal || class != accevents.ErrorUpstream {
		t.Fatalf("CP-FAIL-014 failed success=%v terminal=%v class=%q", success, terminal, class)
	}
	if success, terminal, class := codexWebsocketTurnOutcome([]byte(`{"type":"response.incomplete","response":{"status":"incomplete","usage":{"input_tokens":1,"output_tokens":2}}}`)); !success || !terminal || class != "" {
		t.Fatalf("CP-STREAM-007 incomplete success=%v terminal=%v class=%q", success, terminal, class)
	}
}

func TestCloseCodexWebsocketDoesNotInventAccountSuccess(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(4)
	t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	recorded := make(chan accevents.RecordResultCommand, 2)
	accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, result event.Result) { result.Set(accevents.ReleaseResult{Released: true}, nil) })
	accounts.Subscribe(accevents.TopicRecordResult, func(ev event.Event, result event.Result) {
		recorded <- ev.Data().(accevents.RecordResultCommand)
		result.Set(accevents.RecordResultResult{}, nil)
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicWSClose, func(_ event.Event, result event.Result) { result.Set(upevents.WSCloseResult{Closed: true}, nil) })
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background), codexWebsockets: map[string]codexWebsocketBinding{
		"session": {leaseID: "lease", accountID: "account", model: "gpt-test"},
	}}
	proxy.CloseCodexWebsocket(context.Background(), "session")
	select {
	case command := <-recorded:
		t.Fatalf("CP-FAIL-014 close invented result=%+v", command)
	default:
	}

	proxy.codexWebsockets["completed"] = codexWebsocketBinding{leaseID: "lease-2", accountID: "account", model: "gpt-test"}
	proxy.recordCodexWebsocketTurn(context.Background(), "completed", true, "")
	proxy.CloseCodexWebsocket(context.Background(), "completed")
	command := <-recorded
	if !command.Success {
		t.Fatalf("CP-FAIL-014 completed result=%+v", command)
	}
	select {
	case duplicate := <-recorded:
		t.Fatalf("CP-FAIL-014 duplicate result=%+v", duplicate)
	default:
	}
}

func TestCompleteCodexResponsesSwitchesBeforeOutputForRetryableStatuses(t *testing.T) {
	tests := []struct {
		name       string
		class      upevents.ErrorClass
		statusCode int
	}{
		{name: "forbidden", class: upevents.ErrorUpstream, statusCode: http.StatusForbidden},
		{name: "request timeout", class: upevents.ErrorTimeout, statusCode: http.StatusRequestTimeout},
		{name: "rate limit", class: upevents.ErrorRateLimit, statusCode: http.StatusTooManyRequests},
		{name: "server error", class: upevents.ErrorUpstream, statusCode: http.StatusBadGateway},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			hub := event.NewHub(24)
			background := task.NewBackgroundRoutine(8)
			t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
			accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
			acquires := 0
			accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, result event.Result) {
				acquires++
				result.Set(accevents.AcquireResult{AccountID: fmt.Sprintf("account-%d", acquires), AccessToken: fmt.Sprintf("token-%d", acquires), LeaseID: fmt.Sprintf("lease-%d", acquires)}, nil)
			})
			accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, result event.Result) { result.Set(accevents.ReleaseResult{Released: true}, nil) })
			accounts.Subscribe(accevents.TopicRecordResult, func(_ event.Event, result event.Result) { result.Set(accevents.RecordResultResult{}, nil) })
			upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
			upstream.Subscribe(upevents.TopicComplete, func(ev event.Event, result event.Result) {
				if ev.Data().(upevents.CompleteCommand).AccessToken == "token-1" {
					result.Set(upevents.CompleteResult{ErrorClass: testCase.class, HTTPStatus: testCase.statusCode}, nil)
					return
				}
				result.Set(upevents.CompleteResult{Body: []byte(`{"id":"resp_ok"}`)}, nil)
			})
			proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
			if _, err := proxy.CompleteCodexResponses(context.Background(), codexresponses.Request{Model: "gpt-test", Body: []byte(`{"model":"gpt-test"}`)}); err != nil || acquires != 2 {
				t.Fatalf("CP-FAIL-004..007 status=%d acquires=%d err=%v", testCase.statusCode, acquires, err)
			}
		})
	}
}

func TestCodexCompactSwitchesAndRecordsExplicitlyUnsupportedAccount(t *testing.T) {
	hub := event.NewHub(24)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	acquires := 0
	accounts.Subscribe(accevents.TopicAcquire, func(ev event.Event, result event.Result) {
		command := ev.Data().(accevents.AcquireCommand)
		if command.Transport != accevents.TransportCompact {
			t.Errorf("transport=%q", command.Transport)
		}
		acquires++
		result.Set(accevents.AcquireResult{AccountID: fmt.Sprintf("account-%d", acquires), AccessToken: fmt.Sprintf("token-%d", acquires), LeaseID: fmt.Sprintf("lease-%d", acquires)}, nil)
	})
	accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, result event.Result) { result.Set(accevents.ReleaseResult{Released: true}, nil) })
	accounts.Subscribe(accevents.TopicRecordResult, func(_ event.Event, result event.Result) { result.Set(accevents.RecordResultResult{}, nil) })
	capabilities := make(chan accevents.RecordTransportCapabilityCommand, 2)
	accounts.Subscribe(accevents.TopicRecordTransportCapability, func(ev event.Event, result event.Result) {
		capabilities <- ev.Data().(accevents.RecordTransportCapabilityCommand)
		result.Set(accevents.RecordTransportCapabilityResult{}, nil)
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicCompact, func(ev event.Event, result event.Result) {
		if ev.Data().(upevents.CompactCommand).AccessToken == "token-1" {
			result.Set(upevents.CompactResult{ErrorClass: upevents.ErrorInvalidRequest, HTTPStatus: http.StatusNotFound}, nil)
			return
		}
		result.Set(upevents.CompactResult{Body: []byte(`{"id":"resp_compact"}`)}, nil)
	})
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	completed, err := proxy.CompleteCodexCompact(context.Background(), codexresponses.Request{Model: "gpt-test", Body: []byte(`{"model":"gpt-test"}`)})
	if err != nil || acquires != 2 || string(completed.Body) != `{"id":"resp_compact"}` {
		t.Fatalf("completed=%s acquires=%d err=%v", completed.Body, acquires, err)
	}
	first, second := <-capabilities, <-capabilities
	if first.AccountID != "account-1" || first.Supported || second.AccountID != "account-2" || !second.Supported {
		t.Fatalf("capabilities=%+v %+v", first, second)
	}
}

func TestCompleteCodexResponsesPreservesLastRetryableFailure(t *testing.T) {
	hub := event.NewHub(24)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	acquires := 0
	accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, result event.Result) {
		acquires++
		if acquires > 2 {
			result.Set(nil, cd.NewError(cd.NotFound, "exhausted"))
			return
		}
		result.Set(accevents.AcquireResult{AccountID: fmt.Sprintf("account-%d", acquires), AccessToken: fmt.Sprintf("token-%d", acquires), LeaseID: fmt.Sprintf("lease-%d", acquires)}, nil)
	})
	accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, result event.Result) { result.Set(accevents.ReleaseResult{Released: true}, nil) })
	accounts.Subscribe(accevents.TopicRecordResult, func(_ event.Event, result event.Result) { result.Set(accevents.RecordResultResult{}, nil) })
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicComplete, func(ev event.Event, result event.Result) {
		status := http.StatusBadGateway
		if ev.Data().(upevents.CompleteCommand).AccessToken == "token-2" {
			status = http.StatusServiceUnavailable
		}
		result.Set(upevents.CompleteResult{ErrorClass: upevents.ErrorUpstream, HTTPStatus: status}, nil)
	})
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	_, err := proxy.CompleteCodexResponses(context.Background(), codexresponses.Request{Model: "gpt-test", Body: []byte(`{"model":"gpt-test"}`)})
	failure, ok := codexresponses.AsFailure(err)
	if !ok || failure.HTTPStatus != http.StatusServiceUnavailable || failure.Kind == codexresponses.KindProviderUnavailable {
		t.Fatalf("CP-FAIL-010 failure=%+v err=%v", failure, err)
	}
}

func TestCompleteCodexResponsesSwitchesOnUpstreamFailureAndReleasesLeases(t *testing.T) {
	hub := event.NewHub(32)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	acquires := 0
	accounts.Subscribe(accevents.TopicAcquire, func(ev event.Event, result event.Result) {
		acquires++
		command := ev.Data().(accevents.AcquireCommand)
		if command.SessionHash != "safe-session-hash" {
			t.Errorf("CP-SCHED-003: acquire command=%+v", command)
		}
		id := fmt.Sprintf("account-%d", acquires)
		result.Set(accevents.AcquireResult{AccountID: id, AccessToken: id + "-token", LeaseID: "lease-" + id}, nil)
	})
	releases := map[string]int{}
	accounts.Subscribe(accevents.TopicRelease, func(ev event.Event, result event.Result) {
		lease := ev.Data().(accevents.ReleaseCommand).LeaseID
		releases[lease]++
		result.Set(accevents.ReleaseResult{Released: releases[lease] == 1}, nil)
	})
	accounts.Subscribe(accevents.TopicRecordResult, func(_ event.Event, result event.Result) { result.Set(accevents.RecordResultResult{}, nil) })
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicComplete, func(ev event.Event, result event.Result) {
		if ev.Data().(upevents.CompleteCommand).AccessToken == "account-1-token" {
			result.Set(upevents.CompleteResult{ErrorClass: upevents.ErrorUpstream, HTTPStatus: 503}, nil)
			return
		}
		result.Set(upevents.CompleteResult{Body: []byte(`{"id":"resp_ok"}`)}, nil)
	})
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	completed, err := proxy.CompleteCodexResponses(context.Background(), codexresponses.Request{Model: "gpt-test", SessionHash: "safe-session-hash", Body: []byte(`{"model":"gpt-test"}`)})
	if err != nil || string(completed.Body) != `{"id":"resp_ok"}` || acquires != 2 {
		t.Fatalf("CP-FAIL-007: completed=%s err=%v acquires=%d", completed.Body, err, acquires)
	}
	if releases["lease-account-1"] != 1 || releases["lease-account-2"] != 1 {
		t.Fatalf("CP-SCHED-005: releases=%v", releases)
	}
}

func TestCompleteCodexResponsesDoesNotSwitchOnInvalidRequest(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	acquires := 0
	accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, result event.Result) {
		acquires++
		result.Set(accevents.AcquireResult{AccountID: "account-1", AccessToken: "token", LeaseID: "lease-1"}, nil)
	})
	accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, result event.Result) { result.Set(accevents.ReleaseResult{Released: true}, nil) })
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicComplete, func(_ event.Event, result event.Result) {
		result.Set(upevents.CompleteResult{ErrorClass: upevents.ErrorInvalidRequest, HTTPStatus: 400}, nil)
	})
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	_, err := proxy.CompleteCodexResponses(context.Background(), codexresponses.Request{Model: "gpt-test", Body: []byte(`{"model":"gpt-test"}`)})
	failure, ok := codexresponses.AsFailure(err)
	if !ok || failure.Kind != codexresponses.KindInvalidRequest || acquires != 1 {
		t.Fatalf("CP-FAIL-001/002: failure=%+v acquires=%d", failure, acquires)
	}
}

func TestStreamCodexResponsesNeverSwitchesAfterBusinessOutput(t *testing.T) {
	hub := event.NewHub(32)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	acquires := 0
	accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, result event.Result) {
		acquires++
		result.Set(accevents.AcquireResult{AccountID: "account-1", AccessToken: "token", LeaseID: "lease-1"}, nil)
	})
	accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, result event.Result) { result.Set(accevents.ReleaseResult{Released: true}, nil) })
	accounts.Subscribe(accevents.TopicRecordResult, func(_ event.Event, result event.Result) { result.Set(accevents.RecordResultResult{}, nil) })
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicStart, func(_ event.Event, result event.Result) { result.Set(upevents.StartResult{StreamID: "stream-1"}, nil) })
	pulls := 0
	upstream.Subscribe(upevents.TopicPull, func(_ event.Event, result event.Result) {
		pulls++
		if pulls == 1 {
			result.Set(upevents.PullResult{Data: []byte("event: response.output_text.delta\n")}, nil)
			return
		}
		result.Set(upevents.PullResult{Done: true, ErrorClass: upevents.ErrorUpstream}, nil)
	})
	upstream.Subscribe(upevents.TopicCancel, func(_ event.Event, result event.Result) { result.Set(upevents.CancelResult{Cancelled: true}, nil) })
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	err := proxy.StreamCodexResponses(context.Background(), codexresponses.Request{Model: "gpt-test", Body: []byte(`{"model":"gpt-test"}`)}, nil, func([]byte) error { return nil })
	if err == nil || acquires != 1 {
		t.Fatalf("CP-STREAM-003/CP-FAIL-008: err=%v acquires=%d", err, acquires)
	}
}

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

func TestCompleteCodexResponsesPreservesUpstreamInvalidTokenWhenRecoveryIsExhausted(t *testing.T) {
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
		result.Set(accevents.AcquireResult{AccountID: "account-1", AccessToken: "rejected-token"}, nil)
	})
	accounts.Subscribe(accevents.TopicRefreshToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.RefreshTokenResult{AccountID: "account-1", PermanentFailure: true, ErrorClass: accevents.ErrorInvalidToken}, cd.NewError(cd.Unexpected, "OAuth credential rejected"))
	})
	recorded := make(chan accevents.RecordResultCommand, 1)
	accounts.Subscribe(accevents.TopicRecordResult, func(ev event.Event, result event.Result) {
		recorded <- ev.Data().(accevents.RecordResultCommand)
		result.Set(accevents.RecordResultResult{}, nil)
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicComplete, func(_ event.Event, result event.Result) {
		result.Set(upevents.CompleteResult{ErrorClass: upevents.ErrorInvalidToken, HTTPStatus: 401}, nil)
	})

	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	_, err := proxy.CompleteCodexResponses(context.Background(), codexresponses.Request{Model: "gpt-test", Body: []byte(`{"model":"gpt-test"}`)})
	failure, ok := codexresponses.AsFailure(err)
	if !ok || failure.Kind != codexresponses.KindInvalidToken || failure.HTTPStatus != 401 {
		t.Fatalf("failure=%+v err=%v", failure, err)
	}
	if command := <-recorded; command.ErrorClass != accevents.ErrorInvalidToken {
		t.Fatalf("feedback=%+v", command)
	}
}

func TestCompleteCodexResponsesDoesNotPenalizeAccountForInvalidRequest(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() {
		background.Shutdown(nil)
		hub.Terminate(context.Background())
	})
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireResult{AccountID: "account-1", AccessToken: "token"}, nil)
	})
	recorded := make(chan accevents.RecordResultCommand, 1)
	accounts.Subscribe(accevents.TopicRecordResult, func(ev event.Event, result event.Result) {
		recorded <- ev.Data().(accevents.RecordResultCommand)
		result.Set(accevents.RecordResultResult{}, nil)
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicComplete, func(_ event.Event, result event.Result) {
		result.Set(upevents.CompleteResult{ErrorClass: upevents.ErrorInvalidRequest, HTTPStatus: 400}, nil)
	})
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	_, err := proxy.CompleteCodexResponses(context.Background(), codexresponses.Request{Model: "gpt-test", Body: []byte(`{"model":"gpt-test"}`)})
	failure, ok := codexresponses.AsFailure(err)
	if !ok || failure.Kind != codexresponses.KindInvalidRequest || failure.HTTPStatus != 400 {
		t.Fatalf("failure=%+v err=%v", failure, err)
	}
	select {
	case command := <-recorded:
		t.Fatalf("invalid request penalized account: %+v", command)
	default:
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
