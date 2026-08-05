package store

import (
	"path/filepath"
	"testing"
	"time"

	events "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
)

func TestModelSnapshotPersistenceAndCatalogUnion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	s := New(path, 2)
	if _, _, err := s.Add([]string{"token-text", "token-image"}, "web"); err != nil {
		t.Fatalf("add: %v", err)
	}
	list := s.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}
	textID, imageID := list[0].ID, list[1].ID

	now := time.Now().UTC()
	expire := now.Add(time.Hour).Format(time.RFC3339)
	v1, ok, err := s.PutModelSnapshot(textID, events.AccountModelSnapshot{
		Models: []events.AccountModelEntry{{
			ID: "gpt-5", Capabilities: []string{events.ModelCapabilityTextGeneration},
		}},
		DiscoveredAt: now.Format(time.RFC3339),
		ExpiresAt:    expire,
	})
	if err != nil || !ok || v1 == 0 {
		t.Fatalf("put text snapshot version=%d ok=%v err=%v", v1, ok, err)
	}
	v2, ok, err := s.PutModelSnapshot(imageID, events.AccountModelSnapshot{
		Models: []events.AccountModelEntry{{
			ID: "gpt-image-2", Capabilities: []string{events.ModelCapabilityTextGeneration, events.ModelCapabilityImageGeneration},
		}},
		DiscoveredAt: now.Format(time.RFC3339),
		ExpiresAt:    expire,
	})
	if err != nil || !ok || v2 <= v1 {
		t.Fatalf("put image snapshot version=%d ok=%v err=%v", v2, ok, err)
	}

	catalog := s.CatalogSnapshot()
	if catalog.Version != v2 || catalog.AvailableAccounts != 2 || len(catalog.Models) != 2 {
		t.Fatalf("catalog=%+v", catalog)
	}
	byID := map[string]events.CatalogModel{}
	for _, model := range catalog.Models {
		byID[model.ID] = model
	}
	if len(byID["gpt-5"].Capabilities) != 1 || byID["gpt-5"].Capabilities[0] != events.ModelCapabilityTextGeneration {
		t.Fatalf("gpt-5 ops=%v", byID["gpt-5"].Capabilities)
	}
	if len(byID["gpt-image-2"].Capabilities) != 2 {
		t.Fatalf("gpt-image-2 ops=%v", byID["gpt-image-2"].Capabilities)
	}

	// Reload from disk and ensure snapshots survive.
	reloaded := New(path, 2)
	if reloaded.CatalogVersion() != 0 {
		// version is process-local; only snapshots are durable.
	}
	catalog = reloaded.CatalogSnapshot()
	if len(catalog.Models) != 2 {
		t.Fatalf("reloaded catalog models=%+v", catalog.Models)
	}
	candidates, err := reloaded.ListDiscoveryCandidates()
	if err != nil || len(candidates.Candidates) != 2 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	for _, candidate := range candidates.Candidates {
		if candidate.NeedsDiscovery {
			t.Fatalf("fresh snapshot should not need discovery: %+v", candidate)
		}
	}
}

