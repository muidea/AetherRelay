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

	admincodex "aetherrelay/internal/modules/application/adminapi/pkg/codexmanagement"
	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	codexevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

type codexAccountRuntimeStub struct {
	accounts     []codexevents.AccountView
	started      [][]string
	usageStarted [][]string
	exportedIDs  []string
	exported     []codexevents.CredentialInput
	imported     []codexevents.CredentialInput
	importErr    error
}

func (s *codexAccountRuntimeStub) ListCodexAccounts(context.Context) ([]codexevents.AccountView, error) {
	return append([]codexevents.AccountView(nil), s.accounts...), nil
}
func (s *codexAccountRuntimeStub) ImportCodexAccounts(_ context.Context, accounts []codexevents.CredentialInput) (admincodex.ImportResult, error) {
	s.imported = append([]codexevents.CredentialInput(nil), accounts...)
	if s.importErr != nil {
		return admincodex.ImportResult{}, s.importErr
	}
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

func TestCodexAccountSlotExportEndpointRemoved(t *testing.T) {
	handler := NewHandler("", &testRuntime{}).WithCodexRuntime(&codexAccountRuntimeStub{})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/codex/accounts/export", strings.NewReader(`{"ids":["account-1"]}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("slot export status=%d body=%s", rec.Code, rec.Body.String())
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
	importRequest := httptest.NewRequest(http.MethodPost, "/admin/api/codex/accounts", strings.NewReader(`{"accounts":[{"credential_type":"codex_cli","access_token":"access","refresh_token":"refresh"}]}`))
	importRequest.RemoteAddr = "127.0.0.1:1234"
	importRequest.Header.Set("X-AetherRelay-Admin", "1")
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
	startRequest.Header.Set("X-AetherRelay-Admin", "1")
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
	usageRequest.Header.Set("X-AetherRelay-Admin", "1")
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

func TestCodexAccountImportAcceptsDirectCredentialShapes(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantAccess []string
	}{
		{
			name:       "single object",
			payload:    `{"credential_type":"codex_cli","access_token":"access-single","refresh_token":"refresh-single","email":"single@example.invalid"}`,
			wantAccess: []string{"access-single"},
		},
		{
			name:       "array",
			payload:    `[{"credential_type":"codex_cli","access_token":"access-one","refresh_token":"refresh-one"},{"credential_type":"codex_cli","access_token":"access-two","refresh_token":"refresh-two"}]`,
			wantAccess: []string{"access-one", "access-two"},
		},
		{
			name:       "legacy envelope",
			payload:    `{"accounts":[{"credential_type":"codex_cli","access_token":"access-wrapped","refresh_token":"refresh-wrapped"}]}`,
			wantAccess: []string{"access-wrapped"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := &codexAccountRuntimeStub{}
			handler := NewHandler("", &testRuntime{}).WithCodexRuntime(runtime)
			req := httptest.NewRequest(http.MethodPost, "/admin/api/codex/accounts", strings.NewReader(tt.payload))
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("X-AetherRelay-Admin", "1")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusCreated {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if len(runtime.imported) != len(tt.wantAccess) {
				t.Fatalf("accounts=%d want=%d", len(runtime.imported), len(tt.wantAccess))
			}
			for i, account := range runtime.imported {
				if account.AccessToken != tt.wantAccess[i] {
					t.Fatalf("account[%d].access_token=%q want=%q", i, account.AccessToken, tt.wantAccess[i])
				}
			}
		})
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
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "at most 1000") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
