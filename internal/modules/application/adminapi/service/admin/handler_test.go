package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	"aetherrelay/internal/pkg/aetherrelayclientauth"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelaymetrics"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

type testRuntime struct {
	mu        sync.Mutex
	cfg       config.Config
	updates   int
	version   string
	startedAt time.Time
	keyIndex  *clientauth.Index
}

func (r *testRuntime) PrepareClientKeyIndex(records map[string]usage.ClientAPIKeyRecord) (*clientauth.Index, error) {
	entries := make([]clientauth.KeyEntry, 0, len(records))
	for _, record := range records {
		entries = append(entries, clientauth.KeyEntry{ID: record.ID, APIKeyHash: record.Hash, Enabled: record.Enabled, ProviderAccess: record.ProviderAccess})
	}
	return clientauth.PrepareIndex(entries)
}

func (r *testRuntime) ActivateClientKeyIndex(index *clientauth.Index) { r.keyIndex = index }

func (r *testRuntime) EffectiveCatalogSnapshot() effectivecatalog.Snapshot {
	return effectivecatalog.FromStatic(r.ConfigSnapshot())
}

type rejectingRuntime struct{ cfg config.Config }

// catalogChatGPTRuntimeStub supplies an account-pool catalog without making a
// network call. Embedding the existing complete Admin runtime stub keeps this
// focused test aligned with the production ChatGPTRuntime contract.
type catalogChatGPTRuntimeStub struct {
	*chatGPTAccountRuntimeStub
	snapshot effectivecatalog.Snapshot
}

func (s *catalogChatGPTRuntimeStub) ChatGPTEffectiveCatalog(context.Context) (effectivecatalog.Snapshot, error) {
	return s.snapshot, nil
}

func (r *rejectingRuntime) ConfigSnapshot() config.Config { return r.cfg }
func (r *rejectingRuntime) UpdateConfig(config.Config) error {
	return errors.New("activation rejected")
}
func (r *rejectingRuntime) ProviderStorageAvailable() bool { return true }
func (r *rejectingRuntime) ReplaceProviders(map[string]config.Provider) error {
	return errors.New("activation rejected")
}

func (r *testRuntime) ConfigSnapshot() config.Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg
}

func (r *testRuntime) UpdateConfig(cfg config.Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
	r.updates++
	return nil
}

func (r *testRuntime) ProviderStorageAvailable() bool { return true }

func (r *testRuntime) ReplaceProviders(providers map[string]config.Provider) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next, err := config.ReplaceProviders(r.cfg, providers)
	if err != nil {
		return err
	}
	r.cfg = next
	r.updates++
	return nil
}

func (r *testRuntime) SystemVersion() string      { return r.version }
func (r *testRuntime) SystemStartedAt() time.Time { return r.startedAt }

func writeAdminTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `server:
  listen_addr: 127.0.0.1:8080
providers:
  openai:
    enabled: true
    protocol: openai
    base_url: https://api.openai.com/v1
    api_key: ${ADMIN_TEST_API_KEY}
    endpoints: chat_completions
    models: gpt-4o
model_metadata:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHandlerServesProjectAdminPageAndMasksAPIKey(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(path, &testRuntime{cfg: cfg})

	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Provider 管理") {
		t.Fatalf("admin page = %d %s", rec.Code, rec.Body.String())
	}
	for _, marker := range []string{
		"officialCount", "thirdPartyCount", "providerSourceMeta", "provider-table", ".provider-table th,.provider-table td{text-align:left}", "<th>来源</th>", "builtinProviderDialog", "openBuiltinDialog(index)", "provider-health", "builtin-providers", "function providerChanges(original,item)", "function invalidateFeatureCatalog()", "async function createProvider(payload,message,close=false)", "async function patchProvider(name,payload,message,close=false)", "/api/providers/${encodeURIComponent(name)}", `method:"DELETE"`, "featureSubSearch", "cgPanelSearch", "/api/features/search", "/api/features/search/history", "cgSearchHistory", "function loadFeatureSearchHistory()", "function submitFeatureSearch(event)", `id="uaAccountTable"`, "<summary class=\"btn btn-primary\">账号池迁移 ▾</summary><div class=\"account-menu-panel\"><span class=\"menu-label\">整体账号池文件</span>\n              <button type=\"button\" class=\"btn\" id=\"uaBundleImport\">导入整体账号池</button>\n              <button type=\"button\" class=\"btn\" id=\"uaBundleExport\">导出整体账号池</button>", "function unifiedAccounts()", "function unifiedSlotEmail(slot)", "pairUnique(unifiedSlotIdentity,true)", "pairUnique(unifiedSlotEmail,false)", "credential_type=chatgpt_web", "credential_type=codex_cli", "/api/account-pool-bundle/export", "if(state.codex.busy)return;", `id="codexAccNormal"`, `id="codexAccAbnormal"`, `id="codexAccRoutable"`, "`${counts.abnormal} / ${counts.disabled}`", `id="cgImportFile"`, `id="codexImportFile"`, `accept=".json,application/json" multiple`, "async function readAccountImport(fileID,textID)", "accountImportMaxBytes=1<<20", "accountImportMaxItems=1000", "accountImportMaxFiles=20", "function accountImportRequestBody(payload)", "function validCodexImportProxy(value)", "function collectCodexAccountImportFiles(files)", "ignored.push({name:file.name", "function collectChatGPTAccountImportFiles(files)", "beginCodexDiscoveryPolling(result.model_discovery,{visible:false})", "beginCodexUsagePolling(result.usage_refresh,{visible:false})", "function scheduleCodexProgressDismiss(kind,progress,visible)", "renderCodexUsageProgress(null)", "账号刷新完成：成功 ${refreshed}，失败 ${failed}，共 ${total} 个账号", "},4200);",
		`id="featureSubChat" data-feature-sub="chat" class="active">临时对话</button>`, `id="tcAttach" title="添加附件" aria-label="添加附件"`, `application/pdf,text/plain,text/markdown,text/csv`, "temporaryMessageAttachmentURL", `<svg viewBox="0 0 24 24" aria-hidden="true">`, ".tc-citation", "function normalizeTemporaryContent(value)", "function renderTemporaryContent(value)", "renderTemporaryContent(content)", "normalizeTemporaryContent(msg.content)", "function sortChatGPTTasks(items)", `id="cgTaskOwner"`, "if(!setTaskOwner(owner))return;", "loadFeatureModels(state.cg.tasks.mode,true,true);", `id="navSystem"`, `id="panelSystem"`, "function loadSystemInfo()", "/api/system/info", "function displayEmail(email)", "搜索邮箱",
	} {
		if !strings.Contains(rec.Body.String(), marker) {
			t.Fatalf("admin page missing provider source marker %q", marker)
		}
	}
	for _, marker := range []string{"function codexFingerprintControl(account)", "async function updateCodexFingerprintMode(select)", `JSON.stringify({fingerprint_mode:mode})`, "function validCodexImportFingerprintMode(value)"} {
		if !strings.Contains(rec.Body.String(), marker) {
			t.Fatalf("admin page missing Codex fingerprint marker %q", marker)
		}
	}
	for _, marker := range []string{`data-ua-web-reauth=`, `data-ua-codex-reauth=`, `data-acc-reauth=`, `data-codex-reauth=`, `body.target_id=state.cg.oauth.targetId`, `target_id:state.codex.oauth.targetId`} {
		if !strings.Contains(rec.Body.String(), marker) {
			t.Fatalf("admin page missing OAuth reauthentication marker %q", marker)
		}
	}
	for _, obsolete := range []string{`id="codexAccAvailable"`, `id="codexAccUsageLimited"`, `id="codexAccDisabled"`, `id="cgTaskReload"`, `id="usageExport"`, `id="storeDot"`, `id="storeLabel"`, "async function importAccountFiles(files,parse,post)"} {
		if strings.Contains(rec.Body.String(), obsolete) {
			t.Fatalf("admin page still exposes obsolete Codex statistic %q", obsolete)
		}
	}
	for _, id := range []string{`id="uaBundleImport"`, `id="uaBundleExport"`} {
		if count := strings.Count(rec.Body.String(), id); count != 1 {
			t.Fatalf("admin page expects one account-pool migration control %q, got %d", id, count)
		}
	}
	page := rec.Body.String()
	serviceInfoFields := `[[t("服务名称"),service.name],[t("访问基址"),location.origin],[t("服务器时间"),runtime.server_time]`
	if !strings.Contains(page, serviceInfoFields) {
		t.Fatal("system service information does not use the expected fields")
	}
	for _, diagnosticField := range []string{`t("构建修订")`, `t("构建时间")`, `t("工作区修改")`} {
		if strings.Contains(page, diagnosticField) {
			t.Fatalf("system service information exposes diagnostic field %s", diagnosticField)
		}
	}
	chatIndex := strings.Index(page, `id="featureSubChat"`)
	searchIndex := strings.Index(page, `id="featureSubSearch"`)
	tasksIndex := strings.Index(page, `id="featureSubTasks"`)
	imagesIndex := strings.Index(page, `id="featureSubImages"`)
	if chatIndex < 0 || !(chatIndex < searchIndex && searchIndex < tasksIndex && tasksIndex < imagesIndex) {
		t.Fatalf("feature tabs are not ordered chat, search, tasks, images")
	}
	for _, removed := range []string{"routingContent", "routingSearch", "/api/routing/models", "data-builtin-priority-save", "builtin-policy", "<th>Base URL</th>", "<th>路由策略</th>", "历史对话", "历史会话"} {
		if strings.Contains(rec.Body.String(), removed) {
			t.Fatalf("admin page still exposes model routing marker %q", removed)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/api/providers", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("providers = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-value") || strings.Contains(rec.Body.String(), "ADMIN_TEST_API_KEY") {
		t.Fatalf("provider response leaked API key: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"api_key_configured":true`) {
		t.Fatalf("provider response missing configured marker: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"source":"official"`) {
		t.Fatalf("provider response missing official source from base_url: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"provider_writable":true`) || !strings.Contains(rec.Body.String(), `"config_writable":true`) {
		t.Fatalf("provider response missing independent writable states: %s", rec.Body.String())
	}
}

func TestSystemInfoReportsVersionRuntimeAndRegisteredEndpoints(t *testing.T) {
	startedAt := time.Now().UTC().Add(-2 * time.Hour)
	runtime := &testRuntime{cfg: config.Config{}, version: "v1.2.3", startedAt: startedAt}
	handler := NewHandler("", runtime)
	req := httptest.NewRequest(http.MethodGet, "/admin/api/system/info", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("system info status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response systemInfoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Service.Version != "v1.2.3" || response.Runtime.UptimeSeconds < 7100 || response.Runtime.StartedAt != startedAt.Format(time.RFC3339) {
		t.Fatalf("response=%+v", response)
	}
	if len(response.AccessMethods) != 3 || len(response.Endpoints) < 10 {
		t.Fatalf("response=%+v", response)
	}
	foundHealth, foundResponses := false, false
	for _, endpoint := range response.Endpoints {
		if endpoint.Method == http.MethodGet && endpoint.Path == "/healthz" && endpoint.Authentication == "none" {
			foundHealth = true
		}
		if endpoint.Method == http.MethodPost && endpoint.Path == "/v1/responses" && endpoint.Authentication == "client_api_key" {
			foundResponses = true
		}
	}
	if !foundHealth || !foundResponses {
		t.Fatalf("endpoints=%+v", response.Endpoints)
	}
}

func TestHandlerDoesNotExposeRoutingModels(t *testing.T) {
	handler := NewHandler("", &testRuntime{})
	req := httptest.NewRequest(http.MethodGet, "/admin/api/routing/models", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerClassifiesProviderSources(t *testing.T) {
	cfg := config.Config{
		ChatGPTWeb: config.ChatGPTWebConfig{},
		Providers: map[string]config.Provider{
			"deepseek": {
				Name:      "deepseek",
				Protocol:  "openai",
				BaseURL:   "https://api.deepseek.com",
				APIKey:    "sk-test",
				Models:    []string{"deepseek-chat"},
				Endpoints: []string{"chat_completions"},
			},
			"relay": {
				Name:      "relay",
				Protocol:  "openai",
				BaseURL:   "https://aiapi.bluetron.cn",
				APIKey:    "sk-test",
				Models:    []string{"MiniMax*"},
				Endpoints: []string{"chat_completions"},
			},
		},
	}
	handler := NewHandler("", &testRuntime{cfg: cfg})
	req := httptest.NewRequest(http.MethodGet, "/admin/api/providers", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Providers []struct {
			Name    string `json:"name"`
			Source  string `json:"source"`
			Builtin bool   `json:"builtin"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range payload.Providers {
		got[p.Name] = p.Source
		if p.Name == "chatgptweb" && (!p.Builtin || p.Source != ProviderSourceBuiltin) {
			t.Fatalf("chatgptweb = %+v", p)
		}
	}
	if got["deepseek"] != ProviderSourceOfficial {
		t.Fatalf("deepseek source = %q, want official", got["deepseek"])
	}
	if got["relay"] != ProviderSourceThirdParty {
		t.Fatalf("relay source = %q, want third_party", got["relay"])
	}
	if got["chatgptweb"] != ProviderSourceBuiltin {
		t.Fatalf("chatgptweb source = %q, want builtin", got["chatgptweb"])
	}
}

func TestHandlerRejectsRemoteAdminAccess(t *testing.T) {
	handler := NewHandler("config.yaml", &testRuntime{})
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.RemoteAddr = "203.0.113.8:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestHandlerProjectsProviderAvailability(t *testing.T) {
	registry := metrics.NewRegistry()
	for range 3 {
		registry.RecordRequest("healthy", "m", "chat_completions", http.StatusOK, time.Millisecond, "success")
	}
	for range 4 {
		registry.RecordRequest("degraded", "m", "chat_completions", http.StatusOK, time.Millisecond, "success")
	}
	registry.RecordRequest("degraded", "m", "chat_completions", http.StatusBadGateway, time.Millisecond, "upstream_failed")
	for range 3 {
		registry.RecordRequest("unavailable", "m", "chat_completions", http.StatusServiceUnavailable, time.Millisecond, "upstream_failed")
	}
	registry.RecordRequest("credential", "m", "chat_completions", http.StatusForbidden, time.Millisecond, "upstream_failed")
	registry.RecordRequest("drift", "m", "chat_completions", http.StatusBadRequest, time.Millisecond, "endpoint_drift")
	cfg := config.Config{Providers: map[string]config.Provider{
		"healthy":     {},
		"degraded":    {},
		"unavailable": {},
		"credential":  {},
		"drift":       {},
		"unknown":     {},
		"disabled":    {Disabled: true},
	}}
	handler := NewHandler("", &testRuntime{cfg: cfg}).WithMetrics(registry)
	availability := handler.providerHealth(cfg)
	for name, want := range map[string]string{
		"healthy": "healthy", "degraded": "degraded", "unavailable": "unhealthy",
		"credential": "credential_error", "drift": "endpoint_drift", "unknown": "unknown", "disabled": "disabled",
	} {
		if got := availability[name].Status; got != want {
			t.Errorf("%s status = %q, want %q", name, got, want)
		}
	}
}

func TestBuiltinDisabledAvailabilityKeepsMetricsAsDetails(t *testing.T) {
	registry := metrics.NewRegistry()
	for range 3 {
		registry.RecordRequest("chatgptweb", "gpt-5", "chat_completions", http.StatusOK, time.Millisecond, "success")
	}
	handler := NewHandler("", &testRuntime{}).WithMetrics(registry)
	view := providerView{Name: "chatgptweb", Availability: providerAvailability{Status: "disabled"}}
	handler.applyObservedHealth(&view)
	if view.Availability.Status != "disabled" || view.Availability.SampleCount != 3 || view.Availability.SuccessRate != 1 {
		t.Fatalf("availability=%+v", view.Availability)
	}
}

func TestHandlerProbesProviderAndRecordsAvailability(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-value" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-probe"}`))
	}))
	defer upstream.Close()
	provider := cfg.Providers["openai"]
	provider.BaseURL = upstream.URL
	cfg.Providers["openai"] = provider
	registry := metrics.NewRegistry()
	handler := NewHandler(path, &testRuntime{cfg: cfg}).WithMetrics(registry)

	missingHeader := httptest.NewRequest(http.MethodPost, "/admin/api/providers/openai/probe", nil)
	missingHeader.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, missingHeader)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("probe without header = %d, want 403", rec.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers/openai/probe", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"conclusion":"success"`) {
		t.Fatalf("probe = %d %s", rec.Code, rec.Body.String())
	}
	availability := handler.providerHealth(cfg)["openai"]
	if availability.Status != "unknown" || availability.LastOutcome != "success" || availability.SampleCount != 1 || availability.Score != 100 {
		t.Fatalf("availability = %#v", availability)
	}
}

