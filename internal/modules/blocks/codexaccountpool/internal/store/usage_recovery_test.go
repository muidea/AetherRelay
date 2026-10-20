package store

import (
	"path/filepath"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

func recoveredUsage(started time.Time) events.AccountUsageSnapshot {
	return events.AccountUsageSnapshot{
		StartedAt: started.Format(time.RFC3339Nano), Complete: true,
		ObservedAt: started.Add(time.Millisecond).Format(time.RFC3339Nano),
		ExpiresAt:  started.Add(15 * time.Minute).Format(time.RFC3339Nano),
		Windows: []events.UsageWindow{
			{ID: "standard-primary", UsedPercentKnown: true, UsedPercent: 10, AllowedKnown: true, Allowed: true},
			{ID: "standard-secondary", UsedPercentKnown: true, UsedPercent: 30, AllowedKnown: true, Allowed: true},
		},
	}
}

func TestFullUsageRecoveryReleasesQuotaAndPersistsAdmission(t *testing.T) {
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
	if _, _, err = s.PutModelSnapshot(id, events.AccountModelSnapshot{
		Models:    []events.AccountModelEntry{{ID: "gpt-6.1-sol"}, {ID: "gpt-6-luna"}},
		ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordResult(id, "gpt-6.1-sol", false, events.ErrorRateLimit, 3600, true, now.Add(5*24*time.Hour).Format(time.RFC3339), false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Acquire("gpt-6-luna", nil); err == nil {
		t.Fatal("credential quota must block the other model too")
	}
	// Keep a separate real model failure when the credential quota recovers.
	if _, err = s.RecordResult(id, "gpt-6-luna", false, events.ErrorNetwork, 300, false, "", false); err != nil {
		t.Fatal(err)
	}
	snapshot := recoveredUsage(time.Now().UTC())
	snapshot.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = s.PutUsageSnapshotWithSchedule(id, snapshot, "scheduled", now.Add(10*time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	view, _ := s.View(id)
	if len(view.QuotaObservations) != 0 || len(view.Cooldowns) != 1 || view.Cooldowns[0].Model != "gpt-6-luna" {
		t.Fatalf("recovery must only clear quota state: %+v", view)
	}
	if _, err = s.Acquire("gpt-6.1-sol", nil); err != nil {
		t.Fatalf("recovered model still denied: %v", err)
	}
	if _, err = s.Acquire("gpt-6-luna", nil); err == nil {
		t.Fatal("recovery cleared unrelated model failure")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Acquire("gpt-6.1-sol", nil); err != nil {
		t.Fatalf("restart resurrected quota cooldown: %v", err)
	}
}

func TestRecoveryRequiresNewCompletePositiveEvidence(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name   string
		change func(*events.AccountUsageSnapshot)
	}{
		{"partial headers", func(s *events.AccountUsageSnapshot) { s.Complete = false }},
		{"missing secondary", func(s *events.AccountUsageSnapshot) { s.Windows = s.Windows[:1] }},
		{"secondary still exhausted", func(s *events.AccountUsageSnapshot) { s.Windows[1].LimitReached = true }},
		{"unknown usage", func(s *events.AccountUsageSnapshot) { s.Windows[1] = events.UsageWindow{ID: "standard-secondary"} }},
		{"expired", func(s *events.AccountUsageSnapshot) { s.ExpiresAt = now.Add(-time.Second).Format(time.RFC3339Nano) }},
		{"no request start", func(s *events.AccountUsageSnapshot) { s.StartedAt = "" }},
		{"started before failure", func(s *events.AccountUsageSnapshot) { s.StartedAt = now.Add(-2 * time.Minute).Format(time.RFC3339Nano) }},
		{"future response", func(s *events.AccountUsageSnapshot) { s.ObservedAt = now.Add(time.Minute).Format(time.RFC3339Nano) }},
		{"unknown additional limit", func(s *events.AccountUsageSnapshot) {
			s.Windows = append(s.Windows, events.UsageWindow{ID: "additional-other-primary", LimitReached: true})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := now.Add(-time.Minute).Format(time.RFC3339Nano)
			reset := now.Add(5 * 24 * time.Hour)
			item := &account{
				Cooldowns:         map[string]cooldown{"": {Until: reset, ErrorClass: events.ErrorRateLimit, Source: "usage_limit", ObservedAt: observed}},
				QuotaObservations: map[string]quotaObservation{"": {State: "exhausted", ObservedAt: observed, ResetAt: reset.Format(time.RFC3339Nano)}},
			}
			snapshot := recoveredUsage(now.Add(-time.Second))
			tc.change(&snapshot)
			clean := prepareUsageSnapshot(snapshot, "scheduled")
			item.UsageSnapshot = &clean
			reconcileQuotaRecovery(item, clean, now)
			if len(item.Cooldowns) != 1 || len(item.QuotaObservations) != 1 {
				t.Fatalf("incomplete recovery cleared real failure: %+v", item)
			}
		})
	}
}

func TestLegacyQuotaRecoveryRequiresMatchingProvenance(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name          string
		source        string
		class         string
		matchingReset bool
		wantCooldown  bool
	}{
		{"verified legacy quota", "", events.ErrorRateLimit, true, false},
		{"unmatched legacy rate limit", "", events.ErrorRateLimit, false, true},
		{"explicit generic limit", "request_failure", events.ErrorRateLimit, true, true},
		{"unrelated network", "", events.ErrorNetwork, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reset := now.Add(time.Hour)
			quotaReset := reset
			if !tc.matchingReset {
				quotaReset = reset.Add(time.Minute)
			}
			item := &account{
				Cooldowns:         map[string]cooldown{"": {Until: reset, ErrorClass: tc.class, Source: tc.source}},
				QuotaObservations: map[string]quotaObservation{"": {State: "exhausted", ObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), ResetAt: quotaReset.Format(time.RFC3339Nano)}},
			}
			snapshot := prepareUsageSnapshot(recoveredUsage(now.Add(-time.Second)), "manual")
			item.UsageSnapshot = &snapshot
			reconcileQuotaRecovery(item, snapshot, now)
			if _, got := item.Cooldowns[""]; got != tc.wantCooldown {
				t.Fatalf("cooldown retained=%v want=%v", got, tc.wantCooldown)
			}
		})
	}
}

func TestPartialUsageKeepsWindowAgeAndFullRefreshSchedule(t *testing.T) {
	s := openTestStore(t)
	_, _, _, _ = s.Import([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}})
	id := s.List()[0].ID
	now := time.Now().UTC()
	old := recoveredUsage(now.Add(-20 * time.Minute))
	old.Windows[1].LimitReached = true
	old.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339Nano)
	if _, err := s.PutUsageSnapshotWithSchedule(id, old, "scheduled", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordUsageFailureWithSchedule(id, "timeout", "scheduled", true); err != nil {
		t.Fatal(err)
	}
	before, _ := s.View(id)
	header := events.AccountUsageSnapshot{StartedAt: now.Format(time.RFC3339Nano), ObservedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(15 * time.Minute).Format(time.RFC3339Nano), Windows: []events.UsageWindow{{ID: "header-primary", UsedPercentKnown: true, UsedPercent: 20}}}
	if _, err := s.MergeUsageSnapshotWithSchedule(id, header, "response_header", now.Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	after, _ := s.View(id)
	if after.NextUsageRefreshAt != before.NextUsageRefreshAt || after.UsageRefreshError != "timeout" || after.UsageRefreshSource != "scheduled" {
		t.Fatalf("headers overwrote full-refresh diagnostics/schedule: %+v", after)
	}
	if len(after.UsageSnapshot.Windows) != 2 || after.UsageSnapshot.Windows[0].ID != "standard-primary" {
		t.Fatalf("header aliases created duplicate windows: %+v", after.UsageSnapshot)
	}
	if usageLimitCooling(s.items[id], now, "gpt-6.1-sol") {
		t.Fatal("partial refresh made expired secondary limit fresh again")
	}
	if after.UsageSnapshot.Windows[1].ExpiresAt != old.ExpiresAt {
		t.Fatal("unobserved window expiry changed")
	}
}

func TestUsageOrderingAndMetadataCannotEraseKnownLimit(t *testing.T) {
	now := time.Now().UTC()
	newest := recoveredUsage(now.Add(-time.Second))
	newest.Windows[0].LimitReached = true
	current := prepareUsageSnapshot(newest, "scheduled")
	older := prepareUsageSnapshot(recoveredUsage(now.Add(-time.Minute)), "manual")
	merged := mergeUsageObservations(&current, older)
	if !merged.Windows[0].LimitReached {
		t.Fatal("late old response erased newer limit")
	}
	metadata := prepareUsageSnapshot(events.AccountUsageSnapshot{ObservedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano), Windows: []events.UsageWindow{{ID: "header-primary", ResetAt: now.Add(time.Hour).Format(time.RFC3339Nano)}}}, "response_header")
	merged = mergeUsageObservations(merged, metadata)
	if !merged.Windows[0].LimitReached || merged.Windows[0].ExpiresAt != current.Windows[0].ExpiresAt {
		t.Fatal("reset metadata erased or prolonged known quota status")
	}
}

func TestQuotaScopesAndRetryHintsAgree(t *testing.T) {
	now := time.Now().UTC()
	snapshot := prepareUsageSnapshot(recoveredUsage(now.Add(-time.Second)), "scheduled")
	snapshot.Windows = append(snapshot.Windows,
		events.UsageWindow{ID: "code-review-primary", Scope: "code-review", LimitReached: true, ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano)},
		events.UsageWindow{ID: "additional-model-primary", Scope: "additional", Model: "gpt-reserve", LimitReached: true, ExpiresAt: now.Add(2 * time.Minute).Format(time.RFC3339Nano)},
		events.UsageWindow{ID: "additional-unknown-primary", Scope: "additional", LimitReached: true, ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano)},
	)
	item := &account{UsageSnapshot: &snapshot}
	for _, tc := range []struct {
		model string
		wait  time.Duration
	}{
		{"gpt-6.1-sol", 0}, {"codex-auto-review", time.Minute}, {"gpt-reserve", 2 * time.Minute},
	} {
		reset, blocked := usageLimitResetAt(item, now, tc.model)
		if blocked != (tc.wait > 0) || blocked && !reset.Equal(now.Add(tc.wait)) {
			t.Fatalf("model=%s reset=%v blocked=%v", tc.model, reset, blocked)
		}
	}
}

