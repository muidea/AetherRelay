package admin

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	codexevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

func TestResolveBundleTargetEmailOnlyUsesEmailFallback(t *testing.T) {
	target, reason := resolveBundleTarget("", "user@example.com", []bundleTargetView{{ID: "local-1", IdentityKey: "acct_upstream", Email: "USER@example.com"}}, false)
	if reason != "" || target != "local-1" {
		t.Fatalf("email-only target=%q reason=%q", target, reason)
	}
}

func TestResolveBundleTargetPreservesDifferentAccountIDUnlessReplace(t *testing.T) {
	views := []bundleTargetView{{ID: "local-1", IdentityKey: "acct_existing", Email: "user@example.com"}}
	if target, reason := resolveBundleTarget("other-upstream", "user@example.com", views, false); target != "" || reason != "" {
		t.Fatalf("expected a new workspace without replace, target=%q reason=%q", target, reason)
	}
	if target, reason := resolveBundleTarget("other-upstream", "user@example.com", views, true); target != "local-1" || reason != "" {
		t.Fatalf("expected replacement target=%q reason=%q", target, reason)
	}
}

func TestResolveBundleTargetDoesNotGuessAmongSharedEmailWorkspaces(t *testing.T) {
	views := []bundleTargetView{
		{ID: "local-1", IdentityKey: "acct_existing_1", Email: "user@example.com"},
		{ID: "local-2", IdentityKey: "acct_existing_2", Email: "USER@example.com"},
	}
	if target, reason := resolveBundleTarget("new-upstream", "user@example.com", views, false); target != "" || reason != "" {
		t.Fatalf("expected a new workspace without replace, target=%q reason=%q", target, reason)
	}
	if target, reason := resolveBundleTarget("new-upstream", "user@example.com", views, true); target != "" || reason != "multiple existing slots match email" {
		t.Fatalf("expected ambiguous replacement conflict, target=%q reason=%q", target, reason)
	}
}

func TestResolveBundleTargetsRejectsMultipleBundleAccountsForOneSlot(t *testing.T) {
	payload := accountPoolBundle{Replace: true, Accounts: []accountPoolBundleEntry{
		{AccountRef: "acct-a", Slots: accountPoolBundleSlots{ChatGPT: &accountPoolBundleChatGPT{AccountID: "upstream-a", Email: "user@example.com", AccessToken: "access-a"}}},
		{AccountRef: "acct-b", Slots: accountPoolBundleSlots{ChatGPT: &accountPoolBundleChatGPT{AccountID: "upstream-b", Email: "user@example.com", AccessToken: "access-b"}}},
	}}
	chat := []accevents.ExportItem{{AccountID: "upstream-a", Email: "user@example.com", AccessToken: "access-a"}, {AccountID: "upstream-b", Email: "user@example.com", AccessToken: "access-b"}}
	conflicts := resolveAccountPoolBundleTargets(payload, chat, nil, []accevents.AccountView{{ID: "local-1", IdentityKey: "acct-existing", Email: "user@example.com"}}, nil)
	if len(conflicts) != 1 || conflicts[0].AccountRef != "acct-b" {
		t.Fatalf("expected one target collision, conflicts=%+v", conflicts)
	}
}

