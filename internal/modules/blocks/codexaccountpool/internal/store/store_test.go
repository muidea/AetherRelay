package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	events "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/events"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "state.duckdb"), "256MB", 1)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestImportValidatesWholeBatchAndRedactsView(t *testing.T) {
	store := openTestStore(t)
	_, _, _, err := store.Import([]events.CredentialInput{
		{AccessToken: "access-one", RefreshToken: "refresh-one", AccountID: "acct-secret", Proxy: "http://127.0.0.1:8080"},
		{AccessToken: "access-two", RefreshToken: "refresh-two", Proxy: "ftp://invalid.example"},
	})
	if err == nil {
		t.Fatal("expected malformed proxy to reject whole batch")
	}
	if got := store.List(); len(got) != 0 {
		t.Fatalf("partial import leaked into state: %+v", got)
	}

	added, updated, skipped, err := store.Import([]events.CredentialInput{{
		AccessToken: "access-one", RefreshToken: "refresh-one", AccountID: "acct-secret", Email: "operator@example.com", Proxy: "http://127.0.0.1:8080",
	}})
	if err != nil || added != 1 || updated != 0 || skipped != 0 {
		t.Fatalf("import = added=%d updated=%d skipped=%d err=%v", added, updated, skipped, err)
	}
	view, found := store.ViewByRefreshToken("refresh-one")
	if !found || view.ID == "" || view.Email != "operator@example.com" {
		t.Fatalf("redacted view = %+v found=%v", view, found)
	}
	payload, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"access-one", "refresh-one", "acct-secret", "127.0.0.1:8080"} {
		if string(payload) != "" && contains(string(payload), secret) {
			t.Fatalf("management projection leaked secret %q: %s", secret, payload)
		}
	}
}

func TestCooldownAndRefreshDue(t *testing.T) {
	store := openTestStore(t)
	now := time.Now().UTC()
	_, _, _, err := store.Import([]events.CredentialInput{
		{AccessToken: "access-due", RefreshToken: "refresh-due", Expired: now.Add(time.Minute).Format(time.RFC3339)},
		{AccessToken: "access-unknown", RefreshToken: "refresh-unknown"},
	})
	if err != nil {
		t.Fatal(err)
	}
	due := store.RefreshDue(now, 5*time.Minute)
	if len(due) != 1 {
		t.Fatalf("due accounts = %v, want one account with expiry", due)
	}
	account := store.items[due[0]]
	account.Cooldowns = map[string]cooldown{"gpt-5": {Until: now.Add(-time.Minute), ErrorClass: events.ErrorRateLimit}}
	if cooling(account, "gpt-5", now) {
		t.Fatal("expired cooldown must not block acquire")
	}
	if len(account.Cooldowns) != 1 {
		t.Fatal("read-only cooldown check must not mutate persisted account state")
	}
}

func TestQuotaObservationIsAccountAndModelScopedAndClearsOnSuccess(t *testing.T) {
	store := openTestStore(t)
	_, _, _, err := store.Import([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}})
	if err != nil {
		t.Fatal(err)
	}
	accountID := store.List()[0].ID
	resetAt := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	view, err := store.RecordResult(accountID, "gpt-5.2-codex", false, events.ErrorRateLimit, 30, true, resetAt)
	if err != nil || len(view.QuotaObservations) != 1 {
		t.Fatalf("quota record view=%+v err=%v", view, err)
	}
	observation := view.QuotaObservations[0]
	if observation.Model != "gpt-5.2-codex" || observation.State != "exhausted" || observation.ResetAt != resetAt {
		t.Fatalf("quota observation=%+v", observation)
	}
	if len(view.Cooldowns) != 1 || view.Cooldowns[0].Until != resetAt {
		t.Fatalf("quota reset must define the model cooldown: %+v", view.Cooldowns)
	}
	view, err = store.RecordResult(accountID, "gpt-5.2-codex", true, "", 0, false, "")
	if err != nil || len(view.QuotaObservations) != 0 {
		t.Fatalf("successful account result did not clear quota observation: %+v err=%v", view, err)
	}
	view, err = store.RecordResult(accountID, "gpt-5.2-codex", false, events.ErrorRateLimit, 30, false, resetAt)
	if err != nil || len(view.QuotaObservations) != 0 {
		t.Fatalf("generic rate limit must not create quota observation: %+v err=%v", view, err)
	}
}

