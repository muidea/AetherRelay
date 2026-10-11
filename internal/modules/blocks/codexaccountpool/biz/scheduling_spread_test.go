package biz

import (
	"fmt"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
)

// CP-SCHED-011..012: live bindings spread new sessions even between turns.
func TestNewSessionsSpreadBetweenTurnsDespiteQuotaPreference(t *testing.T) {
	account, _, ids := newSchedulingAccount(t)
	now := time.Now().UTC()
	if _, err := account.store.PutUsageSnapshot(ids[0], events.AccountUsageSnapshot{
		ObservedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		Windows: []events.UsageWindow{{ID: "primary", UsedPercent: 20, UsedPercentKnown: true}},
	}); err != nil {
		t.Fatal(err)
	}
	selected := make(map[string]int)
	for i := range 6 {
		lease := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: fmt.Sprintf("client-%d", i)})
		selected[lease.AccountID]++
		releaseForTest(t, account, lease.LeaseID)
	}
	for _, id := range ids {
		if selected[id] != 3 {
			t.Fatalf("new sessions concentrated on one account: %v", selected)
		}
	}
}

// CP-SCHED-004/005/012: affinity outranks load until the account becomes full.
func TestExistingSessionKeepsOccupiedBindingAndMovesWhenFull(t *testing.T) {
	account, _, _ := newSchedulingAccount(t)
	first := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "client-a"})
	second := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "client-a"})
	if second.AccountID != first.AccountID {
		t.Fatal("existing session moved to an idle account")
	}
	third := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "client-b"})
	if third.AccountID == first.AccountID {
		t.Fatal("new session selected a full account")
	}
	fallback := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "client-a"})
	if fallback.AccountID != third.AccountID {
		t.Fatal("full binding did not move to the remaining account")
	}
	for _, lease := range []events.AcquireResult{first, second, third, fallback} {
		releaseForTest(t, account, lease.LeaseID)
	}
	next := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "client-a"})
	if next.AccountID != fallback.AccountID {
		t.Fatal("session did not retain the replacement binding")
	}
	if len(account.sessions) != 2 {
		t.Fatalf("repeat turns created extra bindings: %v", account.sessions)
	}
}

// CP-SCHED-012: active slot utilization outranks bound-session counts.
func TestNewSessionPrefersIdleAccountOverFewerBindings(t *testing.T) {
	account, _, ids := newSchedulingAccount(t)
	for i := range 4 {
		account.bindSessionLocked(fmt.Sprintf("idle-binding-%d", i), ids[0], time.Now().Add(time.Hour))
	}
	occupied := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", PreferredID: ids[1]})
	defer releaseForTest(t, account, occupied.LeaseID)
	lease := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "new"})
	if lease.AccountID != ids[0] {
		t.Fatal("new session did not choose the idle account")
	}
}

// CP-SCHED-012: expired bindings do not participate in selection.
func TestExpiredBindingsDoNotBiasNewSessions(t *testing.T) {
	account, _, ids := newSchedulingAccount(t)
	now := time.Now().UTC()
	account.now = func() time.Time { return now }
	account.bindSessionLocked("expired", ids[0], now)
	account.bindSessionLocked("active", ids[1], now.Add(time.Hour))
	lease := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "new"})
	if lease.AccountID != ids[0] {
		t.Fatal("expired binding biased the new session")
	}
	if _, found := account.sessions["expired"]; found {
		t.Fatal("expired binding was not removed")
	}
}

// CP-SCHED-005/012: concurrent admission atomically spreads new sessions.
func TestConcurrentNewSessionsSpreadWithinAccountLimits(t *testing.T) {
	account, _, ids := newSchedulingAccount(t)
	results := make(chan event.Result, 4)
	for i := range 4 {
		go func() {
			result := event.NewResult(events.TopicAcquire, "test", account.ID())
			account.handleAcquire(event.NewEvent(events.TopicAcquire, "test", account.ID(), nil, events.AcquireCommand{
				Model: "gpt-test", SessionHash: fmt.Sprintf("client-%d", i),
			}), result)
			results <- result
		}()
	}
	selected := make(map[string]int)
	for range 4 {
		value, err := (<-results).Get()
		if err != nil {
			t.Fatal(err)
		}
		selected[value.(events.AcquireResult).AccountID]++
	}
	for _, id := range ids {
		if selected[id] != 2 {
			t.Fatalf("concurrent distribution = %v", selected)
		}
	}
}