func TestAccountPoolBundleExportGroupsByCredentialEmailWhenListEmailMissing(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{
		accounts:      []accevents.AccountView{{ID: "web-1"}},
		exportedItems: []accevents.ExportItem{{AccountID: "web-upstream", Email: "user@example.com", AccessToken: "web-access", RefreshToken: "web-refresh"}},
	}
	codex := &codexAccountRuntimeStub{
		accounts: []codexevents.AccountView{{ID: "codex-1", Email: "user@example.com"}},
		exported: []codexevents.CredentialInput{{AccountID: "codex-upstream", Email: "user@example.com", AccessToken: "codex-access", RefreshToken: "codex-refresh", FingerprintMode: codexevents.FingerprintModeSession}},
	}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web).WithCodexRuntime(codex)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/export", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload accountPoolBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(payload.Accounts) != 1 || payload.Accounts[0].Slots.ChatGPT == nil || payload.Accounts[0].Slots.Codex == nil {
		t.Fatalf("export did not merge slots by effective email: %+v", payload.Accounts)
	}
	if payload.Accounts[0].Identity.Email != "user@example.com" {
		t.Fatalf("export identity email=%q", payload.Accounts[0].Identity.Email)
	}
	if payload.Accounts[0].Slots.ChatGPT.IdentityKey == "" || payload.Accounts[0].Slots.Codex.IdentityKey == "" {
		t.Fatalf("export did not retain fallback identity keys: %+v", payload.Accounts[0].Slots)
	}
	if payload.Accounts[0].Slots.Codex.FingerprintMode != codexevents.FingerprintModeSession {
		t.Fatalf("export lost fingerprint mode: %+v", payload.Accounts[0].Slots.Codex)
	}
	exportedAt, err := time.Parse(time.RFC3339, payload.ExportedAt)
	if err != nil {
		t.Fatalf("exported_at=%q: %v", payload.ExportedAt, err)
	}
	mediaType, params, err := mime.ParseMediaType(rec.Header().Get("Content-Disposition"))
	if err != nil {
		t.Fatalf("parse Content-Disposition: %v", err)
	}
	wantFilename := bundleExportFilename(bundleExportArtifactAccountPool, accountPoolBundleSchemaVersion, bundleExportProfileComplete, exportedAt)
	if mediaType != "attachment" || params["filename"] != wantFilename {
		t.Fatalf("Content-Disposition=%q, want attachment filename %q", rec.Header().Get("Content-Disposition"), wantFilename)
	}
}

func TestAccountPoolBundleExportPreservesAmbiguousSameEmailSlots(t *testing.T) {
	const email = "shared@example.com"
	web := &chatGPTAccountRuntimeStub{
		accounts: []accevents.AccountView{
			{ID: "web-1", IdentityKey: "identity-web-1", Email: email},
			{ID: "web-2", IdentityKey: "identity-web-2", Email: email},
		},
		exportedByID: map[string]accevents.ExportItem{
			"web-1": {AccountID: "upstream-web-1", Email: email, AccessToken: "web-access-1", RefreshToken: "web-refresh-1"},
			"web-2": {AccountID: "upstream-web-2", Email: email, AccessToken: "web-access-2", RefreshToken: "web-refresh-2"},
		},
	}
	codex := &codexAccountRuntimeStub{
		accounts: []codexevents.AccountView{{ID: "codex-1", IdentityKey: "identity-codex-1", Email: email}},
		exportedByID: map[string]codexevents.CredentialInput{
			"codex-1": {AccountID: "upstream-codex-1", Email: email, AccessToken: "codex-access-1", RefreshToken: "codex-refresh-1"},
		},
	}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web).WithCodexRuntime(codex)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/export", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload accountPoolBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	webSlots, codexSlots, dualSlots := 0, 0, 0
	for _, account := range payload.Accounts {
		if account.Slots.ChatGPT != nil {
			webSlots++
		}
		if account.Slots.Codex != nil {
			codexSlots++
		}
		if account.Slots.ChatGPT != nil && account.Slots.Codex != nil {
			dualSlots++
		}
	}
	if len(payload.Accounts) != 3 || webSlots != 2 || codexSlots != 1 || dualSlots != 0 {
		t.Fatalf("ambiguous email cohort was merged or dropped: %+v", payload.Accounts)
	}
}

func TestGroupAccountPoolBundleExportSlotsPairsUniqueIdentityBeforeEmail(t *testing.T) {
	const email = "shared@example.com"
	accounts := groupAccountPoolBundleExportSlots([]accountPoolBundleExportSlot{
		{position: 0, email: email, identity: "identity-1", chatGPT: &accountPoolBundleChatGPT{AccessToken: "web-1"}},
		{position: 1, email: email, identity: "identity-2", chatGPT: &accountPoolBundleChatGPT{AccessToken: "web-2"}},
		{position: 2, email: email, identity: "identity-2", codex: &accountPoolBundleCodex{AccessToken: "codex-2"}},
		{position: 3, email: email, identity: "identity-1", codex: &accountPoolBundleCodex{AccessToken: "codex-1"}},
	})
	if len(accounts) != 2 {
		t.Fatalf("accounts=%+v", accounts)
	}
	for _, account := range accounts {
		if account.Slots.ChatGPT == nil || account.Slots.Codex == nil || account.Slots.ChatGPT.AccessToken[len("web-"):] != account.Slots.Codex.AccessToken[len("codex-"):] {
			t.Fatalf("identity pairing mismatch: %+v", account)
		}
	}
}

