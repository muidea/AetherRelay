// Package store tests account-pool persistence.
package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAccountPoolAcquireAndMark(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	s := New(path, 1)

	added, skipped, err := s.Add([]string{"token-a", "token-b", "token-a"}, "web")
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 || skipped != 1 {
		t.Fatalf("added=%d skipped=%d", added, skipped)
	}

	// no quota yet
	if _, ok := s.AcquireImageToken("", "", nil); ok {
		t.Fatal("should not acquire without quota")
	}

	quota := 2
	if _, ok, err := s.Update("token-a", "Plus", StatusNormal, &quota, ""); err != nil || !ok {
		t.Fatalf("update failed ok=%v err=%v", ok, err)
	}

	acc, ok := s.AcquireImageToken("", "", nil)
	if !ok || acc.AccessToken != "token-a" {
		t.Fatalf("acquire failed: %+v", acc)
	}
	// concurrency=1, same token should not acquire again
	if _, ok := s.AcquireImageToken("", "", nil); ok {
		t.Fatal("second acquire should fail due to slot")
	}
	s.ReleaseImageSlot("token-a")
	acc2, ok := s.AcquireImageToken("", "", nil)
	if !ok {
		t.Fatal("acquire after release failed")
	}
	s.MarkImageResult(acc2.AccessToken, true)
	s.ReleaseImageSlot(acc2.AccessToken)

	h := s.Health()
	if h.Total != 2 {
		t.Fatalf("total=%d", h.Total)
	}
}

func TestAcquireImageAccountUsesStableAccountID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1)
	if _, _, err := s.Add([]string{"token-a"}, "web"); err != nil {
		t.Fatal(err)
	}
	quota := 1
	account, found, err := s.Update("token-a", "plus", StatusNormal, &quota, "")
	if err != nil || !found {
		t.Fatalf("update found=%v err=%v", found, err)
	}
	acquired, ok := s.AcquireImageAccount(account.ID)
	if !ok || acquired.AccessToken != "token-a" || acquired.ID != account.ID {
		t.Fatalf("acquired=%+v ok=%v", acquired, ok)
	}
	if _, ok := s.AcquireImageAccount(account.ID); ok {
		t.Fatal("specific account acquisition ignored its inflight slot")
	}
	s.ReleaseImageSlot("token-a")
}

func TestStableIDManagementDoesNotRequireAccessTokenSelector(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1)
	if _, _, err := s.Add([]string{"token-a", "token-b"}, "web"); err != nil {
		t.Fatal(err)
	}
	items := s.List()
	if len(items) != 2 || items[0].ID == "" {
		t.Fatalf("items=%+v", items)
	}
	proxy, status, quota := "http://proxy.invalid", StatusLimited, 0
	updated, found, err := s.UpdateByID(items[0].ID, nil, &status, &quota, &proxy)
	if err != nil || !found || updated.Status != StatusLimited || updated.Quota != 0 {
		t.Fatalf("updated=%+v found=%v err=%v", updated, found, err)
	}
	if got := s.RefreshCandidatesForIDs([]string{items[0].ID}); len(got) != 1 || got[0].ID != items[0].ID {
		t.Fatalf("refresh candidates=%+v", got)
	}
	deleted, err := s.DeleteByIDs([]string{items[1].ID})
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	if got := s.List(); len(got) != 1 || got[0].ID != items[0].ID {
		t.Fatalf("remaining items=%+v", got)
	}
}

func TestExportByIDsSelectsOnlyRequestedOAuthAccount(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 1)
	first, _, err := s.AddOAuth("access-first", "refresh-first", "id-first")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddOAuth("access-second", "refresh-second", "id-second"); err != nil {
		t.Fatal(err)
	}
	items := s.ExportByIDs([]string{first.ID})
	if len(items) != 1 || items[0].AccessToken != "access-first" {
		t.Fatalf("export=%+v", items)
	}
}

