// Package store tests account-pool persistence.
package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	events "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
)

func TestAccountPoolAcquireAndMark(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	s := New(path, 1, encryptedTestCodec(t))

	added, skipped, err := s.Add([]string{"token-a", "token-b", "token-a"}, "web")
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 || skipped != 1 {
		t.Fatalf("added=%d skipped=%d", added, skipped)
	}

	// no quota yet
	if _, ok := s.AcquireImageToken("", "", nil, "", ""); ok {
		t.Fatal("should not acquire without quota")
	}

	quota := 2
	if _, ok, err := s.Update("token-a", "Plus", StatusNormal, &quota, ""); err != nil || !ok {
		t.Fatalf("update failed ok=%v err=%v", ok, err)
	}

	acc, ok := s.AcquireImageToken("", "", nil, "", "")
	if !ok || acc.AccessToken != "token-a" {
		t.Fatalf("acquire failed: %+v", acc)
	}
	// concurrency=1, same token should not acquire again
	if _, ok := s.AcquireImageToken("", "", nil, "", ""); ok {
		t.Fatal("second acquire should fail due to slot")
	}
	s.ReleaseImageSlot("token-a")
	acc2, ok := s.AcquireImageToken("", "", nil, "", "")
	if !ok {
		t.Fatal("acquire after release failed")
	}
	result, marked := s.MarkImageResult(acc2.AccessToken, "", true, "")
	if !marked || result.Success != 1 || result.Fail != 0 || result.ImageInflight != 1 {
		t.Fatalf("image result accounting=%+v marked=%v", result, marked)
	}
	s.ReleaseImageSlot(acc2.AccessToken)

	h := s.Health()
	if h.Total != 2 {
		t.Fatalf("total=%d", h.Total)
	}
}

func TestListProjectsLegacyAccountCapabilitiesFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1, encryptedTestCodec(t))
	if _, _, err := s.Add([]string{"token-a"}, "web"); err != nil {
		t.Fatal(err)
	}
	acc := s.items["token-a"]
	acc.Quota = 2
	acc.CreatedAt = "2026-07-26T01:02:03Z"
	acc.Extra = map[string]any{"restore_at": "2026-07-26T03:04:05Z", "success": 4, "fail": 2}
	s.items["token-a"] = acc
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.AcquireImageToken("", "", nil, "", ""); !ok {
		t.Fatal("acquire image token")
	}
	items := s.List()
	if len(items) != 1 {
		t.Fatalf("items=%d", len(items))
	}
	item := items[0]
	if item.CreatedAt == "" || item.RestoreAt == "" || item.ImageInflight != 1 || item.Success != 4 || item.Fail != 2 {
		t.Fatalf("legacy account capabilities view=%+v", item)
	}
}

func TestAcquireImageAccountUsesStableAccountID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1, encryptedTestCodec(t))
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
	s := New(path, 1, encryptedTestCodec(t))
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
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 1, encryptedTestCodec(t))
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

func TestImportRestoresCompleteOAuthExport(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 3, encryptedTestCodec(t))
	input := events.ExportItem{
		Type: "codex", Email: "operator@example.invalid", AccountID: "account-header",
		AccessToken: "access-import", RefreshToken: "refresh-import", IDToken: "id-import",
		Expired: "2026-08-06T00:00:00Z", LastRefresh: "2026-08-05T00:00:00Z",
		Password: "password-import", Proxy: "http://127.0.0.1:8080",
	}
	added, updated, skipped, err := s.Import(nil, []events.ExportItem{input}, "")
	if err != nil || added != 1 || updated != 0 || skipped != 0 {
		t.Fatalf("import result added=%d updated=%d skipped=%d err=%v", added, updated, skipped, err)
	}
	items := s.ExportByIDs([]string{shortID(input.AccessToken)})
	if len(items) != 1 || items[0].AccessToken != input.AccessToken || items[0].RefreshToken != input.RefreshToken || items[0].IDToken != input.IDToken || items[0].Proxy != input.Proxy || items[0].AccountID != input.AccountID || items[0].Password != input.Password {
		t.Fatalf("exported=%+v", items)
	}
}

