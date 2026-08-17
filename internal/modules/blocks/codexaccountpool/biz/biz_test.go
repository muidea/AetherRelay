package biz

import (
	"bytes"
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/internal/store"
	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"aetherrelay/internal/pkg/aetherrelaycredential"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func newSchedulingAccount(t *testing.T) (*Account, event.Hub, []string) {
	t.Helper()
	codec, err := aetherrelaycredential.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "accounts.duckdb"), "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, ids, err := st.ImportWithIDs([]events.CredentialInput{
		{AccessToken: "access-one", RefreshToken: "refresh-one"},
		{AccessToken: "access-two", RefreshToken: "refresh-two"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, id := range ids {
		if _, _, err := st.PutModelSnapshot(id, events.AccountModelSnapshot{
			Models:       []events.AccountModelEntry{{ID: "gpt-test"}},
			DiscoveredAt: now.Format(time.RFC3339),
			ExpiresAt:    now.Add(time.Hour).Format(time.RFC3339),
		}); err != nil {
			t.Fatal(err)
		}
	}
	hub := event.NewHub(64)
	background := task.NewBackgroundRoutine(16)
	account := newAccount(hub, background, st, time.Hour, time.Minute)
	t.Cleanup(func() {
		account.Teardown(context.Background())
		background.Shutdown(context.Background())
		hub.Terminate(context.Background())
	})
	return account, hub, ids
}

func acquireForTest(t *testing.T, account *Account, cmd events.AcquireCommand) events.AcquireResult {
	t.Helper()
	result := event.NewResult(events.TopicAcquire, "test", account.ID())
	account.handleAcquire(event.NewEvent(events.TopicAcquire, "test", account.ID(), nil, cmd), result)
	value, err := result.Get()
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	acquired, ok := value.(events.AcquireResult)
	if !ok {
		t.Fatalf("invalid acquire result: %#v", value)
	}
	return acquired
}

func releaseForTest(t *testing.T, account *Account, leaseID string) events.ReleaseResult {
	t.Helper()
	result := event.NewResult(events.TopicRelease, "test", account.ID())
	account.handleRelease(event.NewEvent(events.TopicRelease, "test", account.ID(), nil, events.ReleaseCommand{LeaseID: leaseID}), result)
	value, err := result.Get()
	if err != nil {
		t.Fatalf("release failed: %v", err)
	}
	return value.(events.ReleaseResult)
}

func TestAcquireKeepsHealthySessionAffinity(t *testing.T) {
	account, _, _ := newSchedulingAccount(t)
	first := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "hashed-session"})
	releaseForTest(t, account, first.LeaseID)
	second := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "hashed-session"})
	if second.AccountID != first.AccountID {
		t.Fatalf("CP-SCHED-003/004: session moved from %q to %q", first.AccountID, second.AccountID)
	}
}

func TestAcquireEnforcesConcurrencyAndReleaseIsIdempotent(t *testing.T) {
	account, _, _ := newSchedulingAccount(t)
	leasing := make([]events.AcquireResult, 0, defaultAccountConcurrency)
	for i := 0; i < defaultAccountConcurrency; i++ {
		leasing = append(leasing, acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", PreferredID: account.store.List()[0].ID}))
	}
	preferredID := leasing[0].AccountID
	for _, lease := range leasing {
		if lease.AccountID != preferredID {
			t.Fatalf("CP-SCHED-005: preferred account saturated too early: %+v", leasing)
		}
	}
	overflow := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", PreferredID: preferredID})
	if overflow.AccountID == preferredID {
		t.Fatal("CP-SCHED-005: saturated account received another lease")
	}
	if released := releaseForTest(t, account, leasing[0].LeaseID); !released.Released {
		t.Fatal("first release did not release lease")
	}
	if released := releaseForTest(t, account, leasing[0].LeaseID); released.Released {
		t.Fatal("duplicate release changed inflight state")
	}
	again := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", PreferredID: preferredID})
	if again.AccountID != preferredID {
		t.Fatal("released account did not regain an available slot")
	}
	if account.inflight[preferredID] != defaultAccountConcurrency {
		t.Fatalf("inflight=%d, want %d", account.inflight[preferredID], defaultAccountConcurrency)
	}
}

func TestAcquireIgnoresExpiredOrIneligibleAffinity(t *testing.T) {
	account, _, ids := newSchedulingAccount(t)
	now := time.Now().UTC()
	account.now = func() time.Time { return now }
	first := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "expired"})
	releaseForTest(t, account, first.LeaseID)
	account.sessions["expired"] = sessionBinding{accountID: first.AccountID, expiresAt: now.Add(-time.Second)}
	next := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "expired", Exclude: []string{first.AccountID}})
	if next.AccountID == first.AccountID {
		t.Fatal("CP-SCHED-004: expired affinity was reused")
	}
	status := events.StatusDisabled
	if _, err := account.store.Update(first.AccountID, &status, nil, nil); err != nil {
		t.Fatal(err)
	}
	account.sessions["disabled"] = sessionBinding{accountID: first.AccountID, expiresAt: now.Add(time.Hour)}
	disabled := acquireForTest(t, account, events.AcquireCommand{Model: "gpt-test", SessionHash: "disabled"})
	if disabled.AccountID == first.AccountID || disabled.AccountID == "" || len(ids) != 2 {
		t.Fatalf("CP-SCHED-004: disabled affinity selected: %+v", disabled)
	}
}