func TestRefreshProjectionRestoresLimitedAccountAndPreservesOperatorStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1)
	if _, _, err := s.Add([]string{"token-a", "token-b"}, "web"); err != nil {
		t.Fatal(err)
	}
	zero := 0
	if _, ok, err := s.Update("token-a", "", StatusLimited, &zero, ""); err != nil || !ok {
		t.Fatalf("prepare limited account: ok=%v err=%v", ok, err)
	}
	if updated, err := s.ApplyUpstreamInfo("token-a", "account@example.invalid", "plus", 4, "2027-01-01T00:00:00Z"); err != nil || !updated {
		t.Fatalf("apply upstream info: updated=%v err=%v", updated, err)
	}
	if _, ok, err := s.Update("token-b", "", StatusDisabled, &zero, ""); err != nil || !ok {
		t.Fatalf("prepare disabled account: ok=%v err=%v", ok, err)
	}
	if updated, err := s.ApplyUpstreamInfo("token-b", "", "plus", 4, ""); err != nil || updated {
		t.Fatalf("disabled account changed: updated=%v err=%v", updated, err)
	}

	reloaded := New(path, 1)
	first := reloaded.items["token-a"]
	if first == nil || first.Status != StatusNormal || first.Quota != 4 || first.Type != "plus" || first.Email != "account@example.invalid" || first.Extra["restore_at"] != "2027-01-01T00:00:00Z" {
		t.Fatalf("refreshed account=%+v", first)
	}
	if second := reloaded.items["token-b"]; second == nil || second.Status != StatusDisabled {
		t.Fatalf("disabled account=%+v", second)
	}
}

func TestRefreshCandidatesForDoesNotTreatUnknownSelectionAsAllAccounts(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 1)
	if _, _, err := s.Add([]string{"token-a", "token-b"}, "web"); err != nil {
		t.Fatal(err)
	}
	if got := s.RefreshCandidatesFor([]string{"unknown-token"}); len(got) != 0 {
		t.Fatalf("unknown selection returned %d accounts", len(got))
	}
	if got := s.RefreshCandidatesFor(nil); len(got) != 2 {
		t.Fatalf("empty selection returned %d accounts", len(got))
	}
}

func TestRefreshCandidatesIncludeAllChatGPTWebCredentialSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	data := []byte(`[
  {"access_token":"token-web","source_type":"web","status":"正常"},
  {"access_token":"token-oauth","source_type":"oauth_login","status":"正常"},
  {"access_token":"token-password","source_type":"password","status":"限流"},
  {"access_token":"token-other","source_type":"api","status":"正常"}
]`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	accounts := New(path, 3)
	candidates := accounts.RefreshCandidates()
	if len(candidates) != 3 {
		t.Fatalf("candidate count=%d candidates=%#v", len(candidates), candidates)
	}
	for _, item := range candidates {
		if item.SourceType == "api" {
			t.Fatalf("non-Web source was selected: %#v", item)
		}
	}
}

func TestApplyRefreshedTokenCarriesImageSlotAndAliasesOldToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1)
	oldToken := testJWT(time.Now().Add(30*time.Minute), time.Now().Add(-time.Hour))
	newToken := testJWT(time.Now().Add(24*time.Hour), time.Now())
	if _, _, err := s.Add([]string{oldToken}, "web"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Update(oldToken, "plus", StatusNormal, intPointer(1), ""); err != nil || !ok {
		t.Fatalf("prepare account: ok=%v err=%v", ok, err)
	}
	if _, ok := s.AcquireImageToken("", "", nil); !ok {
		t.Fatal("acquire image slot")
	}
	got, rotated, err := s.ApplyRefreshedToken(oldToken, newToken, "refresh-new", "id-new")
	if err != nil || !rotated || got != newToken {
		t.Fatalf("rotate got=%q rotated=%v err=%v", got, rotated, err)
	}
	s.ReleaseImageSlot(oldToken)
	if acquired, ok := s.AcquireImageToken("", "", nil); !ok || acquired.AccessToken != newToken {
		t.Fatalf("old token alias did not release slot: account=%+v ok=%v", acquired, ok)
	}
	s.ReleaseImageSlot(newToken)
	reloaded := New(path, 1)
	account := reloaded.items[newToken]
	if account == nil || account.RefreshToken != "refresh-new" || account.Extra["id_token"] != "id-new" || account.Extra["last_token_refresh_at"] == nil {
		t.Fatalf("refreshed account=%+v", account)
	}
}

