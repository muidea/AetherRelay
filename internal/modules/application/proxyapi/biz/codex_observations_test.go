package biz

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

func TestCodexFeedbackTimeoutAllowsFailureDeliveryAfterRelease(t *testing.T) {
	hub := event.NewHub(32)
	defer hub.Terminate(context.Background())
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	var released atomic.Bool
	accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, r event.Result) {
		r.Set(accevents.AcquireResult{AccountID: "account", AccessToken: "test", LeaseID: "lease"}, nil)
	})
	accounts.Subscribe(accevents.TopicRelease, func(ev event.Event, r event.Result) {
		if _, limited := ev.Context().Deadline(); limited || ev.Context().Err() != nil {
			t.Error("accepted release could be discarded with its request context")
		}
		released.Store(true)
		r.Set(accevents.ReleaseResult{Released: true}, nil)
	})
	feedback := make(chan struct{})
	abort := make(chan struct{})
	defer close(abort)
	accounts.Subscribe(accevents.TopicRecordResult, func(ev event.Event, r event.Result) {
		close(feedback)
		if !released.Load() {
			t.Error("slow feedback retained the lease")
		}
		if _, limited := ev.Context().Deadline(); !limited {
			t.Error("feedback has no deadline")
		}
		select {
		case <-ev.Context().Done():
		case <-abort:
		}
		r.Set(nil, cd.NewError(cd.Timeout, "feedback timed out"))
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicStart, func(_ event.Event, r event.Result) {
		r.Set(upevents.StartResult{ErrorClass: upevents.ErrorRateLimit, HTTPStatus: 429}, nil)
	})
	p := &Proxy{Base: basebiz.New("test", hub, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- p.StreamCodexResponses(ctx, codexresponses.Request{Model: "test", Body: []byte(`{"model":"test"}`)}, nil, nil)
	}()
	select {
	case <-feedback:
	case <-time.After(time.Second):
		t.Fatal("did not reach feedback")
	}
	cancel()
	select {
	case err := <-done:
		failure, ok := codexresponses.AsFailure(err)
		if !ok || failure.Kind != codexresponses.KindRateLimit {
			t.Fatalf("cleanup changed the upstream failure: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow feedback prevented the executor from returning")
	}
}
