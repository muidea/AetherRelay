package store

import (
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

func TestAdmissionRetryUsesExactModelAndLatestAccountCooldown(t *testing.T) {
	// CP-FAIL-019: earliest recoverable account, latest applicable cooldown,
	// upward rounding and no mutation when the client repeatedly retries.
	now := time.Now().UTC()
	s := &Store{items: map[string]*account{}}
	for _, id := range []string{"a", "b"} {
		s.items[id] = &account{ID: id, Status: events.StatusNormal, AccessToken: "test", ModelSnapshot: &events.AccountModelSnapshot{
			Models: []events.AccountModelEntry{{ID: "gpt-test"}, {ID: "other"}}, ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		}, Cooldowns: map[string]cooldown{}}
	}
	s.items["a"].Cooldowns["gpt-test"] = cooldown{Until: now.Add(10 * time.Second), ErrorClass: events.ErrorNetwork}
	s.items["a"].Cooldowns[""] = cooldown{Until: now.Add(20 * time.Second), ErrorClass: events.ErrorRateLimit}
	s.items["b"].Cooldowns["gpt-test"] = cooldown{Until: now.Add(12*time.Second + 100*time.Millisecond), ErrorClass: events.ErrorNetwork}
	for range 5 {
		got := s.unavailableResult("gpt-test", nil, nil, events.TransportResponses, now)
		if got.UnavailableReason != "accounts_cooling" || got.RetryAfterSeconds != 13 || got.AccountID != "" || got.AccessToken != "" {
			t.Fatalf("denial=%+v", got)
		}
	}
	got := s.unavailableResult("gpt-test", map[string]struct{}{"b": {}}, nil, events.TransportResponses, now)
	if got.RetryAfterSeconds != 20 {
		t.Fatalf("excluded account supplied hint: %+v", got)
	}
	s.items["a"].Status = events.StatusDisabled
	got = s.unavailableResult("other", nil, nil, events.TransportResponses, now)
	if got.RetryAfterSeconds != 0 {
		t.Fatalf("other model polluted: %+v", got)
	}
	if !modelAvailability(s.items["b"], "gpt-test", now.Add(13*time.Second)).Available {
		t.Fatal("retry extended cooldown")
	}
}

func TestAdmissionDistinguishesBusyCoolingAndPermanentCredentials(t *testing.T) {
	now := time.Now().UTC()
	newAccount := func(id string) *account {
		return &account{ID: id, Status: events.StatusNormal, AccessToken: "access", ModelSnapshot: &events.AccountModelSnapshot{
			Models: []events.AccountModelEntry{{ID: "gpt-test"}}, ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		}, Cooldowns: map[string]cooldown{}}
	}
	s := &Store{items: map[string]*account{"a": newAccount("a")}}
	busy := map[string]struct{}{"a": {}}
	if got := s.unavailableResult("gpt-test", nil, busy, events.TransportResponses, now); got.UnavailableReason != "accounts_busy" {
		t.Fatalf("busy denial=%+v", got)
	}
	s.items["a"].Cooldowns["gpt-test"] = cooldown{Until: now.Add(12 * time.Second), ErrorClass: events.ErrorNetwork}
	if got := s.unavailableResult("gpt-test", nil, nil, events.TransportResponses, now); got.UnavailableReason != "accounts_cooling" || got.RetryAfterSeconds != 12 {
		t.Fatalf("cooling denial=%+v", got)
	}
	s.items["a"].Cooldowns = nil
	s.items["a"].PermanentAuthFailure = true
	s.items["a"].Status = events.StatusAbnormal
	if got := s.unavailableResult("gpt-test", nil, nil, events.TransportResponses, now); got.UnavailableReason != "credential_permanently_invalid" || got.RetryAfterSeconds != 0 {
		t.Fatalf("permanent denial=%+v", got)
	}
}