func TestImportCanReplaceCredentialForExplicitTargetID(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 3, encryptedTestCodec(t))
	first := events.ExportItem{AccountID: "upstream-account", AccessToken: "old-access", RefreshToken: "old-refresh"}
	if added, _, _, err := s.Import(nil, []events.ExportItem{first}, ""); err != nil || added != 1 {
		t.Fatalf("initial import added=%d err=%v", added, err)
	}
	items := s.List()
	if len(items) != 1 {
		t.Fatalf("items=%+v", items)
	}
	version := s.CatalogVersion()
	second := events.ExportItem{AccountID: "new-upstream-account", AccessToken: "new-access", RefreshToken: "new-refresh", TargetID: items[0].ID}
	if added, updated, _, err := s.Import(nil, []events.ExportItem{second}, ""); err != nil || added != 0 || updated != 1 {
		t.Fatalf("replacement added=%d updated=%d err=%v", added, updated, err)
	}
	if got := s.CatalogVersion(); got <= version {
		t.Fatalf("replacement did not invalidate catalog generation: before=%d after=%d", version, got)
	}
	if got := s.List(); len(got) != 1 {
		t.Fatalf("replacement created a duplicate: %+v", got)
	}
	exported := s.ExportByIDs([]string{items[0].ID})
	if len(exported) != 1 || exported[0].AccessToken != "new-access" || exported[0].RefreshToken != "new-refresh" || exported[0].AccountID != "new-upstream-account" {
		t.Fatalf("replacement export=%+v", exported)
	}
}

func TestImportRejectsPartialOAuthCredential(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 3, encryptedTestCodec(t))
	_, _, _, err := s.Import(nil, []events.ExportItem{{AccessToken: "access", IDToken: "id-without-refresh"}}, "")
	if err == nil || len(s.List()) != 0 {
		t.Fatalf("partial OAuth import err=%v items=%+v", err, s.List())
	}
}

func TestImportValidatesCompleteBatchBeforeMutation(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 3, encryptedTestCodec(t))
	_, _, _, err := s.Import(nil, []events.ExportItem{
		{AccessToken: "valid-access", RefreshToken: "valid-refresh"},
		{AccessToken: "invalid-access", RefreshToken: "invalid-refresh", Proxy: "file:///tmp/proxy"},
	}, "")
	if err == nil || len(s.List()) != 0 {
		t.Fatalf("batch validation err=%v items=%+v", err, s.List())
	}
}

func TestImportRejectsCodexCredentialType(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 1, encryptedTestCodec(t))
	_, _, _, err := s.Import(nil, []events.ExportItem{{CredentialType: "codex_cli", AccessToken: "access", RefreshToken: "refresh"}}, "")
	if err == nil {
		t.Fatal("expected cross-client credential import to fail")
	}
}

func TestRefreshProjectionRestoresLimitedAccountAndPreservesOperatorStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1, encryptedTestCodec(t))
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

	reloaded := New(path, 1, encryptedTestCodec(t))
	first := reloaded.items["token-a"]
	if first == nil || first.Status != StatusNormal || first.Quota != 4 || first.Type != "plus" || first.Email != "account@example.invalid" || first.Extra["restore_at"] != "2027-01-01T00:00:00Z" {
		t.Fatalf("refreshed account=%+v", first)
	}
	if second := reloaded.items["token-b"]; second == nil || second.Status != StatusDisabled {
		t.Fatalf("disabled account=%+v", second)
	}
}

func TestRefreshCandidatesForDoesNotTreatUnknownSelectionAsAllAccounts(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 1, encryptedTestCodec(t))
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
	accounts := New(path, 3, encryptedTestCodec(t))
	for _, item := range []struct{ token, source string }{{"token-web", "web"}, {"token-oauth", "oauth_login"}, {"token-import", "oauth_import"}, {"token-password", "password"}, {"token-other", "api"}} {
		if _, _, err := accounts.Add([]string{item.token}, item.source); err != nil {
			t.Fatal(err)
		}
	}
	candidates := accounts.RefreshCandidates()
	if len(candidates) != 4 {
		t.Fatalf("candidate count=%d candidates=%#v", len(candidates), candidates)
	}
	for _, item := range candidates {
		if item.SourceType == "api" {
			t.Fatalf("non-Web source was selected: %#v", item)
		}
	}
}