func TestHandlerProbesBuiltinProviderFromCatalogWithoutRecordingMetrics(t *testing.T) {
	cfg := config.Config{ChatGPTWeb: config.ChatGPTWebConfig{}}
	snapshot := effectivecatalog.Build(cfg, 1, 2, []effectivecatalog.PoolModel{{
		ID: "gpt-5",
	}}, "2026-08-03T12:00:00Z")
	runtime := &catalogChatGPTRuntimeStub{
		chatGPTAccountRuntimeStub: &chatGPTAccountRuntimeStub{},
		snapshot:                  snapshot,
	}
	registry := metrics.NewRegistry()
	handler := NewHandler("", &testRuntime{cfg: cfg}).WithChatGPTRuntime(runtime).WithMetrics(registry)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers/chatgptweb/probe", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"conclusion":"success"`) || !strings.Contains(rec.Body.String(), `"status":200`) {
		t.Fatalf("builtin probe = %d %s", rec.Code, rec.Body.String())
	}
	if health := registry.ProviderHealthSnapshot(); len(health) != 0 {
		t.Fatalf("builtin catalog check must not record health samples: %#v", health)
	}
}

func TestHandlerCreatesProviderWithoutRewritingConfigAndHotReloads(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{cfg: cfg}
	handler := NewHandler(path, runtime)
	zeroPriority := 0
	body, err := json.Marshal(providerInput{
		Name:      "gateway",
		Protocol:  "openai",
		BaseURL:   "https://gateway.example.com/v1",
		APIKey:    "gateway-secret",
		Models:    []string{"gpt-*"},
		Endpoints: []string{config.ProviderEndpointChatCompletions},
		Priority:  &zeroPriority,
		Enabled:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "${ADMIN_TEST_API_KEY}") || strings.Contains(string(raw), "gateway.example.com") || strings.Contains(string(raw), "priority: 0") {
		t.Fatalf("Provider update unexpectedly rewrote config.yaml:\n%s", raw)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.updates != 1 {
		t.Fatalf("updates = %d, want 1", runtime.updates)
	}
	provider := runtime.cfg.Providers["gateway"]
	if provider.BaseURL != "https://gateway.example.com/v1" || provider.APIKey != "gateway-secret" || config.EffectiveProviderPriority(provider) != 0 {
		t.Fatalf("runtime provider = %+v", provider)
	}
	if runtime.cfg.Providers["openai"].APIKey != "secret-value" {
		t.Fatal("creating Provider changed existing Provider credential")
	}
}

func TestHandlerRejectsProviderCreationWithoutAPIKey(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{cfg: cfg}
	handler := NewHandler(path, runtime)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers", strings.NewReader(`{"name":"gateway","protocol":"openai","base_url":"http://127.0.0.1:8081/v1","models":["gpt-*"],"endpoints":["chat_completions"],"enabled":false}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "api_key is required") {
		t.Fatalf("create without key = %d %s", rec.Code, rec.Body.String())
	}
	if _, exists := runtime.ConfigSnapshot().Providers["gateway"]; exists {
		t.Fatal("provider without API key was created")
	}
}

func TestHandlerPatchesOnlyTargetProviderAndPreservesCredential(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers["other"] = config.Provider{
		Name: "other", Protocol: "openai", BaseURL: "https://other.example/v1", APIKey: "other-secret",
		Models: []string{"other-*"}, Endpoints: []string{config.ProviderEndpointChatCompletions},
	}
	runtime := &testRuntime{cfg: cfg}
	handler := NewHandler(path, runtime)

	body := `{"base_url":"https://gateway.example.com/v1","api_key":"","enabled":false}`
	req := httptest.NewRequest(http.MethodPatch, "/admin/api/providers/OPENAI", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
	updated := runtime.ConfigSnapshot()
	if got := updated.Providers["openai"]; got.BaseURL != "https://gateway.example.com/v1" || got.APIKey != "secret-value" || !got.Disabled {
		t.Fatalf("patched provider = %+v", got)
	}
	if got := updated.Providers["other"]; got.BaseURL != "https://other.example/v1" || got.APIKey != "other-secret" || got.Disabled {
		t.Fatalf("unrelated provider changed = %+v", got)
	}
}

func TestHandlerRejectsRemovedProviderConversionRelease(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{cfg: cfg}
	handler := NewHandler(path, runtime)
	body := `{"conversion_releases":{"gpt-4o":{"anthropic_to_responses":{"enabled":true,"verified":true,"evidence_id":"eval-admin"}}}}`
	req := httptest.NewRequest(http.MethodPatch, "/admin/api/providers/openai", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown field") {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerSwitchesProviderTransportWithoutReleaseState(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{cfg: cfg}
	handler := NewHandler(path, runtime)
	req := httptest.NewRequest(http.MethodPatch, "/admin/api/providers/openai", strings.NewReader(`{"protocol":"anthropic","base_url":"https://api.deepseek.com/anthropic","endpoints":["messages"]}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
	updated := runtime.ConfigSnapshot().Providers["openai"]
	if updated.Protocol != "anthropic" || len(updated.Endpoints) != 1 || updated.Endpoints[0] != config.ProviderEndpointMessages || updated.APIKey != "secret-value" {
		t.Fatalf("provider = %#v", updated)
	}
}

func TestHandlerDeletesOnlyTargetProvider(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers["other"] = config.Provider{
		Name: "other", Protocol: "openai", BaseURL: "https://other.example/v1", APIKey: "other-secret",
		Models: []string{"other-*"}, Endpoints: []string{config.ProviderEndpointChatCompletions},
	}
	runtime := &testRuntime{cfg: cfg}
	handler := NewHandlerWithUsage(path, runtime, usage.NewMemoryStore())
	req := httptest.NewRequest(http.MethodDelete, "/admin/api/providers/other", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	updated := runtime.ConfigSnapshot()
	if _, exists := updated.Providers["other"]; exists {
		t.Fatal("deleted Provider remains present")
	}
	if got := updated.Providers["openai"]; got.APIKey != "secret-value" || got.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("unrelated provider changed = %+v", got)
	}
}

func TestHandlerProviderPatchCredentialAndErrorSemantics(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{cfg: cfg}
	handler := NewHandler(path, runtime)

	patch := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-AetherRelay-Admin", "1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := patch("/admin/api/providers/openai", `{"api_key":"replacement"}`); rec.Code != http.StatusOK {
		t.Fatalf("replace credential = %d %s", rec.Code, rec.Body.String())
	}
	if got := runtime.ConfigSnapshot().Providers["openai"].APIKey; got != "replacement" {
		t.Fatalf("credential = %q", got)
	}
	if rec := patch("/admin/api/providers/openai", `{"base_url":"http://127.0.0.1:8081/v1","clear_api_key":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("clear credential = %d %s", rec.Code, rec.Body.String())
	}
	if got := runtime.ConfigSnapshot().Providers["openai"]; got.APIKey != "replacement" || got.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("provider changed after rejected clear = %+v", got)
	}
	for _, tc := range []struct {
		path string
		body string
		code int
	}{
		{path: "/admin/api/providers/missing", body: `{"enabled":false}`, code: http.StatusNotFound},
		{path: "/admin/api/providers/chatgptweb", body: `{"enabled":false}`, code: http.StatusBadRequest},
		{path: "/admin/api/providers/openai", body: `{}`, code: http.StatusBadRequest},
		{path: "/admin/api/providers/openai", body: `{"api_key":"new","clear_api_key":true}`, code: http.StatusBadRequest},
	} {
		if rec := patch(tc.path, tc.body); rec.Code != tc.code {
			t.Fatalf("patch %s = %d, want %d: %s", tc.path, rec.Code, tc.code, rec.Body.String())
		}
	}
}

func TestHandlerProviderPatchReportsActivationFailure(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(path, &rejectingRuntime{cfg: cfg})
	req := httptest.NewRequest(http.MethodPatch, "/admin/api/providers/openai", strings.NewReader(`{"enabled":false}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "activation rejected") {
		t.Fatalf("activation failure = %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerUpdatesBuiltinProviderRoutingPolicy(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte(`
chatgpt_web:
  provider_enabled: true
  priority: 10
codex_oauth:
  provider_enabled: true
  priority: 90
`)...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{cfg: cfg}
	handler := NewHandler(path, runtime)
	enabled := false
	priority := 0
	body, err := json.Marshal(builtinProviderInput{Enabled: &enabled, Priority: &priority})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/admin/api/builtin-providers/chatgptweb", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(persisted), "provider_enabled: false") || !strings.Contains(string(persisted), "priority: 0") {
		t.Fatalf("builtin route policy was not persisted:\n%s", persisted)
	}
	updated := runtime.ConfigSnapshot()
	if config.EffectiveChatGPTWebProviderEnabled(updated.ChatGPTWeb) || config.EffectiveChatGPTWebProviderPriority(updated.ChatGPTWeb) != 0 {
		t.Fatalf("runtime builtin policy = %+v", updated.ChatGPTWeb)
	}
	if runtime.updates != 1 {
		t.Fatalf("updates=%d want 1", runtime.updates)
	}
}

func TestHandlerListsAlwaysAssembledBuiltinProviders(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(path, &testRuntime{cfg: cfg})
	req := httptest.NewRequest(http.MethodGet, "/admin/api/providers", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Providers []providerView `json:"providers"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	got := map[string]providerView{}
	for _, provider := range payload.Providers {
		got[provider.Name] = provider
	}
	for _, id := range []string{"chatgptweb", "codexoauth"} {
		provider, ok := got[id]
		if !ok || !provider.Builtin || !provider.Enabled || provider.Availability.Status != "unavailable" {
			t.Fatalf("builtin %s = %+v, present=%t", id, provider, ok)
		}
	}
}

func TestHandlerManagesAdminDefaultLanguage(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{cfg: cfg}
	handler := NewHandler(path, runtime)

	get := httptest.NewRequest(http.MethodGet, "/admin/api/admin/preferences", nil)
	get.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"default_language":"zh-CN"`) || !strings.Contains(rec.Body.String(), `"writable":true`) {
		t.Fatalf("preferences = %d %s", rec.Code, rec.Body.String())
	}

	put := httptest.NewRequest(http.MethodPut, "/admin/api/admin/preferences", strings.NewReader(`{"default_language":"en-US"}`))
	put.RemoteAddr = "127.0.0.1:1234"
	put.Header.Set("Content-Type", "application/json")
	put.Header.Set("X-AetherRelay-Admin", "1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, put)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"default_language":"en-US"`) {
		t.Fatalf("update preferences = %d %s", rec.Code, rec.Body.String())
	}
	if runtime.ConfigSnapshot().AdminAuth.DefaultLanguage != "en-US" {
		t.Fatalf("runtime default language = %q", runtime.ConfigSnapshot().AdminAuth.DefaultLanguage)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "admin_default_language: \"en-US\"") {
		t.Fatalf("config did not persist default language:\n%s", raw)
	}
}

func TestHandlerRejectsUnsupportedAdminDefaultLanguageWithoutWriting(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(path, &testRuntime{cfg: cfg})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/admin/preferences", strings.NewReader(`{"default_language":"fr-FR"}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("unsupported language replaced config file")
	}
}

func TestHandlerDoesNotPersistPreferencesWhenActivationFails(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &rejectingRuntime{cfg: cfg}
	handler := NewHandler(path, runtime)
	req := httptest.NewRequest(http.MethodPut, "/admin/api/admin/preferences", strings.NewReader(`{"default_language":"en-US"}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("activation failure replaced config file")
	}
	if runtime.ConfigSnapshot().AdminAuth.DefaultLanguage != config.DefaultAdminLanguage {
		t.Fatalf("runtime default language = %q", runtime.ConfigSnapshot().AdminAuth.DefaultLanguage)
	}
}

func TestLegacyClientAPIKeyConfigManagementRemoved(t *testing.T) {
	t.Skip("obsolete: client API keys are managed by DuckDB")
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{cfg: cfg}
	handler := NewHandler(path, runtime)

	create := httptest.NewRequest(http.MethodPost, "/admin/api/client-api-keys", strings.NewReader(`{"id":"ci-agent"}`))
	create.RemoteAddr = "127.0.0.1:1234"
	create.Header.Set("X-AetherRelay-Admin", "1")
	create.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || !strings.HasPrefix(created.APIKey, "sk_") {
		t.Fatalf("created = %#v err=%v", created, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw

	list := httptest.NewRequest(http.MethodGet, "/admin/api/client-api-keys", nil)
	list.RemoteAddr = "127.0.0.1:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, list)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), created.APIKey) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}

	disable := httptest.NewRequest(http.MethodPatch, "/admin/api/client-api-keys/ci-agent", strings.NewReader(`{"enabled":false}`))
	disable.RemoteAddr = "127.0.0.1:1234"
	disable.Header.Set("X-AetherRelay-Admin", "1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, disable)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteClientAPIKeyRemovesInteractionScope(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{InteractionDir: filepath.Join(root, "interactions")}
	runtime := &testRuntime{cfg: cfg}
	store := usage.NewMemoryStore()
	now := time.Now().UTC()
	if err := store.CreateClientAPIKey(context.Background(), usage.ClientAPIKeyRecord{ID: "ci-agent", Hash: "sha256:test", Enabled: true, CreatedAt: now, ProviderAccess: clientaccess.All()}); err != nil {
		t.Fatal(err)
	}
	scope := filepath.Join(cfg.InteractionDir, "ci-agent")
	if err := os.MkdirAll(filepath.Join(scope, "000001"), 0o700); err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithUsage("", runtime, store)
	req := httptest.NewRequest(http.MethodDelete, "/admin/api/client-api-keys/ci-agent", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(scope); !os.IsNotExist(err) {
		t.Fatalf("interaction scope still exists: %v", err)
	}
	keys, err := store.ListClientAPIKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keys["ci-agent"]; ok {
		t.Fatal("client API key metadata still exists")
	}
}

func TestClientAPIKeyProviderAccessAndEffectiveModels(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{cfg: cfg}
	store := usage.NewMemoryStore()
	handler := NewHandlerWithUsage(path, runtime, store)

	create := httptest.NewRequest(http.MethodPost, "/admin/api/client-api-keys", strings.NewReader(`{"id":"scoped","provider_access":{"mode":"selected","provider_ids":["openai"]}}`))
	create.RemoteAddr = "127.0.0.1:1234"
	create.Header.Set("X-AetherRelay-Admin", "1")
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, create)
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"mode":"selected"`) {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}

	models := httptest.NewRequest(http.MethodGet, "/admin/api/client-api-keys/scoped/models", nil)
	models.RemoteAddr = "127.0.0.1:1234"
	modelResponse := httptest.NewRecorder()
	handler.ServeHTTP(modelResponse, models)
	if modelResponse.Code != http.StatusOK || !strings.Contains(modelResponse.Body.String(), `"id":"gpt-4o"`) || !strings.Contains(modelResponse.Body.String(), `"provider_ids":["openai"]`) {
		t.Fatalf("models = %d %s", modelResponse.Code, modelResponse.Body.String())
	}

	update := httptest.NewRequest(http.MethodPut, "/admin/api/client-api-keys/scoped/provider-access", strings.NewReader(`{"mode":"all","provider_ids":[]}`))
	update.RemoteAddr = "127.0.0.1:1234"
	update.Header.Set("X-AetherRelay-Admin", "1")
	updated := httptest.NewRecorder()
	handler.ServeHTTP(updated, update)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"mode":"all"`) {
		t.Fatalf("update = %d %s", updated.Code, updated.Body.String())
	}

	bad := httptest.NewRequest(http.MethodPut, "/admin/api/client-api-keys/scoped/provider-access", strings.NewReader(`{"mode":"selected","provider_ids":["missing"]}`))
	bad.RemoteAddr = "127.0.0.1:1234"
	bad.Header.Set("X-AetherRelay-Admin", "1")
	badResponse := httptest.NewRecorder()
	handler.ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider = %d %s", badResponse.Code, badResponse.Body.String())
	}
}

func TestBuiltinLocalClientAPIKeyIsListedButImmutable(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store := usage.NewMemoryStore()
	if err := store.EnsureClientAPIKey(context.Background(), config.BuiltinClientAPIKeyID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{cfg: cfg}
	handler := NewHandlerWithUsage(path, runtime, store)

	list := httptest.NewRequest(http.MethodGet, "/admin/api/client-api-keys", nil)
	list.RemoteAddr = "127.0.0.1:1234"
	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, list)
	if listed.Code != http.StatusOK {
		t.Fatalf("list = %d %s", listed.Code, listed.Body.String())
	}
	var payload struct {
		Keys []struct {
			ID      string `json:"id"`
			Builtin bool   `json:"builtin"`
		} `json:"client_api_keys"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Keys) != 1 || payload.Keys[0].ID != config.BuiltinClientAPIKeyID || !payload.Keys[0].Builtin {
		t.Fatalf("built-in key list = %#v", payload.Keys)
	}
	if strings.Contains(listed.Body.String(), "hash") || strings.Contains(listed.Body.String(), "secret") {
		t.Fatalf("built-in key response leaked credential material: %s", listed.Body.String())
	}

	create := httptest.NewRequest(http.MethodPost, "/admin/api/client-api-keys", strings.NewReader(`{"id":"BUILTIN-LOCAL","provider_access":{"mode":"all","provider_ids":[]}}`))
	create.RemoteAddr = "127.0.0.1:1234"
	create.Header.Set("X-AetherRelay-Admin", "1")
	create.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, create)
	if created.Code != http.StatusBadRequest {
		t.Fatalf("create built-in = %d %s", created.Code, created.Body.String())
	}

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "patch", method: http.MethodPatch, path: "/admin/api/client-api-keys/builtin-local", body: `{"enabled":false}`},
		{name: "provider access", method: http.MethodPut, path: "/admin/api/client-api-keys/builtin-local/provider-access", body: `{"mode":"all","provider_ids":[]}`},
		{name: "rotate", method: http.MethodPost, path: "/admin/api/client-api-keys/builtin-local/rotate"},
		{name: "delete", method: http.MethodDelete, path: "/admin/api/client-api-keys/builtin-local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("X-AetherRelay-Admin", "1")
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "built-in") {
				t.Fatalf("mutation = %d %s", response.Code, response.Body.String())
			}
		})
	}

	models := httptest.NewRequest(http.MethodGet, "/admin/api/client-api-keys/builtin-local/models", nil)
	models.RemoteAddr = "127.0.0.1:1234"
	modelResponse := httptest.NewRecorder()
	handler.ServeHTTP(modelResponse, models)
	if modelResponse.Code != http.StatusOK || !strings.Contains(modelResponse.Body.String(), `"id":"gpt-4o"`) {
		t.Fatalf("built-in models = %d %s", modelResponse.Code, modelResponse.Body.String())
	}

	imageScopeResponse := httptest.NewRecorder()
	imageScope, ok := handler.imageAPIKeyID(imageScopeResponse, context.Background(), "", "")
	if !ok || imageScope != config.BuiltinClientAPIKeyID {
		t.Fatalf("empty Admin image scope = %q ok=%v response=%d", imageScope, ok, imageScopeResponse.Code)
	}
}

