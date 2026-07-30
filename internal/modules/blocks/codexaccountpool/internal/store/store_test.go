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

func contains(value, needle string) bool {
	for index := 0; index+len(needle) <= len(value); index++ {
		if value[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
