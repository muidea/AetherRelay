package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	events "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "aetherrelay.duckdb"), "256MB", 1, encryptedTestCodec(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestImportValidatesWholeBatchAndReturnsSafeView(t *testing.T) {
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
		t.Fatalf("safe view = %+v found=%v", view, found)
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

func TestImportRejectsChatGPTWebCredentialType(t *testing.T) {
	store := openTestStore(t)
	_, _, _, err := store.Import([]events.CredentialInput{{CredentialType: "chatgpt_web", AccessToken: "access", RefreshToken: "refresh"}})
	if err == nil {
		t.Fatal("expected cross-client credential import to fail")
	}
}

func TestExportByIDsReturnsOnlySelectedCredentials(t *testing.T) {
	store := openTestStore(t)
	first := events.CredentialInput{AccessToken: "access-one", RefreshToken: "refresh-one", IDToken: "id-one", AccountID: "account-one", Proxy: "http://127.0.0.1:8080"}
	second := events.CredentialInput{AccessToken: "access-two", RefreshToken: "refresh-two"}
	if added, _, _, err := store.Import([]events.CredentialInput{first, second}); err != nil || added != 2 {
		t.Fatalf("import added=%d err=%v", added, err)
	}
	items := store.List()
	if len(items) != 2 {
		t.Fatalf("items=%+v", items)
	}
	exported := store.ExportByIDs([]string{items[0].ID, items[0].ID, "missing"})
	if len(exported) != 1 || exported[0].AccessToken != first.AccessToken || exported[0].RefreshToken != first.RefreshToken || exported[0].Proxy != first.Proxy {
		t.Fatalf("exported=%+v", exported)
	}
}

func TestImportCanReplaceCredentialForExplicitTargetID(t *testing.T) {
	store := openTestStore(t)
	if added, _, _, err := store.Import([]events.CredentialInput{{AccountID: "upstream-account", AccessToken: "old-access", RefreshToken: "old-refresh"}}); err != nil || added != 1 {
		t.Fatalf("initial import added=%d err=%v", added, err)
	}
	items := store.List()
	if len(items) != 1 {
		t.Fatalf("items=%+v", items)
	}
	input := events.CredentialInput{AccountID: "new-upstream-account", AccessToken: "new-access", RefreshToken: "new-refresh", TargetID: items[0].ID}
	if added, updated, _, err := store.Import([]events.CredentialInput{input}); err != nil || added != 0 || updated != 1 {
		t.Fatalf("replacement added=%d updated=%d err=%v", added, updated, err)
	}
	if got := store.List(); len(got) != 1 {
		t.Fatalf("replacement created a duplicate: %+v", got)
	}
	exported := store.ExportByIDs([]string{items[0].ID})
	if len(exported) != 1 || exported[0].AccessToken != "new-access" || exported[0].RefreshToken != "new-refresh" || exported[0].AccountID != "new-upstream-account" {
		t.Fatalf("replacement export=%+v", exported)
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
	candidates := store.ListDiscoveryCandidates(nil)
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
	retryAt, found, err := store.RecordModelDiscoveryFailure(id, events.ErrorUpstream)
	if err != nil || !found || retryAt == "" {
		t.Fatalf("failure retry_at=%q found=%v err=%v", retryAt, found, err)
	}
	candidates := store.ListDiscoveryCandidates(nil)
	if len(candidates.Candidates) != 1 || candidates.Candidates[0].DiscoveryDue || !candidates.Candidates[0].DiscoveryBackedOff {
		t.Fatalf("failed account should honor discovery backoff: %+v", candidates)
	}
}

func TestModelDiscoveryInvalidTokenQuarantinesAccountAndProjectsSafeClass(t *testing.T) {
	store := openTestStore(t)
	if _, _, _, err := store.Import([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}}); err != nil {
		t.Fatal(err)
	}
	id := store.List()[0].ID
	version := store.catalogVersion
	retryAt, found, err := store.RecordModelDiscoveryFailure(id, events.ErrorInvalidToken)
	if err != nil || !found || retryAt != "" {
		t.Fatalf("record retry_at=%q found=%v err=%v", retryAt, found, err)
	}
	view, found := store.View(id)
	if !found || view.Status != events.StatusAbnormal || view.ModelDiscoveryErrorClass != events.ErrorInvalidToken {
		t.Fatalf("invalid-token discovery view=%+v found=%v", view, found)
	}
	if candidates := store.ListDiscoveryCandidates(nil); len(candidates.Candidates) != 0 {
		t.Fatalf("quarantined account candidates=%+v", candidates)
	}
	if store.catalogVersion <= version {
		t.Fatalf("invalid token did not invalidate catalog: before=%d after=%d", version, store.catalogVersion)
	}
}