func TestGroupAccountPoolBundleExportSlotsDoesNotPairIdentityWithoutEmail(t *testing.T) {
	accounts := groupAccountPoolBundleExportSlots([]accountPoolBundleExportSlot{
		{position: 0, identity: "identity-1", chatGPT: &accountPoolBundleChatGPT{AccessToken: "web-1"}},
		{position: 1, identity: "identity-1", codex: &accountPoolBundleCodex{AccessToken: "codex-1"}},
	})
	if len(accounts) != 2 {
		t.Fatalf("email-less slots were paired into an invalid dual-slot account: %+v", accounts)
	}
	for _, account := range accounts {
		if (account.Slots.ChatGPT == nil) == (account.Slots.Codex == nil) {
			t.Fatalf("expected exactly one slot per account: %+v", account)
		}
	}
}

func TestAccountPoolBundleImportDispatchesBothSlots(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{}
	codex := &codexAccountRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web).WithCodexRuntime(codex)
	body := `{"format":"aetherrelay.account-pool-bundle","schema_version":2,"accounts":[{"account_ref":"acct_01","identity":{"email":"USER@example.com"},"slots":{"chatgpt_web":{"access_token":"web-access","refresh_token":"web-refresh"},"codex_cli":{"access_token":"codex-access","refresh_token":"codex-refresh","fingerprint_mode":" SESSION "}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(web.addedAccounts) != 1 || web.addedAccounts[0].Email != "USER@example.com" || len(codex.imported) != 1 || codex.imported[0].Email != "USER@example.com" || codex.imported[0].FingerprintMode != codexevents.FingerprintModeSession {
		t.Fatalf("web imports=%+v codex imports=%+v", web.addedAccounts, codex.imported)
	}
}

func TestAccountPoolBundleImportDoesNotReflectCredentialInStoreFailure(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{addErr: errors.New("cannot import credential web-secret")}
	codex := &codexAccountRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web).WithCodexRuntime(codex)
	body := `{"format":"aetherrelay.account-pool-bundle","schema_version":2,"accounts":[{"account_ref":"acct_01","identity":{"email":"user@example.com"},"slots":{"chatgpt_web":{"access_token":"web-secret","refresh_token":"web-refresh"},"codex_cli":{"access_token":"codex-access","refresh_token":"codex-refresh"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), "ChatGPT Web account import failed") || strings.Contains(rec.Body.String(), "web-secret") {
		t.Fatalf("unsafe failure response status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAccountPoolBundleImportValidatesAllSlotsBeforeWriting(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{}
	codex := &codexAccountRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web).WithCodexRuntime(codex)
	body := `{"format":"aetherrelay.account-pool-bundle","schema_version":2,"accounts":[{"account_ref":"acct_01","slots":{"chatgpt_web":{"access_token":"web-secret","refresh_token":"web-refresh"},"codex_cli":{"credential_type":"wrong","access_token":"codex-secret","refresh_token":"codex-refresh"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(web.addedAccounts) != 0 || len(codex.imported) != 0 {
		t.Fatalf("invalid bundle was partially imported: web=%+v codex=%+v", web.addedAccounts, codex.imported)
	}
	if strings.Contains(rec.Body.String(), "web-secret") || strings.Contains(rec.Body.String(), "codex-secret") {
		t.Fatalf("error response leaked a credential: %s", rec.Body.String())
	}
}

func TestAccountPoolBundleImportValidatesProxyBeforeWriting(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{}
	codex := &codexAccountRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web).WithCodexRuntime(codex)
	body := `{"format":"aetherrelay.account-pool-bundle","schema_version":2,"accounts":[{"account_ref":"acct_01","slots":{"chatgpt_web":{"access_token":"web-access","refresh_token":"web-refresh"},"codex_cli":{"access_token":"codex-access","refresh_token":"codex-refresh","proxy":"file:///tmp/proxy"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(web.addedAccounts) != 0 || len(codex.imported) != 0 {
		t.Fatalf("invalid proxy was partially imported: web=%+v codex=%+v", web.addedAccounts, codex.imported)
	}
}

func TestAccountPoolBundleImportValidatesFingerprintModeBeforeWriting(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{}
	codex := &codexAccountRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web).WithCodexRuntime(codex)
	body := `{"format":"aetherrelay.account-pool-bundle","schema_version":2,"accounts":[{"account_ref":"acct_01","slots":{"chatgpt_web":{"access_token":"web-access","refresh_token":"web-refresh"},"codex_cli":{"access_token":"codex-access","refresh_token":"codex-refresh","fingerprint_mode":"automatic"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || len(web.addedAccounts) != 0 || len(codex.imported) != 0 {
		t.Fatalf("invalid fingerprint mode was partially imported: status=%d body=%s web=%+v codex=%+v", rec.Code, rec.Body.String(), web.addedAccounts, codex.imported)
	}
}

func TestAccountPoolBundleImportReturnsFileConflictsWithoutWriting(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{}
	codex := &codexAccountRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web).WithCodexRuntime(codex)
	body := `{"format":"aetherrelay.account-pool-bundle","schema_version":2,"accounts":[{"account_ref":"acct_duplicate","identity":{"email":"same@example.com"},"slots":{"chatgpt_web":{"access_token":"web-a","refresh_token":"refresh-a"}}},{"account_ref":"acct_duplicate","identity":{"email":"same@example.com"},"slots":{"codex_cli":{"access_token":"codex-a","refresh_token":"refresh-b"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result accountPoolBundleImportResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if len(result.Conflicts) == 0 || len(web.addedAccounts) != 0 || len(codex.imported) != 0 {
		t.Fatalf("conflict was not reported before writing: result=%+v web=%+v codex=%+v", result, web.addedAccounts, codex.imported)
	}
}

func TestAccountPoolBundleImportLastSameAccountIDWins(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{}
	codex := &codexAccountRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web).WithCodexRuntime(codex)
	body := `{"format":"aetherrelay.account-pool-bundle","schema_version":2,"accounts":[{"account_ref":"acct-web-a","slots":{"chatgpt_web":{"account_id":"same-web","access_token":"superseded-web-without-refresh"}}},{"account_ref":"acct-web-b","slots":{"chatgpt_web":{"account_id":"same-web","access_token":"web-b","refresh_token":"web-refresh-b"}}},{"account_ref":"acct-codex-a","slots":{"codex_cli":{"account_id":"same-codex","access_token":"superseded-codex","fingerprint_mode":"automatic"}}},{"account_ref":"acct-codex-b","slots":{"codex_cli":{"account_id":"same-codex","access_token":"codex-b","refresh_token":"codex-refresh-b","fingerprint_mode":"session"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("same-name overwrite status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(web.addedAccounts) != 1 || web.addedAccounts[0].AccessToken != "web-b" || web.addedAccounts[0].RefreshToken != "web-refresh-b" {
		t.Fatalf("ChatGPT same-name winner=%+v", web.addedAccounts)
	}
	if len(codex.imported) != 1 || codex.imported[0].AccessToken != "codex-b" || codex.imported[0].RefreshToken != "codex-refresh-b" || codex.imported[0].FingerprintMode != codexevents.FingerprintModeSession {
		t.Fatalf("Codex same-name winner=%+v", codex.imported)
	}
}

func TestAccountPoolBundleImportStillRejectsCredentialReuseAcrossAccountIDs(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web)
	body := `{"format":"aetherrelay.account-pool-bundle","schema_version":2,"accounts":[{"account_ref":"acct-a","slots":{"chatgpt_web":{"account_id":"upstream-a","access_token":"shared-access","refresh_token":"refresh-a"}}},{"account_ref":"acct-b","slots":{"chatgpt_web":{"account_id":"upstream-b","access_token":"shared-access","refresh_token":"refresh-b"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || len(web.addedAccounts) != 0 {
		t.Fatalf("credential reuse status=%d body=%s imports=%+v", rec.Code, rec.Body.String(), web.addedAccounts)
	}
}

func TestAccountPoolBundleImportAllowsDistinctSlotsWithSharedEmail(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{}
	codex := &codexAccountRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web).WithCodexRuntime(codex)
	body := `{"format":"aetherrelay.account-pool-bundle","schema_version":2,"accounts":[{"account_ref":"acct_web","identity":{"email":"same@example.com"},"slots":{"chatgpt_web":{"account_id":"web-account","access_token":"web-access","refresh_token":"web-refresh"}}},{"account_ref":"acct_codex","identity":{"email":"same@example.com"},"slots":{"codex_cli":{"account_id":"codex-account","access_token":"codex-access","refresh_token":"codex-refresh"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || len(web.addedAccounts) != 1 || len(codex.imported) != 1 {
		t.Fatalf("shared-email slots were not preserved status=%d body=%s web=%+v codex=%+v", rec.Code, rec.Body.String(), web.addedAccounts, codex.imported)
	}
}

func TestAccountPoolBundleImportRejectsDualSlotWithoutEmail(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{}
	codex := &codexAccountRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web).WithCodexRuntime(codex)
	body := `{"format":"aetherrelay.account-pool-bundle","schema_version":2,"accounts":[{"account_ref":"acct_no_email","slots":{"chatgpt_web":{"access_token":"web-access","refresh_token":"web-refresh"},"codex_cli":{"access_token":"codex-access","refresh_token":"codex-refresh"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || len(web.addedAccounts) != 0 || len(codex.imported) != 0 {
		t.Fatalf("expected dual-slot email conflict status=%d body=%s web=%+v codex=%+v", rec.Code, rec.Body.String(), web.addedAccounts, codex.imported)
	}
}

func TestAccountPoolBundleImportNormalizesCredentialFields(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{}
	codex := &codexAccountRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web).WithCodexRuntime(codex)
	body := `{"format":"aetherrelay.account-pool-bundle","schema_version":2,"accounts":[{"account_ref":" acct_01 ","identity":{"email":" USER@example.com "},"slots":{"chatgpt_web":{"credential_type":" chatgpt_web ","access_token":" web-access ","refresh_token":" web-refresh "},"codex_cli":{"credential_type":" codex_oauth ","access_token":" codex-access ","refresh_token":" codex-refresh "}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(web.addedAccounts) != 1 || web.addedAccounts[0].Email != "USER@example.com" || web.addedAccounts[0].AccessToken != "web-access" || len(codex.imported) != 1 || codex.imported[0].Email != "USER@example.com" || codex.imported[0].AccessToken != "codex-access" {
		t.Fatalf("fields were not normalized: web=%+v codex=%+v", web.addedAccounts, codex.imported)
	}
}

func TestAccountPoolBundleImportPreservesDifferentAccountIDUnlessReplace(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{}
	codex := &codexAccountRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web).WithCodexRuntime(codex)
	body := `{"format":"aetherrelay.account-pool-bundle","schema_version":2,"accounts":[{"account_ref":"acct_01","identity":{"email":"operator@example.invalid"},"slots":{"chatgpt_web":{"account_id":"different-upstream","access_token":"new-access","refresh_token":"new-refresh"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || len(web.addedAccounts) != 1 || web.addedAccounts[0].TargetID != "" {
		t.Fatalf("distinct workspace import status=%d body=%s imports=%+v", rec.Code, rec.Body.String(), web.addedAccounts)
	}

	body = `{"format":"aetherrelay.account-pool-bundle","schema_version":2,"replace":true,"accounts":[{"account_ref":"acct_01","identity":{"email":"operator@example.invalid"},"slots":{"chatgpt_web":{"account_id":"different-upstream","access_token":"new-access","refresh_token":"new-refresh"}}}]}`
	req = httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || len(web.addedAccounts) != 1 || web.addedAccounts[0].TargetID != "account-1" {
		t.Fatalf("replace import status=%d body=%s imports=%+v", rec.Code, rec.Body.String(), web.addedAccounts)
	}
}

// Ensure the bundle endpoint remains an Admin mutation even when runtimes are
// present but the request has no mutation identity.
func TestAccountPoolBundleImportRequiresRuntimes(t *testing.T) {
	h := NewHandler("", &testRuntime{})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAccountPoolBundleImportSingleSlotDoesNotRequireOtherStore(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web)
	body := `{"format":"aetherrelay.account-pool-bundle","schema_version":2,"accounts":[{"account_ref":"acct_web","slots":{"chatgpt_web":{"access_token":"web-access","refresh_token":"web-refresh"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || len(web.addedAccounts) != 1 {
		t.Fatalf("single-slot import status=%d body=%s imports=%+v", rec.Code, rec.Body.String(), web.addedAccounts)
	}
}
