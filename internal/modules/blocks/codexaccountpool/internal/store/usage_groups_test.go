package store

import (
	"path/filepath"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"aetherrelay/internal/pkg/aetherrelaycodex"
)

func groupedUsage(now time.Time) events.AccountUsageSnapshot {
	snapshot := recoveredUsage(now.Add(-time.Second))
	snapshot.PlanType = "plus"
	snapshot.Groups = []aetherrelaycodex.UsageLimitGroup{{ID: "standard", Scope: "standard", AllowedKnown: true, LimitReachedKnown: true, LimitReached: true}}
	for i := range snapshot.Windows {
		window := &snapshot.Windows[i]
		window.GroupID = "standard"
		window.LimitReachedKnown = true
		window.ResetAt = now.Add(5 * time.Hour).Format(time.RFC3339Nano)
		window.WindowSeconds = 18000
		window.UsedPercent = 0
	}
	snapshot.Windows[1].UsedPercent = 100
	snapshot.Windows[1].Allowed = false
	snapshot.Windows[1].LimitReached = true
	snapshot.Windows[1].WindowSeconds = 604800
	snapshot.Windows[1].ResetAt = now.Add(5*24*time.Hour + 19*time.Hour).Format(time.RFC3339Nano)
	return snapshot
}

func TestWeeklyRestrictionShowsActualResetAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.duckdb")
	codec := encryptedTestCodec(t)
	s, err := Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, _, _, err = s.Import([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}}); err != nil {
		t.Fatal(err)
	}
	id := s.List()[0].ID
	now := time.Now().UTC()
	models := events.AccountModelSnapshot{ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano), Models: []events.AccountModelEntry{{ID: "gpt-6.1-sol"}, {ID: "codex-auto-review"}}}
	if _, _, err = s.PutModelSnapshot(id, models); err != nil {
		t.Fatal(err)
	}
	snapshot := groupedUsage(now)
	if _, err = s.PutUsageSnapshot(id, snapshot); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		view, _ := s.View(id)
		if len(view.UsageSnapshot.Groups) != 1 || view.UsageSnapshot.Windows[0].LimitReached || !view.UsageSnapshot.Windows[0].Allowed {
			t.Fatalf("persisted windows contaminated: %+v", view.UsageSnapshot)
		}
		detail := modelAvailability(s.items[id], "gpt-6.1-sol", now)
		if detail.Available || detail.Reason != "usage_limit" || detail.UsageLimitWindow != "standard-secondary" || detail.UsageLimitWindowSeconds != 604800 || detail.UsageLimitResetAt != snapshot.Windows[1].ResetAt || detail.Until != "" {
			t.Fatalf("wrong management reason: %+v", detail)
		}
		retry, blocked := usageLimitResetAt(s.items[id], now, "gpt-6.1-sol")
		expiry, _ := parseExpiry(snapshot.ExpiresAt)
		if !blocked || !retry.Equal(expiry) {
			t.Fatalf("retry must be bounded by fresh evidence: %v %v", retry, blocked)
		}
		if _, err := s.Acquire("gpt-6.1-sol", nil); err == nil {
			t.Fatal("exhausted ordinary quota admitted")
		}
		if detail := modelAvailability(s.items[id], "codex-auto-review", now); !detail.Available {
			t.Fatalf("ordinary quota leaked into independent review scope: %+v", detail)
		}
	}
	check()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	check()
}