func TestModelDiscoverySnapshotControlsCatalogAndAcquire(t *testing.T) {
	store := openTestStore(t)
	_, _, _, err := store.Import([]events.CredentialInput{
		{AccessToken: "access-a", RefreshToken: "refresh-a", AccountID: "account-a", Proxy: "http://127.0.0.1:8080"},
		{AccessToken: "access-b", RefreshToken: "refresh-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	accounts := store.List()
	if len(accounts) != 2 {
		t.Fatalf("accounts=%+v", accounts)
	}
	candidates := store.ListDiscoveryCandidates()
	if len(candidates.Candidates) != 2 || !candidates.Candidates[0].NeedsDiscovery || !candidates.Candidates[0].DiscoveryDue {
		t.Fatalf("discovery candidates=%+v", candidates)
	}
	if candidates.Candidates[0].AccountIDHeader != "account-a" || candidates.Candidates[0].Proxy != "http://127.0.0.1:8080" {
		t.Fatalf("candidate transport projection=%+v", candidates.Candidates[0])
	}

	now := time.Now().UTC()
	if _, ok, err := store.PutModelSnapshot(accounts[0].ID, events.AccountModelSnapshot{
		Models:       []events.AccountModelEntry{{ID: "gpt-5.2-codex", OwnedBy: "openai"}},
		DiscoveredAt: now.Format(time.RFC3339),
		ExpiresAt:    now.Add(time.Hour).Format(time.RFC3339),
	}); err != nil || !ok {
		t.Fatalf("put snapshot ok=%v err=%v", ok, err)
	}
	if _, err := store.Acquire("gpt-5.3-codex", nil); err == nil {
		t.Fatal("undiscovered model must not acquire an account")
	}
	acquired, err := store.Acquire("gpt-5.2-codex", nil)
	if err != nil || acquired.AccountID != accounts[0].ID {
		t.Fatalf("acquire=%+v err=%v", acquired, err)
	}
	catalog := store.CatalogSnapshot()
	if catalog.AvailableAccounts != 2 || len(catalog.Models) != 1 || catalog.Models[0].ID != "gpt-5.2-codex" || len(catalog.Models[0].AccountIDs) != 1 {
		t.Fatalf("catalog=%+v", catalog)
	}
	view, ok := store.View(accounts[0].ID)
	if !ok || view.ModelSnapshot == nil || len(view.ModelSnapshot.Models) != 1 {
		t.Fatalf("account view=%+v ok=%v", view, ok)
	}
	payload, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"access-a", "refresh-a", "account-a", "127.0.0.1:8080"} {
		if contains(string(payload), secret) {
			t.Fatalf("model discovery view leaked secret %q: %s", secret, payload)
		}
	}
}

func TestExpiredModelSnapshotIsNotPublishedAndFailureIsScoped(t *testing.T) {
	store := openTestStore(t)
	_, _, _, err := store.Import([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}})
	if err != nil {
		t.Fatal(err)
	}
	id := store.List()[0].ID
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, ok, err := store.PutModelSnapshot(id, events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-5.2-codex"}}, DiscoveredAt: past, ExpiresAt: past}); err != nil || !ok {
		t.Fatalf("put expired snapshot ok=%v err=%v", ok, err)
	}
	if models := store.CatalogSnapshot().Models; len(models) != 0 {
		t.Fatalf("expired snapshot was published: %+v", models)
	}
	if _, err := store.Acquire("gpt-5.2-codex", nil); err == nil {
		t.Fatal("expired snapshot must not acquire")
	}
	retryAt, found, err := store.RecordModelDiscoveryFailure(id, "upstream unavailable")
	if err != nil || !found || retryAt == "" {
		t.Fatalf("failure retry_at=%q found=%v err=%v", retryAt, found, err)
	}
	candidates := store.ListDiscoveryCandidates()
	if len(candidates.Candidates) != 1 || candidates.Candidates[0].DiscoveryDue {
		t.Fatalf("failed account should honor discovery backoff: %+v", candidates)
	}
}

func contains(value, needle string) bool {
	for index := 0; index+len(needle) <= len(value); index++ {
		if value[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