func TestTokenRefreshCandidatesPreferExpiringAndBoundKeepalive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1)
	now := time.Now().UTC()
	expiring := testJWT(now.Add(time.Hour), now.Add(-time.Hour))
	keepaliveOne := testJWT(now.Add(10*24*time.Hour), now.Add(-4*24*time.Hour))
	keepaliveTwo := testJWT(now.Add(10*24*time.Hour), now.Add(-5*24*time.Hour))
	if _, _, err := s.Add([]string{expiring, keepaliveOne, keepaliveTwo}, "web"); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{expiring, keepaliveOne, keepaliveTwo} {
		s.items[token].RefreshToken = "refresh-" + token[:8]
	}
	candidates := s.TokenRefreshCandidates(now, 24*time.Hour, 72*time.Hour, 6*time.Hour, 1)
	if len(candidates) != 2 || candidates[0].AccessToken != expiring || candidates[0].Reason != "expiring" || candidates[1].Reason != "keepalive" {
		t.Fatalf("candidates=%+v", candidates)
	}
}

func TestAddOAuthPersistsRefreshAndIDTokenWithoutPublicLeak(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1)
	item, added, err := s.AddOAuth("access-oauth", "refresh-oauth", "id-oauth")
	if err != nil || !added || item.AccessToken != "access-oauth" {
		t.Fatalf("item=%#v added=%v err=%v", item, added, err)
	}
	item, added, err = s.AddOAuth("access-oauth", "refresh-new", "id-new")
	if err != nil || added {
		t.Fatalf("duplicate item=%#v added=%v err=%v", item, added, err)
	}
	reloaded := New(path, 1)
	account := reloaded.items["access-oauth"]
	if account == nil || account.RefreshToken != "refresh-new" || account.Extra["id_token"] != "id-new" || account.SourceType != "oauth_login" {
		t.Fatalf("account=%#v", account)
	}
}

func TestExportReturnsOnlyCompleteOAuthAccounts(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 1)
	if _, _, err := s.AddOAuth("access-complete", "refresh-complete", "id-complete"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddOAuth("access-second", "refresh-second", "id-second"); err != nil {
		t.Fatal(err)
	}
	if got := s.Export([]string{"access-complete"}); len(got) != 1 || got[0].AccessToken != "access-complete" {
		t.Fatalf("export count=%d", len(got))
	}
}

func TestExportBuildsCodexShapeFromJWTClaims(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 1)
	accessToken := testJWTWithClaims(map[string]any{
		"exp":                            float64(0),
		"iat":                            float64(3600),
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct-123"},
		"https://api.openai.com/profile": map[string]any{"email": "account@example.invalid"},
	})
	idToken := testJWTWithClaims(map[string]any{"email": "fallback@example.invalid"})
	if _, _, err := s.AddOAuth(accessToken, "refresh-token", idToken); err != nil {
		t.Fatal(err)
	}
	items := s.Export([]string{accessToken})
	if len(items) != 1 {
		t.Fatalf("items=%d", len(items))
	}
	item := items[0]
	if item.Type != "codex" || item.Email != "account@example.invalid" || item.AccountID != "acct-123" || item.Expired != "1970-01-01T08:00:00+08:00" || item.LastRefresh != "1970-01-01T09:00:00+08:00" {
		t.Fatalf("item=%#v", item)
	}
}

func intPointer(value int) *int { return &value }

func testJWT(expiry, issuedAt time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"iat":%d}`, expiry.Unix(), issuedAt.Unix())))
	return "header." + payload + ".signature"
}

func testJWTWithClaims(claims map[string]any) string {
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