func TestGroupRestrictionFreshnessOrderingAndPartialHeaders(t *testing.T) {
	now := time.Now().UTC()
	snapshot := groupedUsage(now)
	// The upstream denies the group without identifying an exhausted child.
	snapshot.Windows = snapshot.Windows[:1]
	original := prepareUsageSnapshot(snapshot, "manual")
	item := &account{UsageSnapshot: &original}
	if !usageLimitCooling(item, now, "gpt-6.1-sol") || freshQuotaHeadroom(item, now, "gpt-6.1-sol") {
		t.Fatal("unexplained denial was ignored")
	}
	detail := usageLimitDetails(item, "gpt-6.1-sol", now)
	if detail.UsageLimitScope != "standard" || detail.UsageLimitWindow != "" || detail.UsageLimitResetAt != "" {
		t.Fatalf("invented exhausted window/reset: %+v", detail)
	}
	header := recoveredUsage(now.Add(-500 * time.Millisecond))
	header.ExpiresAt = now.Add(time.Hour).Format(time.RFC3339Nano)
	item.UsageSnapshot = mergeUsageObservations(item.UsageSnapshot, prepareUsageSnapshot(header, "response_header"))
	group := item.UsageSnapshot.Groups[0]
	if group.ObservedAt != original.Groups[0].ObservedAt || group.ExpiresAt != original.Groups[0].ExpiresAt || !usageLimitCooling(item, now, "gpt-6.1-sol") {
		t.Fatalf("partial header erased/restamped group evidence: %+v", group)
	}
	older := snapshot
	older.StartedAt = now.Add(-time.Minute).Format(time.RFC3339Nano)
	older.Groups = []aetherrelaycodex.UsageLimitGroup{{ID: "standard", Scope: "standard", AllowedKnown: true, Allowed: true}}
	item.UsageSnapshot = mergeUsageObservations(item.UsageSnapshot, prepareUsageSnapshot(older, "manual"))
	if !usageLimitCooling(item, now, "gpt-6.1-sol") {
		t.Fatal("older full response erased group denial")
	}
	newer := snapshot
	newer.StartedAt = now.Add(-time.Millisecond).Format(time.RFC3339Nano)
	newer.Groups = []aetherrelaycodex.UsageLimitGroup{{ID: "standard", Scope: "standard", AllowedKnown: true, Allowed: true, LimitReachedKnown: true}}
	item.UsageSnapshot = mergeUsageObservations(item.UsageSnapshot, prepareUsageSnapshot(newer, "manual"))
	if usageLimitCooling(item, now, "gpt-6.1-sol") {
		t.Fatal("newer healthy group failed to supersede old denial")
	}
	// With no new full query, the denial expires on its own TTL despite headers.
	item.UsageSnapshot = mergeUsageObservations(&original, prepareUsageSnapshot(header, "response_header"))
	if usageLimitCooling(item, now.Add(16*time.Minute), "gpt-6.1-sol") {
		t.Fatal("header extended group lifetime")
	}
}

func TestIdentifiedGroupResetDoesNotExtendRestriction(t *testing.T) {
	now := time.Now().UTC()
	snapshot := groupedUsage(now)
	snapshot.Windows[1].ResetAt = now.Add(time.Minute).Format(time.RFC3339Nano)
	clean := prepareUsageSnapshot(snapshot, "manual")
	item := &account{UsageSnapshot: &clean}
	if !usageLimitCooling(item, now, "gpt-6.1-sol") {
		t.Fatal("exhaustion lost")
	}
	// Partial headers may replace both children before the reset. The group
	// retains its full-response explanation and must neither clear early nor
	// extend the restriction after that originally observed reset.
	header := recoveredUsage(now.Add(-500 * time.Millisecond))
	item.UsageSnapshot = mergeUsageObservations(item.UsageSnapshot, prepareUsageSnapshot(header, "response_header"))
	detail := usageLimitDetails(item, "gpt-6.1-sol", now)
	if !usageLimitCooling(item, now, "gpt-6.1-sol") || detail.UsageLimitWindow != "standard-secondary" || detail.UsageLimitResetAt != snapshot.Windows[1].ResetAt {
		t.Fatalf("partial headers erased original explanation: %+v", detail)
	}
	if usageLimitCooling(item, now.Add(2*time.Minute), "gpt-6.1-sol") {
		t.Fatal("expired reset prolonged by group TTL")
	}
}