func TestOlderFullRefreshCannotOverwriteNewerFullResponse(t *testing.T) {
	s := openTestStore(t)
	_, _, _, _ = s.Import([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}})
	id := s.List()[0].ID
	now := time.Now().UTC()
	newest := recoveredUsage(now.Add(-time.Second))
	newest.Windows[0].UsedPercent = 80
	if _, err := s.PutUsageSnapshotWithSchedule(id, newest, "scheduled", now.Add(time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutUsageSnapshotWithSchedule(id, recoveredUsage(now.Add(-time.Minute)), "manual", ""); err != nil {
		t.Fatal(err)
	}
	view, _ := s.View(id)
	if view.UsageSnapshot.Windows[0].UsedPercent != 80 || view.UsageRefreshSource != "scheduled" || view.NextUsageRefreshAt == "" {
		t.Fatalf("old full response replaced new observation: %+v", view)
	}
}

func TestUsageRecoveryDoesNotOutrunNewerQuotaFailure(t *testing.T) {
	s := openTestStore(t)
	_, _, _, _ = s.Import([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}})
	id := s.List()[0].ID
	reset := time.Now().UTC().Add(5 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := s.RecordResult(id, "gpt-6.1-sol", false, events.ErrorRateLimit, 3600, true, reset, false); err != nil {
		t.Fatal(err)
	}
	snapshot := recoveredUsage(time.Now().UTC())
	if _, err := s.RecordResult(id, "gpt-6-luna", false, events.ErrorRateLimit, 60, true, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), false); err != nil {
		t.Fatal(err)
	}
	snapshot.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.PutUsageSnapshot(id, snapshot); err != nil {
		t.Fatal(err)
	}
	view, _ := s.View(id)
	if len(view.Cooldowns) != 1 || view.Cooldowns[0].Until != reset || len(view.QuotaObservations) != 1 {
		t.Fatalf("late recovery erased a newer failure or shortened its cooldown: %+v", view)
	}
}

func TestFailedUsagePersistenceCannotReleaseRoutingInMemory(t *testing.T) {
	s := openTestStore(t)
	_, _, _, _ = s.Import([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}})
	id := s.List()[0].ID
	if _, err := s.RecordResult(id, "gpt-6.1-sol", false, events.ErrorRateLimit, 3600, true, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), false); err != nil {
		t.Fatal(err)
	}
	snapshot := recoveredUsage(time.Now().UTC())
	snapshot.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.documents.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutUsageSnapshot(id, snapshot); err == nil {
		t.Fatal("closed storage accepted recovery")
	}
	view, _ := s.View(id)
	if len(view.Cooldowns) != 1 || len(view.QuotaObservations) != 1 || view.UsageSnapshot != nil {
		t.Fatalf("failed persistence changed live routing state: %+v", view)
	}
}