func TestAcquireFiltersByModelSnapshot(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "accounts.json"), 2)
	if _, _, err := s.Add([]string{"token-a", "token-b"}, "web"); err != nil {
		t.Fatalf("add: %v", err)
	}
	list := s.List()
	aID, bID := list[0].ID, list[1].ID
	now := time.Now().UTC().Format(time.RFC3339)
	expire := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, _, err := s.PutModelSnapshot(aID, events.AccountModelSnapshot{
		Models:       []events.AccountModelEntry{{ID: "gpt-5", Capabilities: []string{events.ModelCapabilityTextGeneration}}},
		DiscoveredAt: now, ExpiresAt: expire,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutModelSnapshot(bID, events.AccountModelSnapshot{
		Models: []events.AccountModelEntry{{
			ID: "gpt-image-2", Capabilities: []string{events.ModelCapabilityImageGeneration},
		}},
		DiscoveredAt: now, ExpiresAt: expire,
	}); err != nil {
		t.Fatal(err)
	}

	// Empty model keeps legacy behavior.
	if _, ok := s.AcquireTextToken(nil, "", ""); !ok {
		t.Fatal("legacy acquire should succeed")
	}
	// Model-aware text only picks account A.
	got, ok := s.AcquireTextToken(nil, "gpt-5", events.ModelCapabilityTextGeneration)
	if !ok || got.ID != aID {
		t.Fatalf("text acquire=%+v ok=%v", got, ok)
	}
	if _, ok := s.AcquireTextToken(nil, "gpt-image-2", events.ModelCapabilityTextGeneration); ok {
		t.Fatal("image-only model should not serve chat")
	}
	// Image acquire needs quota > 0 and normal status; set quotas.
	quota := 2
	if _, _, err := s.UpdateByID(bID, nil, nil, &quota, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpdateByID(aID, nil, nil, &quota, nil); err != nil {
		t.Fatal(err)
	}
	img, ok := s.AcquireImageToken("", "", nil, "gpt-image-2", events.ModelCapabilityImageGeneration)
	if !ok || img.ID != bID {
		t.Fatalf("image acquire=%+v ok=%v", img, ok)
	}
	if _, ok := s.AcquireImageToken("", "", nil, "gpt-5", events.ModelCapabilityImageGeneration); ok {
		t.Fatal("text-only model should not serve image")
	}
}

func TestExpiredSnapshotExcludedFromCatalogAndAcquire(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "accounts.json"), 1)
	if _, _, err := s.Add([]string{"token-x"}, "web"); err != nil {
		t.Fatal(err)
	}
	id := s.List()[0].ID
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, _, err := s.PutModelSnapshot(id, events.AccountModelSnapshot{
		Models:       []events.AccountModelEntry{{ID: "gpt-5", Capabilities: []string{events.ModelCapabilityTextGeneration}}},
		DiscoveredAt: past,
		ExpiresAt:    past,
	}); err != nil {
		t.Fatal(err)
	}
	if models := s.CatalogSnapshot().Models; len(models) != 0 {
		t.Fatalf("expired models should be excluded: %+v", models)
	}
	if _, ok := s.AcquireTextToken(nil, "gpt-5", events.ModelCapabilityTextGeneration); ok {
		t.Fatal("expired snapshot must not be acquired")
	}
	candidates, _ := s.ListDiscoveryCandidates()
	if len(candidates.Candidates) != 1 || !candidates.Candidates[0].NeedsDiscovery {
		t.Fatalf("expired account should need discovery: %+v", candidates)
	}
}

func TestModelDiscoveryFailureBackoffOnlyDefersAffectedCandidate(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 1)
	if _, _, err := s.Add([]string{"token-a", "token-b"}, "web"); err != nil {
		t.Fatal(err)
	}
	accounts := s.List()
	if len(accounts) != 2 {
		t.Fatalf("accounts=%d", len(accounts))
	}
	failedID := accounts[0].ID
	retryAt, found, err := s.RecordModelDiscoveryFailure(failedID, "upstream unavailable")
	if err != nil || !found || retryAt == "" {
		t.Fatalf("record retry_at=%q found=%v err=%v", retryAt, found, err)
	}

	candidates, err := s.ListDiscoveryCandidates()
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates.Candidates {
		if !candidate.NeedsDiscovery {
			t.Fatalf("new account should need discovery: %+v", candidate)
		}
		if candidate.AccountID == failedID && candidate.DiscoveryDue {
			t.Fatalf("failed candidate ignored retry backoff: %+v", candidate)
		}
		if candidate.AccountID != failedID && !candidate.DiscoveryDue {
			t.Fatalf("unrelated candidate was throttled: %+v", candidate)
		}
	}
}

func TestModelDiscoveryRetryDelayIsBounded(t *testing.T) {
	if got := modelDiscoveryRetryDelay(1); got != modelDiscoveryRetryBase {
		t.Fatalf("first retry delay=%s", got)
	}
	if got := modelDiscoveryRetryDelay(2); got != 2*modelDiscoveryRetryBase {
		t.Fatalf("second retry delay=%s", got)
	}
	if got := modelDiscoveryRetryDelay(99); got != modelDiscoveryRetryMax {
		t.Fatalf("bounded retry delay=%s", got)
	}
}

func TestCatalogVersionTracksRoutingAvailabilityChanges(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 1)
	if _, _, err := s.Add([]string{"token-a"}, "web"); err != nil {
		t.Fatal(err)
	}
	id := s.List()[0].ID
	version := s.CatalogVersion()
	if version == 0 {
		t.Fatal("account add must change discovery generation")
	}
	status := StatusDisabled
	if _, _, err := s.UpdateByID(id, nil, &status, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := s.CatalogVersion(); got <= version {
		t.Fatalf("disable did not change discovery generation: got=%d before=%d", got, version)
	}
	version = s.CatalogVersion()
	status = StatusNormal
	quota := 2
	if _, _, err := s.UpdateByID(id, nil, &status, &quota, nil); err != nil {
		t.Fatal(err)
	}
	if got := s.CatalogVersion(); got <= version {
		t.Fatalf("status/quota update did not change discovery generation: got=%d before=%d", got, version)
	}
}