func TestManualRefreshCandidatesCanRetryAbnormalButNotDisabledAccounts(t *testing.T) {
	accounts := New(filepath.Join(t.TempDir(), "accounts.json"), 2, encryptedTestCodec(t))
	if _, _, err := accounts.Add([]string{"token-abnormal", "token-disabled"}, "oauth_import"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := accounts.Update("token-abnormal", "", StatusAbnormal, nil, ""); err != nil || !ok {
		t.Fatalf("mark abnormal: ok=%v err=%v", ok, err)
	}
	if _, ok, err := accounts.Update("token-disabled", "", StatusDisabled, nil, ""); err != nil || !ok {
		t.Fatalf("mark disabled: ok=%v err=%v", ok, err)
	}

	items := accounts.List()
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	got := accounts.RefreshCandidatesForIDs(ids)
	if len(got) != 1 || got[0].Status != StatusAbnormal {
		t.Fatalf("manual candidates=%#v", got)
	}
	if got := accounts.RefreshCandidates(); len(got) != 0 {
		t.Fatalf("scheduled candidates=%#v", got)
	}
}

func TestApplyRefreshedTokenCarriesImageSlotAndAliasesOldToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1, encryptedTestCodec(t))
	oldToken := testJWT(time.Now().Add(30*time.Minute), time.Now().Add(-time.Hour))
	newToken := testJWT(time.Now().Add(24*time.Hour), time.Now())
	if _, _, err := s.Add([]string{oldToken}, "web"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Update(oldToken, "plus", StatusNormal, intPointer(1), ""); err != nil || !ok {
		t.Fatalf("prepare account: ok=%v err=%v", ok, err)
	}
	if _, ok := s.AcquireImageToken("", "", nil, "", ""); !ok {
		t.Fatal("acquire image slot")
	}
	got, rotated, err := s.ApplyRefreshedToken(oldToken, newToken, "refresh-new", "id-new")
	if err != nil || !rotated || got != newToken {
		t.Fatalf("rotate got=%q rotated=%v err=%v", got, rotated, err)
	}
	s.ReleaseImageSlot(oldToken)
	if acquired, ok := s.AcquireImageToken("", "", nil, "", ""); !ok || acquired.AccessToken != newToken {
		t.Fatalf("old token alias did not release slot: account=%+v ok=%v", acquired, ok)
	}
	s.ReleaseImageSlot(newToken)
	reloaded := New(path, 1, encryptedTestCodec(t))
	account := reloaded.items[newToken]
	if account == nil || account.RefreshToken != "refresh-new" || account.Extra["id_token"] != "id-new" || account.Extra["last_token_refresh_at"] == nil {
		t.Fatalf("refreshed account=%+v", account)
	}
}

func TestTokenRefreshFailureProjectionIsSafeAndClearedOnSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1, encryptedTestCodec(t))
	if _, _, err := s.AddOAuth("access-old", "refresh-old", "id-old"); err != nil {
		t.Fatal(err)
	}
	// Simulate a legacy persisted raw error. The public view must never expose
	// it, and a new failure must erase it rather than extending its lifetime.
	s.items["access-old"].Extra["last_token_refresh_error"] = "proxy password=secret"
	if err := s.RecordTokenRefreshFailure("access-old", "transport"); err != nil {
		t.Fatal(err)
	}
	if _, leaked := s.items["access-old"].Extra["last_token_refresh_error"]; leaked {
		t.Fatal("refresh failure retained raw error text")
	}
	items := s.List()
	if len(items) != 1 {
		t.Fatalf("items=%d", len(items))
	}
	item := items[0]
	if item.LastTokenRefreshErrorClass != "transport" || item.LastTokenRefreshErrorAt == "" {
		t.Fatalf("failure projection=%+v", item)
	}
	if encoded, err := json.Marshal(item); err != nil || strings.Contains(string(encoded), "proxy password=secret") {
		t.Fatalf("unsafe failure projection json=%q err=%v", encoded, err)
	}
	if _, _, err := s.ApplyRefreshedToken("access-old", "access-new", "refresh-new", "id-new"); err != nil {
		t.Fatal(err)
	}
	item, ok := s.ViewForAccessToken("access-new")
	if !ok || item.LastTokenRefreshAt == "" || item.LastTokenRefreshErrorClass != "" || item.LastTokenRefreshErrorAt != "" {
		t.Fatalf("success did not clear failure projection=%+v ok=%v", item, ok)
	}
}