func TestModelDiscoveryFailureNormalizesUnsafeDetail(t *testing.T) {
	store := openTestStore(t)
	if _, _, _, err := store.Import([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}}); err != nil {
		t.Fatal(err)
	}
	id := store.List()[0].ID
	if _, found, err := store.RecordModelDiscoveryFailure(id, "upstream response: access"); err != nil || !found {
		t.Fatalf("record found=%v err=%v", found, err)
	}
	view, found := store.View(id)
	if !found || view.ModelDiscoveryErrorClass != events.ErrorUpstream || view.Status != events.StatusNormal {
		t.Fatalf("unsafe discovery detail leaked into view=%+v found=%v", view, found)
	}
}

func TestUsageSnapshotIsRedactedAndRetainedAfterRefreshFailure(t *testing.T) {
	store := openTestStore(t)
	_, _, _, err := store.Import([]events.CredentialInput{{
		AccessToken: "access-secret", RefreshToken: "refresh-secret", AccountID: "account-secret", Proxy: "http://127.0.0.1:8080",
	}})
	if err != nil {
		t.Fatal(err)
	}
	id := store.List()[0].ID
	now := time.Now().UTC()
	ok, err := store.PutUsageSnapshot(id, events.AccountUsageSnapshot{
		PlanType:   "pro",
		ObservedAt: now.Format(time.RFC3339),
		ExpiresAt:  now.Add(15 * time.Minute).Format(time.RFC3339),
		Windows: []events.UsageWindow{{
			ID: "standard-primary", UsedPercent: 37.5, UsedPercentKnown: true, WindowSeconds: 18000, ResetAt: now.Add(time.Hour).Format(time.RFC3339), Allowed: true, AllowedKnown: true,
		}},
	})
	if err != nil || !ok {
		t.Fatalf("put usage snapshot ok=%v err=%v", ok, err)
	}
	ok, err = store.RecordUsageFailure(id, "network")
	if err != nil || !ok {
		t.Fatalf("record usage failure ok=%v err=%v", ok, err)
	}
	view, found := store.View(id)
	if !found || view.UsageSnapshot == nil || view.UsageSnapshot.PlanType != "pro" || len(view.UsageSnapshot.Windows) != 1 || view.UsageRefreshError != "network" {
		t.Fatalf("usage view=%+v found=%v", view, found)
	}
	payload, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"access-secret", "refresh-secret", "account-secret", "127.0.0.1:8080"} {
		if contains(string(payload), secret) {
			t.Fatalf("usage projection leaked secret %q: %s", secret, payload)
		}
	}
}

