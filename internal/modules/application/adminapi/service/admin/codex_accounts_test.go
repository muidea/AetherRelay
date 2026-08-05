package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	admincodex "ai-proxy/internal/modules/application/adminapi/pkg/codexmanagement"
	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	codexevents "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/events"
)

type codexAccountRuntimeStub struct {
	started      [][]string
	usageStarted [][]string
	exportedIDs  []string
	exported     []codexevents.CredentialInput
}

func (s *codexAccountRuntimeStub) ListCodexAccounts(context.Context) ([]codexevents.AccountView, error) {
	return nil, nil
}
func (s *codexAccountRuntimeStub) ImportCodexAccounts(context.Context, []codexevents.CredentialInput) (admincodex.ImportResult, error) {
	return admincodex.ImportResult{Added: 1, ModelDiscovery: &proxyevents.CodexDiscoveryProgress{ProgressID: "discovery-1", StartedAt: "2026-08-03T12:00:00Z"}, UsageRefresh: &proxyevents.CodexUsageProgress{ProgressID: "usage-1", StartedAt: "2026-08-03T12:00:00Z"}}, nil
}
func (s *codexAccountRuntimeStub) DeleteCodexAccounts(context.Context, []string) (codexevents.DeleteResult, error) {
	return codexevents.DeleteResult{}, nil
}
func (s *codexAccountRuntimeStub) UpdateCodexAccount(context.Context, codexevents.UpdateCommand) (codexevents.UpdateResult, error) {
	return codexevents.UpdateResult{}, nil
}
func (s *codexAccountRuntimeStub) RefreshCodexAccounts(context.Context, []string) (admincodex.RefreshResult, error) {
	return admincodex.RefreshResult{}, nil
}
func (s *codexAccountRuntimeStub) ExportCodexAccounts(_ context.Context, ids []string) (codexevents.ExportByIDResult, error) {
	s.exportedIDs = append([]string(nil), ids...)
	return codexevents.ExportByIDResult{Items: append([]codexevents.CredentialInput(nil), s.exported...)}, nil
}
func (s *codexAccountRuntimeStub) StartCodexOAuth(context.Context, string, string) (codexevents.OAuthStartResult, error) {
	return codexevents.OAuthStartResult{}, nil
}