func TestTokenRefreshCandidatesPreferExpiringAndBoundKeepalive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1, encryptedTestCodec(t))
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
	s := New(path, 1, encryptedTestCodec(t))
	item, added, err := s.AddOAuth("access-oauth", "refresh-oauth", "id-oauth")
	if err != nil || !added || item.AccessToken != "access-oauth" {
		t.Fatalf("item=%#v added=%v err=%v", item, added, err)
	}
	item, added, err = s.AddOAuth("access-oauth", "refresh-new", "id-new")
	if err != nil || added {
		t.Fatalf("duplicate item=%#v added=%v err=%v", item, added, err)
	}
	reloaded := New(path, 1, encryptedTestCodec(t))
	account := reloaded.items["access-oauth"]
	if account == nil || account.RefreshToken != "refresh-new" || account.Extra["id_token"] != "id-new" || account.SourceType != "oauth_login" {
		t.Fatalf("account=%#v", account)
	}
}

func TestExportReturnsOnlyCompleteOAuthAccounts(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 1, encryptedTestCodec(t))
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
	s := New(filepath.Join(t.TempDir(), "accounts.json"), 1, encryptedTestCodec(t))
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

func TestAcquireTextAccountAndRecordTextResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1, encryptedTestCodec(t))
	if _, _, err := s.Add([]string{"token-a"}, "web"); err != nil {
		t.Fatal(err)
	}
	quota := 3
	account, found, err := s.Update("token-a", "plus", StatusNormal, &quota, "")
	if err != nil || !found {
		t.Fatalf("update found=%v err=%v", found, err)
	}
	// Text acquire must not consume image slots.
	acquired, ok := s.AcquireTextAccount(account.ID, "", "")
	if !ok || acquired.AccessToken != "token-a" {
		t.Fatalf("acquire text account=%+v ok=%v", acquired, ok)
	}
	if _, ok := s.AcquireImageToken("", "", nil, "", ""); !ok {
		t.Fatal("text acquisition must not occupy image inflight slot")
	}
	s.ReleaseImageSlot("token-a")

	result, marked := s.RecordTextResult(account.ID, "gpt-test", true, "")
	if !marked || result.Success != 1 || result.Fail != 0 || result.Status != StatusNormal {
		t.Fatalf("success result=%+v marked=%v", result, marked)
	}
	result, marked = s.RecordTextResult(account.ID, "gpt-test", false, "timeout")
	if !marked || result.Fail != 1 || result.Status != StatusNormal {
		t.Fatalf("timeout must only count fail: %+v", result)
	}
	result, marked = s.RecordTextResult(account.ID, "gpt-test", false, "invalid_token")
	if !marked || result.Status != StatusAbnormal {
		t.Fatalf("invalid_token must mark abnormal: %+v", result)
	}
	if _, ok := s.AcquireTextAccount(account.ID, "", ""); ok {
		t.Fatal("abnormal account must not be reacquired")
	}
}