func TestGroupScopeAndCredentialCooldownBoundaries(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		scope, model, target string
		blocked              bool
	}{
		{"standard", "", "gpt-6.1-sol", true}, {"standard", "", "codex-auto-review", false},
		{"code-review", "", "gpt-6.1-sol", false}, {"code-review", "", "codex-auto-review", true},
		{"additional", "gpt-reserve", "gpt-reserve", true}, {"additional", "gpt-reserve", "gpt-6.1-sol", false},
		{"additional", "", "gpt-6.1-sol", false}, {"unknown", "", "gpt-6.1-sol", false},
	} {
		snapshot := events.AccountUsageSnapshot{ObservedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano), Groups: []aetherrelaycodex.UsageLimitGroup{{ID: "group", Scope: tc.scope, Model: tc.model, AllowedKnown: true}}}
		item := &account{UsageSnapshot: &snapshot}
		if got := usageLimitCooling(item, now, tc.target); got != tc.blocked {
			t.Fatalf("scope=%s model=%s target=%s got=%v", tc.scope, tc.model, tc.target, got)
		}
	}
	item := &account{Status: events.StatusNormal, AccessToken: "access", Cooldowns: map[string]cooldown{"": {Until: now.Add(time.Hour), ErrorClass: events.ErrorRateLimit}}, ModelSnapshot: &events.AccountModelSnapshot{ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano), Models: []events.AccountModelEntry{{ID: "codex-auto-review"}, {ID: "gpt-6.1-sol"}}}}
	for _, model := range []string{"codex-auto-review", "gpt-6.1-sol"} {
		if detail := modelAvailability(item, model, now); detail.Available || detail.Reason != events.ErrorRateLimit {
			t.Fatalf("credential cooldown bypassed: %+v", detail)
		}
	}
}

func TestGroupEvidenceControlsQuotaRecovery(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name    string
		group   aetherrelaycodex.UsageLimitGroup
		recover bool
	}{
		{"denied", aetherrelaycodex.UsageLimitGroup{AllowedKnown: true}, false},
		{"reached", aetherrelaycodex.UsageLimitGroup{AllowedKnown: true, Allowed: true, LimitReachedKnown: true, LimitReached: true}, false},
		{"unknown", aetherrelaycodex.UsageLimitGroup{}, false},
		{"allowed but unknown limit", aetherrelaycodex.UsageLimitGroup{AllowedKnown: true, Allowed: true}, false},
		{"healthy", aetherrelaycodex.UsageLimitGroup{AllowedKnown: true, Allowed: true, LimitReachedKnown: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failureAt := now.Add(-time.Minute).Format(time.RFC3339Nano)
			item := &account{Cooldowns: map[string]cooldown{"": {Until: now.Add(time.Hour), Source: "usage_limit", ErrorClass: events.ErrorRateLimit}}, QuotaObservations: map[string]quotaObservation{"": {State: "exhausted", ObservedAt: failureAt}}}
			snapshot := recoveredSingleWeeklyUsage(now.Add(-time.Second))
			tc.group.ID, tc.group.Scope = "standard", "standard"
			snapshot.Groups = []aetherrelaycodex.UsageLimitGroup{tc.group}
			snapshot.Windows[0].GroupID = "standard"
			clean := prepareUsageSnapshot(snapshot, "manual")
			item.UsageSnapshot = &clean
			reconcileQuotaRecovery(item, clean, now)
			if got := len(item.QuotaObservations) == 0 && len(item.Cooldowns) == 0; got != tc.recover {
				t.Fatalf("recovery=%v wanted=%v", got, tc.recover)
			}
		})
	}
}

func TestGroupWithUnknownWindowResetDoesNotInventRecoveryTime(t *testing.T) {
	now := time.Now().UTC()
	snapshot := groupedUsage(now)
	snapshot.Windows[0].UsedPercent, snapshot.Windows[0].LimitReached = 100, true
	snapshot.Windows[0].ResetAt = ""
	clean := prepareUsageSnapshot(snapshot, "manual")
	item := &account{UsageSnapshot: &clean}
	detail := usageLimitDetails(item, "gpt-6.1-sol", now)
	if detail.UsageLimitWindow != "standard-primary" || detail.UsageLimitResetAt != "" {
		t.Fatalf("unknown reset displaced by another window: %+v", detail)
	}
	header := recoveredUsage(now.Add(-500 * time.Millisecond))
	item.UsageSnapshot = mergeUsageObservations(item.UsageSnapshot, prepareUsageSnapshot(header, "response_header"))
	if group := item.UsageSnapshot.Groups[0]; group.ExhaustedWindowID != "standard-primary" || group.ResetAt != "" || !usageLimitCooling(item, now, "gpt-6.1-sol") {
		t.Fatalf("unknown reset lost: %+v", group)
	}
}