func TestCredentialRefreshHealthDoesNotOverrideVerifiedAccessHealth(t *testing.T) {
	store := openTestStore(t)
	_, _, _, err := store.Import([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}})
	if err != nil {
		t.Fatal(err)
	}
	id := store.List()[0].ID
	if _, err := store.RecordRefreshFailure(id, events.ErrorInvalidToken, true); err != nil {
		t.Fatal(err)
	}
	view, found := store.View(id)
	if !found || view.Status != events.StatusNormal || view.LastTokenRefreshErrorClass != events.ErrorInvalidToken {
		t.Fatalf("refresh failure view=%+v found=%v", view, found)
	}

	abnormal := events.StatusAbnormal
	if _, err := store.Update(id, &abnormal, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if ok, err := store.PutUsageSnapshot(id, events.AccountUsageSnapshot{ObservedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339)}); err != nil || !ok {
		t.Fatalf("put usage ok=%v err=%v", ok, err)
	}
	view, _ = store.View(id)
	if view.Status != events.StatusNormal || view.LastTokenRefreshErrorClass != events.ErrorInvalidToken {
		t.Fatalf("usage recovery view=%+v", view)
	}

	if _, err := store.Update(id, &abnormal, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.PutModelSnapshot(id, events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-test"}}, DiscoveredAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}); err != nil || !ok {
		t.Fatalf("put model snapshot ok=%v err=%v", ok, err)
	}
	view, _ = store.View(id)
	if view.Status != events.StatusNormal || view.LastTokenRefreshErrorClass != events.ErrorInvalidToken {
		t.Fatalf("model recovery view=%+v", view)
	}

	disabled := events.StatusDisabled
	if _, err := store.Update(id, &disabled, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.PutUsageSnapshot(id, events.AccountUsageSnapshot{ObservedAt: now.Format(time.RFC3339)}); err != nil || !ok {
		t.Fatalf("put disabled usage ok=%v err=%v", ok, err)
	}
	view, _ = store.View(id)
	if view.Status != events.StatusDisabled {
		t.Fatalf("verified usage bypassed disabled state: %+v", view)
	}
}

func TestExplicitUsageCandidatesCanRetryAbnormalButNotDisabledAccounts(t *testing.T) {
	store := openTestStore(t)
	_, _, _, err := store.Import([]events.CredentialInput{
		{AccessToken: "normal-access", RefreshToken: "normal-refresh"},
		{AccessToken: "abnormal-access", RefreshToken: "abnormal-refresh"},
		{AccessToken: "disabled-access", RefreshToken: "disabled-refresh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items := store.List()
	if len(items) != 3 {
		t.Fatalf("items=%#v", items)
	}
	statusByID := map[string]string{items[0].ID: events.StatusNormal, items[1].ID: events.StatusAbnormal, items[2].ID: events.StatusDisabled}
	for id, status := range statusByID {
		if _, err := store.Update(id, &status, nil); err != nil {
			t.Fatal(err)
		}
	}

	if got := store.ListUsageCandidates(nil).Candidates; len(got) != 1 || got[0].AccountID != items[0].ID {
		t.Fatalf("unscoped candidates=%#v", got)
	}
	ids := []string{items[0].ID, items[1].ID, items[2].ID}
	if got := store.ListUsageCandidates(ids).Candidates; len(got) != 2 || got[0].AccountID != items[0].ID || got[1].AccountID != items[1].ID {
		t.Fatalf("explicit candidates=%#v", got)
	}
}

func TestExplicitDiscoveryCandidatesCanRetryAbnormalButNotDisabledAccounts(t *testing.T) {
	store := openTestStore(t)
	_, _, _, err := store.Import([]events.CredentialInput{
		{AccessToken: "normal-access", RefreshToken: "normal-refresh"},
		{AccessToken: "abnormal-access", RefreshToken: "abnormal-refresh"},
		{AccessToken: "disabled-access", RefreshToken: "disabled-refresh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items := store.List()
	statusByID := map[string]string{items[0].ID: events.StatusNormal, items[1].ID: events.StatusAbnormal, items[2].ID: events.StatusDisabled}
	for id, status := range statusByID {
		if _, err := store.Update(id, &status, nil); err != nil {
			t.Fatal(err)
		}
	}

	if got := store.ListDiscoveryCandidates(nil).Candidates; len(got) != 1 || got[0].AccountID != items[0].ID {
		t.Fatalf("automatic candidates=%#v", got)
	}
	ids := []string{items[0].ID, items[1].ID, items[2].ID}
	if got := store.ListDiscoveryCandidates(ids).Candidates; len(got) != 2 || got[0].AccountID != items[0].ID || got[1].AccountID != items[1].ID {
		t.Fatalf("explicit candidates=%#v", got)
	}
}

func TestTransportCapabilitiesPrioritizeSupportedAndExcludeUnsupported(t *testing.T) {
	store := openTestStore(t)
	_, _, _, err := store.Import([]events.CredentialInput{
		{AccessToken: "unknown-access", RefreshToken: "unknown-refresh"},
		{AccessToken: "supported-access", RefreshToken: "supported-refresh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items := store.List()
	if len(items) != 2 {
		t.Fatalf("items=%#v", items)
	}
	now := time.Now().UTC()
	for _, item := range items {
		if _, ok, err := store.PutModelSnapshot(item.ID, events.AccountModelSnapshot{
			Models: []events.AccountModelEntry{{ID: "gpt-test"}}, DiscoveredAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		}); err != nil || !ok {
			t.Fatalf("put model snapshot account=%s ok=%v err=%v", item.ID, ok, err)
		}
	}
	if _, err := store.RecordTransportCapability(items[1].ID, events.TransportCompact, true); err != nil {
		t.Fatal(err)
	}
	acquired, err := store.AcquirePreferredTransport("gpt-test", nil, "", events.TransportCompact)
	if err != nil || acquired.AccountID != items[1].ID {
		t.Fatalf("supported account was not preferred: acquired=%+v err=%v", acquired, err)
	}
	if _, err := store.RecordTransportCapability(items[1].ID, events.TransportCompact, false); err != nil {
		t.Fatal(err)
	}
	acquired, err = store.AcquirePreferredTransport("gpt-test", nil, "", events.TransportCompact)
	if err != nil || acquired.AccountID != items[0].ID {
		t.Fatalf("unsupported account was not excluded: acquired=%+v err=%v", acquired, err)
	}
	view, _ := store.View(items[1].ID)
	if view.CompactSupported == nil || *view.CompactSupported {
		t.Fatalf("compact capability not projected: %+v", view)
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
