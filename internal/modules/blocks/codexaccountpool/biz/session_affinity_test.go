package biz

import (
	"fmt"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
)

// CP-SCHED-004/012: renewal retains affinity without duplicating counts or expiry records.
func TestSessionRenewalKeepsAffinityUntilRenewedExpiry(t *testing.T) {
	account, _, ids := newSchedulingAccount(t)
	started := time.Now().UTC()
	now := started
	account.now = func() time.Time { return now }
	cmd := events.AcquireCommand{Model: "gpt-test", SessionHash: "existing"}
	first := acquireForTest(t, account, cmd)
	releaseForTest(t, account, first.LeaseID)
	// Renewing the current heap root must expose the next binding's deadline.
	otherID := ids[0]
	if otherID == first.AccountID {
		otherID = ids[1]
	}
	account.bindSessionLocked("other", otherID, started.Add(time.Hour+time.Nanosecond))
	for i := 1; i <= 10; i++ {
		now = started.Add(time.Duration(i) * 3 * time.Minute)
		lease := acquireForTest(t, account, cmd)
		releaseForTest(t, account, lease.LeaseID)
	}
	if len(account.sessions) != 2 || len(account.sessionExpiry) != 2 || account.sessionCounts[first.AccountID] != 1 || account.sessionCounts[otherID] != 1 {
		t.Fatalf("renewal duplicated binding state: sessions=%d expiry=%d counts=%v", len(account.sessions), len(account.sessionExpiry), account.sessionCounts)
	}
	// The original deadline has passed, but only the other binding is due.
	now = started.Add(time.Hour + time.Nanosecond)
	next := acquireForTest(t, account, cmd)
	releaseForTest(t, account, next.LeaseID)
	if next.AccountID != first.AccountID {
		t.Fatal("renewed affinity was lost at its original expiry")
	}
	if _, found := account.sessions["other"]; found || account.sessionCounts[otherID] != 0 {
		t.Fatal("due binding was not removed from the index and count")
	}
	now = now.Add(time.Hour)
	newLease := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "new"})
	releaseForTest(t, account, newLease.LeaseID)
	if _, found := account.sessions["existing"]; found || account.sessionCounts[first.AccountID] != 0 {
		t.Fatal("binding survived its renewed expiry")
	}
	if newLease.AccountID != otherID || len(account.sessionExpiry) != 1 {
		t.Fatal("expired binding biased rotation or retained an expiry record")
	}
}

// CP-SCHED-012: failover moves a single binding and its count to the new account.
func TestSessionRebindingTransfersAccountCount(t *testing.T) {
	account, _, _ := newSchedulingAccount(t)
	first := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "existing"})
	releaseForTest(t, account, first.LeaseID)
	replacement := acquireForTest(t, account, events.AcquireCommand{
		Model: "gpt-test", SessionHash: "existing", Exclude: []string{first.AccountID},
	})
	releaseForTest(t, account, replacement.LeaseID)
	if replacement.AccountID == first.AccountID || account.sessionCounts[first.AccountID] != 0 || account.sessionCounts[replacement.AccountID] != 1 {
		t.Fatalf("rebound counts=%v first=%s replacement=%s", account.sessionCounts, first.AccountID, replacement.AccountID)
	}
	if len(account.sessionCounts) != 1 || len(account.sessions) != 1 || len(account.sessionExpiry) != 1 {
		t.Fatal("rebind retained the old account count or duplicated the expiry record")
	}
	// The now-idle old account should receive a different new session.
	newLease := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "new"})
	if newLease.AccountID != first.AccountID {
		t.Fatal("old binding count biased the next new session")
	}
	releaseForTest(t, account, newLease.LeaseID)
}

// CP-SCHED-012: cleanup stays correct when a clock change moves an expiry earlier.
func TestExpiryIndexHandlesEarlierRenewalAndMultipleDueBindings(t *testing.T) {
	account, _, ids := newSchedulingAccount(t)
	now := time.Now().UTC()
	account.now = func() time.Time { return now }
	account.bindSessionLocked("later", ids[0], now.Add(2*time.Hour))
	account.bindSessionLocked("due", ids[1], now)
	account.bindSessionLocked("renewed-earlier", ids[0], now.Add(time.Hour))
	account.bindSessionLocked("renewed-earlier", ids[0], now)
	lease := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "new"})
	releaseForTest(t, account, lease.LeaseID)
	if lease.AccountID != ids[1] || len(account.sessions) != 2 || len(account.sessionExpiry) != 2 {
		t.Fatalf("due cleanup selected=%s sessions=%d expiry=%d", lease.AccountID, len(account.sessions), len(account.sessionExpiry))
	}
	if account.sessionCounts[ids[0]] != 1 || account.sessionCounts[ids[1]] != 1 {
		t.Fatalf("due cleanup counts=%v", account.sessionCounts)
	}
	for _, hash := range []string{"due", "renewed-earlier"} {
		if _, found := account.sessions[hash]; found {
			t.Fatalf("expired binding %q remained", hash)
		}
	}
}

// CP-SCHED-012: a rejected acquire cleans due bindings but creates no new binding.
func TestFailedAcquireDoesNotAddSessionCount(t *testing.T) {
	account, _, ids := newSchedulingAccount(t)
	now := time.Now().UTC()
	account.now = func() time.Time { return now }
	account.bindSessionLocked("expired", ids[0], now)
	account.bindSessionLocked("active", ids[1], now.Add(time.Hour))
	result := event.NewResult(events.TopicAcquire, "test", account.ID())
	account.handleAcquire(event.NewEvent(events.TopicAcquire, "test", account.ID(), nil, events.AcquireCommand{
		Model: "gpt-test", SessionHash: "failed", Exclude: ids,
	}), result)
	if _, err := result.Get(); err == nil {
		t.Fatal("expected unavailable acquire")
	}
	if len(account.sessions) != 1 || len(account.sessionExpiry) != 1 || len(account.sessionCounts) != 1 || account.sessionCounts[ids[1]] != 1 {
		t.Fatalf("failed acquire changed bindings: sessions=%d expiry=%d counts=%v", len(account.sessions), len(account.sessionExpiry), account.sessionCounts)
	}
	if _, found := account.sessions["failed"]; found {
		t.Fatal("failed acquire created an affinity binding")
	}
}

// CP-SCHED-012: admission cost should not scan retained session history.
func BenchmarkAcquireWithSessionHistory(b *testing.B) {
	for _, count := range []int{0, 100000, 360000} {
		b.Run(fmt.Sprintf("bindings_%d", count), func(b *testing.B) {
			account, _, ids := newSchedulingAccount(b)
			expiresAt := time.Now().Add(time.Hour)
			for i := 0; i < count; i++ {
				account.bindSessionLocked(fmt.Sprintf("history-%d", i), ids[i%len(ids)], expiresAt)
			}
			account.bindSessionLocked("existing", ids[0], expiresAt)
			cmd := events.AcquireCommand{Model: "gpt-test", SessionHash: "existing"}
			warm := acquireForTest(b, account, cmd)
			releaseForTest(b, account, warm.LeaseID)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				lease := acquireForTest(b, account, cmd)
				releaseForTest(b, account, lease.LeaseID)
			}
		})
	}
}
