package biz

import (
	"context"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

func TestCommandAdmissionFailureDoesNotCoolAccount(t *testing.T) {
	hub := event.NewHub(16)
	defer hub.Terminate(context.Background())
	recorded := 0
	observer := event.NewSimpleObserver(acccommon.UnitID, hub)
	if err := observer.Subscribe(accevents.TopicRecordResult, func(_ event.Event, result event.Result) { recorded++; result.Set(accevents.RecordResultResult{}, nil) }); err != nil {
		t.Fatal(err)
	}
	proxy := &Proxy{Base: basebiz.New("proxy", hub, nil)}
	for _, code := range []cd.Code{cd.ResourceExhausted, cd.Timeout, cd.InvalidOperation} {
		failure := codexCommandFailure(context.Background(), cd.NewError(code, "private command error"))
		if failure.Kind != codexresponses.KindProviderUnavailable || failure.UnavailableReason != "local_capacity" {
			t.Fatalf("failure=%+v", failure)
		}
		proxy.recordCodexResult(context.Background(), "account", "model", false, string(failure.Kind), 1, false, "")
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	failure := codexCommandFailure(ctx, ctx.Err())
	if failure.UnavailableReason != "queue_timeout" {
		t.Fatalf("failure=%+v", failure)
	}
	proxy.recordCodexResult(context.Background(), "account", "model", false, string(failure.Kind), 1, false, "")
	if recorded != 0 {
		t.Fatalf("local failures recorded %d account penalties", recorded)
	}
	proxy.recordCodexResult(context.Background(), "account", "model", false, string(codexresponses.KindNetwork), 0, false, "")
	if recorded != 1 {
		t.Fatal("real network failure lost its account feedback")
	}
}
