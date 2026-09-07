package biz

import (
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func TestStickyFailoverStrictlyMatchesAccountModels(t *testing.T) {
	// CP-SCHED-009: exercise the account owner, session affinity and leases,
	// not just a proxy mock that supplies arbitrary account IDs.
	account, _, ids := newSchedulingAccount(t)
	cmd := events.AcquireCommand{Model: "gpt-test", SessionHash: "session", PreferredID: ids[0]}
	first := acquireForTest(t, account, cmd)
	releaseForTest(t, account, first.LeaseID)
	_, _, err := account.store.PutModelSnapshot(first.AccountID, events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "different-model"}}, ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	second := acquireForTest(t, account, cmd)
	if second.AccountID == first.AccountID {
		t.Fatal("session affinity bypassed updated model list")
	}
	releaseForTest(t, account, second.LeaseID)
	_, err = account.store.RecordResult(second.AccountID, "gpt-test", false, events.ErrorModelNotFound, 0, false, "", false)
	if err != nil {
		t.Fatal(err)
	}
	result := event.NewResult(events.TopicAcquire, "test", account.ID())
	account.handleAcquire(event.NewEvent(events.TopicAcquire, "test", account.ID(), nil, cmd), result)
	if _, err := result.Get(); err == nil {
		t.Fatal("all unavailable still acquired account")
	}
	if len(account.leases) != 0 || len(account.inflight) != 0 {
		t.Fatal("failed selection created lease")
	}
}