func TestTextCooldownIsModelScopedAndExpires(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1, encryptedTestCodec(t))
	if _, _, err := s.Add([]string{"token-a"}, "web"); err != nil {
		t.Fatal(err)
	}
	quota := 3
	account, found, err := s.Update("token-a", "plus", StatusNormal, &quota, "")
	if err != nil || !found {
		t.Fatalf("update found=%v err=%v", found, err)
	}
	if _, ok, err := s.PutModelSnapshot(account.ID, events.AccountModelSnapshot{AccountID: account.ID, Models: []events.AccountModelEntry{
		{ID: "gpt-5", Capabilities: []string{events.ModelCapabilityTextGeneration}},
		{ID: "gpt-4.1", Capabilities: []string{events.ModelCapabilityTextGeneration}},
	}}); err != nil || !ok {
		t.Fatalf("put model snapshot ok=%v err=%v", ok, err)
	}
	if _, ok := s.RecordTextResult(account.ID, "gpt-5", false, "rate_limit"); !ok {
		t.Fatal("record rate-limit result")
	}
	items := s.List()
	if len(items) != 1 || len(items[0].TextCooldowns) != 1 {
		t.Fatalf("active cooldown view=%+v", items)
	}
	cooldown := items[0].TextCooldowns[0]
	if cooldown.Model != "gpt-5" || cooldown.ErrorClass != "rate_limit" || cooldown.Until == "" {
		t.Fatalf("cooldown=%+v", cooldown)
	}
	if _, ok := s.AcquireTextToken(nil, "gpt-5", ""); ok {
		t.Fatal("rate-limited model must be in cooldown")
	}
	if acquired, ok := s.AcquireTextToken(nil, "gpt-4.1", ""); !ok || acquired.ID != account.ID {
		t.Fatalf("unaffected model acquire=%+v ok=%v", acquired, ok)
	}
	cooldowns, ok := s.items["token-a"].Extra[textCooldownExtraKey].(map[string]any)
	if !ok {
		t.Fatal("text cooldown was not persisted in account extra")
	}
	cooldowns[textCooldownKey("gpt-5")].(map[string]any)["until"] = time.Now().UTC().Add(-time.Second).Format(time.RFC3339)
	if items := s.List(); len(items) != 1 || len(items[0].TextCooldowns) != 0 {
		t.Fatalf("expired cooldown must not be projected: %+v", items)
	}
	if acquired, ok := s.AcquireTextToken(nil, "gpt-5", ""); !ok || acquired.ID != account.ID {
		t.Fatalf("expired cooldown acquire=%+v ok=%v", acquired, ok)
	}
	if _, ok := s.RecordTextResult(account.ID, "gpt-5", false, "timeout"); !ok {
		t.Fatal("record timeout result")
	}
	if _, ok := s.RecordTextResult(account.ID, "gpt-5", true, ""); !ok {
		t.Fatal("record success result")
	}
	if _, exists := s.items["token-a"].Extra[textCooldownExtraKey]; exists {
		t.Fatal("successful text result must clear its model cooldown")
	}
}

func TestImageCooldownIsModelScopedAndClearsOnSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := New(path, 1, encryptedTestCodec(t))
	if _, _, err := s.Add([]string{"token-a"}, "web"); err != nil {
		t.Fatal(err)
	}
	quota := 3
	account, found, err := s.Update("token-a", "plus", StatusNormal, &quota, "")
	if err != nil || !found {
		t.Fatalf("update found=%v err=%v", found, err)
	}
	if _, ok, err := s.PutModelSnapshot(account.ID, events.AccountModelSnapshot{AccountID: account.ID, Models: []events.AccountModelEntry{
		{ID: "gpt-image-2", Capabilities: []string{events.ModelCapabilityImageGeneration}},
		{ID: "gpt-image-1", Capabilities: []string{events.ModelCapabilityImageGeneration}},
	}}); err != nil || !ok {
		t.Fatalf("put model snapshot ok=%v err=%v", ok, err)
	}
	if _, ok := s.MarkImageResult("token-a", "gpt-image-2", false, "rate_limit"); !ok {
		t.Fatal("record image rate limit")
	}
	items := s.List()
	if len(items) != 1 || len(items[0].ImageCooldowns) != 1 {
		t.Fatalf("image cooldown view=%+v", items)
	}
	cooldown := items[0].ImageCooldowns[0]
	if cooldown.Model != "gpt-image-2" || cooldown.ErrorClass != "rate_limit" || cooldown.Until == "" {
		t.Fatalf("image cooldown=%+v", cooldown)
	}
	if _, ok := s.AcquireImageToken("", "", nil, "gpt-image-2", events.ModelCapabilityImageGeneration); ok {
		t.Fatal("rate-limited image model must be in cooldown")
	}
	if acquired, ok := s.AcquireImageToken("", "", nil, "gpt-image-1", events.ModelCapabilityImageGeneration); !ok || acquired.ID != account.ID {
		t.Fatalf("unaffected image model acquire=%+v ok=%v", acquired, ok)
	} else {
		s.ReleaseImageSlot(acquired.AccessToken)
	}
	if _, ok := s.MarkImageResult("token-a", "gpt-image-2", true, ""); !ok {
		t.Fatal("record image success")
	}
	if items := s.List(); len(items) != 1 || len(items[0].ImageCooldowns) != 0 {
		t.Fatalf("success must clear image cooldown: %+v", items)
	}
}