func TestCodexAccountExportIsNoStoreAndImportable(t *testing.T) {
	runtime := &codexAccountRuntimeStub{exported: []codexevents.CredentialInput{{AccessToken: "access-secret", RefreshToken: "refresh-secret", IDToken: "id-secret", AccountID: "account-header", Proxy: "http://127.0.0.1:8080"}}}
	handler := NewHandler("", &testRuntime{}).WithCodexRuntime(runtime)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/codex/accounts/export", strings.NewReader(`{"ids":["account-1"]}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AI-Proxy-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Header().Get("Content-Disposition"), "codex-oauth-accounts.json") {
		t.Fatalf("export status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	var items []codexevents.CredentialInput
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil || len(items) != 1 || items[0].RefreshToken != "refresh-secret" || !reflect.DeepEqual(runtime.exportedIDs, []string{"account-1"}) {
		t.Fatalf("export body=%s ids=%v items=%+v err=%v", rec.Body.String(), runtime.exportedIDs, items, err)
	}
}
func (s *codexAccountRuntimeStub) FinishCodexOAuth(context.Context, string, string) (admincodex.OAuthFinishResult, error) {
	return admincodex.OAuthFinishResult{}, nil
}
func (s *codexAccountRuntimeStub) StartCodexModelDiscovery(_ context.Context, accountIDs []string) (proxyevents.CodexDiscoveryProgress, error) {
	s.started = append(s.started, append([]string(nil), accountIDs...))
	return proxyevents.CodexDiscoveryProgress{ProgressID: "discovery-1", StartedAt: "2026-08-03T12:00:00Z"}, nil
}
func (s *codexAccountRuntimeStub) CodexModelDiscoveryProgress(_ context.Context, progressID string) (proxyevents.CodexDiscoveryProgress, error) {
	return proxyevents.CodexDiscoveryProgress{ProgressID: progressID, Total: 1, Processed: 1, Succeeded: 1, Done: true, StartedAt: "2026-08-03T12:00:00Z", CompletedAt: "2026-08-03T12:00:05Z"}, nil
}
func (s *codexAccountRuntimeStub) StartCodexUsageRefresh(_ context.Context, accountIDs []string) (proxyevents.CodexUsageProgress, error) {
	s.usageStarted = append(s.usageStarted, append([]string(nil), accountIDs...))
	return proxyevents.CodexUsageProgress{ProgressID: "usage-1", Total: len(accountIDs), StartedAt: "2026-08-03T12:00:00Z"}, nil
}
func (s *codexAccountRuntimeStub) CodexUsageRefreshProgress(_ context.Context, progressID string) (proxyevents.CodexUsageProgress, error) {
	return proxyevents.CodexUsageProgress{ProgressID: progressID, Total: 1, Processed: 1, Succeeded: 1, Done: true, StartedAt: "2026-08-03T12:00:00Z", CompletedAt: "2026-08-03T12:00:05Z"}, nil
}

func TestCodexImportStartsDiscoveryAndExposesProgress(t *testing.T) {
	runtime := &codexAccountRuntimeStub{}
	handler := NewHandler("", &testRuntime{}).WithCodexRuntime(runtime)
	importRequest := httptest.NewRequest(http.MethodPost, "/admin/api/codex/accounts", strings.NewReader(`{"accounts":[{"access_token":"access","refresh_token":"refresh"}]}`))
	importRequest.RemoteAddr = "127.0.0.1:1234"
	importRequest.Header.Set("X-AI-Proxy-Admin", "1")
	importRecorder := httptest.NewRecorder()
	handler.ServeHTTP(importRecorder, importRequest)
	if importRecorder.Code != http.StatusCreated || len(runtime.started) != 0 {
		t.Fatalf("import status=%d started=%v body=%s", importRecorder.Code, runtime.started, importRecorder.Body.String())
	}
	var imported admincodex.ImportResult
	if err := json.Unmarshal(importRecorder.Body.Bytes(), &imported); err != nil || imported.ModelDiscovery == nil || imported.ModelDiscovery.ProgressID != "discovery-1" || imported.UsageRefresh == nil || imported.UsageRefresh.ProgressID != "usage-1" {
		t.Fatalf("import response=%s err=%v value=%+v", importRecorder.Body.String(), err, imported)
	}

	startRequest := httptest.NewRequest(http.MethodPost, "/admin/api/codex/accounts/discovery", strings.NewReader(`{"account_ids":["account-1"]}`))
	startRequest.RemoteAddr = "127.0.0.1:1234"
	startRequest.Header.Set("X-AI-Proxy-Admin", "1")
	startRecorder := httptest.NewRecorder()
	handler.ServeHTTP(startRecorder, startRequest)
	if startRecorder.Code != http.StatusAccepted || !reflect.DeepEqual(runtime.started, [][]string{[]string{"account-1"}}) {
		t.Fatalf("start status=%d started=%v body=%s", startRecorder.Code, runtime.started, startRecorder.Body.String())
	}

	progressRequest := httptest.NewRequest(http.MethodGet, "/admin/api/codex/accounts/discovery/progress/discovery-1", nil)
	progressRequest.RemoteAddr = "127.0.0.1:1234"
	progressRecorder := httptest.NewRecorder()
	handler.ServeHTTP(progressRecorder, progressRequest)
	if progressRecorder.Code != http.StatusOK || !strings.Contains(progressRecorder.Body.String(), `"done":true`) {
		t.Fatalf("progress status=%d body=%s", progressRecorder.Code, progressRecorder.Body.String())
	}

	usageRequest := httptest.NewRequest(http.MethodPost, "/admin/api/codex/accounts/usage", strings.NewReader(`{"account_ids":["account-1"]}`))
	usageRequest.RemoteAddr = "127.0.0.1:1234"
	usageRequest.Header.Set("X-AI-Proxy-Admin", "1")
	usageRecorder := httptest.NewRecorder()
	handler.ServeHTTP(usageRecorder, usageRequest)
	if usageRecorder.Code != http.StatusAccepted || !reflect.DeepEqual(runtime.usageStarted, [][]string{{"account-1"}}) {
		t.Fatalf("usage start status=%d started=%v body=%s", usageRecorder.Code, runtime.usageStarted, usageRecorder.Body.String())
	}

	usageProgressRequest := httptest.NewRequest(http.MethodGet, "/admin/api/codex/accounts/usage/progress/usage-1", nil)
	usageProgressRequest.RemoteAddr = "127.0.0.1:1234"
	usageProgressRecorder := httptest.NewRecorder()
	handler.ServeHTTP(usageProgressRecorder, usageProgressRequest)
	if usageProgressRecorder.Code != http.StatusOK || !strings.Contains(usageProgressRecorder.Body.String(), `"done":true`) {
		t.Fatalf("usage progress status=%d body=%s", usageProgressRecorder.Code, usageProgressRecorder.Body.String())
	}
}

func TestCodexAccountImportRejectsMoreThanLimit(t *testing.T) {
	runtime := &codexAccountRuntimeStub{}
	handler := NewHandler("", &testRuntime{}).WithCodexRuntime(runtime)
	accounts := make([]codexevents.CredentialInput, maxAccountImportItems+1)
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(map[string]any{"accounts": accounts}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/codex/accounts", &body)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AI-Proxy-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "at most 1000") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
