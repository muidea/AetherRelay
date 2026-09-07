package store

import (
	"path/filepath"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

func TestModelNotFoundCooldownAndAvailability(t *testing.T) {
	// CP-FAIL-018 / CP-CAP-010: durable, account/model-local and reversible.
	path := filepath.Join(t.TempDir(), "state.duckdb")
	codec := encryptedTestCodec(t)
	s, err := Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, _, _, err = s.Import([]events.CredentialInput{{AccessToken: "test-access", RefreshToken: "test-refresh"}, {AccessToken: "test-access-2", RefreshToken: "test-refresh-2"}}); err != nil {
		t.Fatal(err)
	}
	ids := s.List()
	now := time.Now().UTC()
	snapshot := events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-5.5"}, {ID: "gpt-6-astra"}}, DiscoveredAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
	for _, id := range ids {
		if _, _, err = s.PutModelSnapshot(id.ID, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	id := ids[0].ID
	view, err := s.RecordResult(id, "gpt-5.5", false, events.ErrorModelNotFound, 999999, false, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != events.StatusNormal || len(view.AvailableModels) != 1 || view.AvailableModels[0] != "gpt-6-astra" || len(view.Cooldowns) != 1 || view.Cooldowns[0].ErrorClass != events.ErrorModelNotFound {
		t.Fatalf("view=%+v", view)
	}
	until, err := time.Parse(time.RFC3339, view.Cooldowns[0].Until)
	if err != nil || until.Sub(now) > 5*time.Minute+time.Second || until.Sub(now) < 4*time.Minute {
		t.Fatalf("unbounded TTL %s %v", until, err)
	}
	got, err := s.AcquirePreferred("gpt-5.5", nil, id)
	if err != nil || got.AccountID == id {
		t.Fatalf("sticky selected unavailable account: %+v %v", got, err)
	}
	got, err = s.AcquirePreferred("gpt-6-astra", nil, id)
	if err != nil || got.AccountID != id {
		t.Fatalf("other model blocked: %+v %v", got, err)
	}
	if _, _, err = s.PutModelSnapshot(id, snapshot); err != nil {
		t.Fatal(err)
	}
	view, _ = s.View(id)
	if len(view.AvailableModels) != 1 {
		t.Fatal("discovery prematurely cleared negative observation")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	view, _ = s.View(id)
	if len(view.AvailableModels) != 1 {
		t.Fatal("cooldown lost on restart")
	}
	item := s.items[id]
	if later := modelAvailability(item, "gpt-5.5", until.Add(time.Second)); !later.Available {
		t.Fatalf("expiry failed: %+v", later)
	}
	if later := modelAvailability(item, "gpt-5.5", now.Add(2*time.Hour)); later.Available || later.Reason != "snapshot_expired" {
		t.Fatalf("stale snapshot=%+v", later)
	}
	item.Status = events.StatusDisabled
	if modelAvailability(item, "gpt-6-astra", now).Available {
		t.Fatal("disabled account advertised")
	}
	item.Status = events.StatusNormal
	if _, _, _, err = s.Import([]events.CredentialInput{{TargetID: id, AccessToken: "replacement-access", RefreshToken: "replacement-refresh"}}); err != nil {
		t.Fatal(err)
	}
	view, _ = s.View(id)
	if len(view.Cooldowns) != 0 || len(view.AvailableModels) != 0 || view.ModelSnapshot != nil {
		t.Fatalf("replacement inherited old state: %+v", view)
	}
}

func TestModelAvailabilityGlobalQuotaAndUnknown(t *testing.T) {
	// Management uses the same account admission predicates as scheduling.
	now := time.Now().UTC()
	item := &account{ID: "test", Status: events.StatusNormal, AccessToken: "test"}
	view := toView(item, now)
	if view.AvailableModels == nil || view.ModelAvailability == nil || len(view.AvailableModels) != 0 {
		t.Fatalf("unknown=%+v", view)
	}
	item.ModelSnapshot = &events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-5.5"}, {ID: "gpt-6-astra"}}}
	item.Cooldowns = map[string]cooldown{"": {Until: now.Add(time.Minute), ErrorClass: events.ErrorRateLimit}}
	view = toView(item, now)
	if len(view.AvailableModels) != 0 || len(view.ModelAvailability) != 2 || view.ModelAvailability[0].Reason != "rate_limit" {
		t.Fatalf("quota=%+v", view)
	}
	item.Cooldowns = nil
	item.UsageSnapshot = &events.AccountUsageSnapshot{ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), Windows: []events.UsageWindow{{ID: "primary", LimitReached: true}}}
	if got := modelAvailability(item, "gpt-5.5", now); got.Available || got.Reason != "usage_limit" {
		t.Fatalf("usage=%+v", got)
	}
}
