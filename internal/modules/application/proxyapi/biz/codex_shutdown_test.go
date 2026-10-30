package biz

import (
	"context"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

func TestShutdownIsRecordedWithoutAccountCooldownOrReplay(t *testing.T) {
	hub := event.NewHub(16)
	defer hub.Terminate(context.Background())
	observer := event.NewSimpleObserver(acccommon.UnitID, hub)
	var feedback accevents.RecordResultCommand
	if err := observer.Subscribe(accevents.TopicRecordResult, func(ev event.Event, result event.Result) {
		feedback = ev.Data().(accevents.RecordResultCommand)
		result.Set(accevents.RecordResultResult{}, nil)
	}); err != nil {
		t.Fatal(err)
	}
	proxy := &Proxy{Base: basebiz.New("proxy", hub, nil)}
	for _, failure := range []*codexresponses.Failure{
		codexCommandFailure(context.Background(), cd.WrapError(cd.Unexpected, upevents.ErrServiceShutdown, "interrupted")),
		failureFromUpstream(upevents.ErrorServiceShutdown, 0, upevents.RateLimitObservation{}, 0),
	} {
		if failure.Kind != codexresponses.KindServiceShutdown || failure.Retryable == nil || *failure.Retryable || retryableCodexFailure(failure) {
			t.Fatalf("wrong lifecycle failure: %+v", failure)
		}
		proxy.recordCodexResult(context.Background(), "account", "model", false, string(failure.Kind), 0, false, "")
		if feedback.Success || !feedback.AvailabilityNeutral || feedback.ErrorClass != "service_shutdown" || feedback.QuotaExhausted {
			t.Fatalf("account penalty: %+v", feedback)
		}
	}
}
