package biz

import (
	"testing"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func TestAccountAdmissionReturnsRetryHintWithoutLease(t *testing.T) {
	// CP-FAIL-019: preserve typed retry data alongside EventHub denial.
	a, _, ids := newSchedulingAccount(t)
	for _, id := range ids {
		if _, err := a.store.RecordResult(id, "gpt-test", false, events.ErrorNetwork, 30, false, "", false); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		r := event.NewResult(events.TopicAcquire, "test", a.ID())
		a.handleAcquire(event.NewEvent(events.TopicAcquire, "test", a.ID(), nil, events.AcquireCommand{Model: "gpt-test"}), r)
		value, err := r.Get()
		got, ok := value.(events.AcquireResult)
		if err == nil || !ok || got.UnavailableReason != "accounts_cooling" || got.RetryAfterSeconds < 25 || got.RetryAfterSeconds > 30 || got.AccountID != "" {
			t.Fatalf("denial=%+v err=%v", got, err)
		}
	}
	if len(a.leases) != 0 || len(a.inflight) != 0 {
		t.Fatal("denial leaked lease")
	}
}
