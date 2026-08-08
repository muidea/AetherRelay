package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccountPoolBundleImportDispatchesBothSlots(t *testing.T) {
	web := &chatGPTAccountRuntimeStub{}
	codex := &codexAccountRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithChatGPTRuntime(web).WithCodexRuntime(codex)
	body := `{"format":"ai-proxy.account-pool-bundle","schema_version":2,"accounts":[{"account_ref":"acct_01","identity":{"email":"USER@example.com"},"slots":{"chatgpt_web":{"access_token":"web-access","refresh_token":"web-refresh"},"codex_cli":{"access_token":"codex-access","refresh_token":"codex-refresh"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AI-Proxy-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(web.addedAccounts) != 1 || web.addedAccounts[0].Email != "USER@example.com" || len(codex.imported) != 1 || codex.imported[0].Email != "USER@example.com" {
		t.Fatalf("web imports=%+v codex imports=%+v", web.addedAccounts, codex.imported)
	}
}

// Ensure the bundle endpoint remains an Admin mutation even when runtimes are
// present but the request has no mutation identity.
func TestAccountPoolBundleImportRequiresRuntimes(t *testing.T) {
	h := NewHandler("", &testRuntime{})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/account-pool-bundle/import", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AI-Proxy-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