func TestChatGPTImageTaskListUsesSelectedClientAPIKey(t *testing.T) {
	store := usage.NewMemoryStore()
	now := time.Now().UTC()
	for _, id := range []string{config.BuiltinClientAPIKeyID, "team-a"} {
		if err := store.CreateClientAPIKey(context.Background(), usage.ClientAPIKeyRecord{ID: id, Enabled: true, CreatedAt: now, ProviderAccess: clientaccess.All()}); err != nil {
			t.Fatal(err)
		}
	}
	runtime := &chatGPTAccountRuntimeStub{}
	handler := NewHandlerWithUsage("", &testRuntime{}, store).WithChatGPTRuntime(runtime)
	req := httptest.NewRequest(http.MethodGet, "/admin/api/chatgpt/image-tasks?api_key_id=team-a", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || runtime.taskListOwner != "team-a" {
		t.Fatalf("list status=%d owner=%q body=%s", rec.Code, runtime.taskListOwner, rec.Body.String())
	}
}

func TestDeleteProviderRejectsSelectedClientKeyReference(t *testing.T) {
	cfg := config.Config{Providers: map[string]config.Provider{
		"target": {Name: "target", Protocol: "openai", BaseURL: "https://target.test", APIKey: "k", Models: []string{"model"}, Endpoints: []string{config.ProviderEndpointResponses}},
	}, ModelMetadata: map[string]config.ModelMetadata{"model": {ID: "model"}}}
	runtime := &testRuntime{cfg: cfg}
	store := usage.NewMemoryStore()
	policy, err := clientaccess.Selected([]string{"target"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateClientAPIKey(context.Background(), usage.ClientAPIKeyRecord{ID: "bound", Hash: "sha256:" + strings.Repeat("0", 64), Enabled: true, CreatedAt: time.Now().UTC(), ProviderAccess: policy}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithUsage("", runtime, store)
	req := httptest.NewRequest(http.MethodDelete, "/admin/api/providers/target", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "bound") {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := runtime.ConfigSnapshot().Providers["target"]; !ok {
		t.Fatal("referenced provider was deleted")
	}
}

func TestHandlerAllowsProviderChangeThatLeavesUnusedMetadata(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(path, &testRuntime{cfg: cfg})
	body := []byte(`{"models":["other-*"]}`)
	req := httptest.NewRequest(http.MethodPatch, "/admin/api/providers/openai", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("managed Provider update rewrote config.yaml")
	}
}
