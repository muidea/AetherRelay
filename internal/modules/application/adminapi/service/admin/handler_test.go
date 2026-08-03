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

	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetrics"
)

type testRuntime struct {
	mu      sync.Mutex
	cfg     config.Config
	updates int
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
    endpoint_capabilities: chat_completions
    models: gpt-*
model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
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
		"officialCount", "thirdPartyCount", "providerSourceMeta", "provider-table", ".provider-table th,.provider-table td{text-align:left}", "<th>来源</th>", "builtinProviderDialog", "openBuiltinDialog(index)", "provider-health", "builtin-providers",
		`id="featureSubChat" data-feature-sub="chat">临时对话</button>`, `id="tcAttach" title="添加附件" aria-label="添加附件"`, `application/pdf,text/plain,text/markdown,text/csv`, "temporaryMessageAttachmentURL", `<svg viewBox="0 0 24 24" aria-hidden="true">`, ".tc-citation", "function normalizeTemporaryContent(value)", "function renderTemporaryContent(value)", "renderTemporaryContent(content)", "normalizeTemporaryContent(msg.content)", "function sortChatGPTTasks(items)",
	} {
		if !strings.Contains(rec.Body.String(), marker) {
			t.Fatalf("admin page missing provider source marker %q", marker)
		}
	}
	for _, removed := range []string{"routingContent", "routingSearch", "/api/routing/models", "data-builtin-priority-save", "builtin-policy", "历史对话", "历史会话"} {
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
		ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true},
		Providers: map[string]config.Provider{
			"deepseek": {
				Name:                 "deepseek",
				Protocol:             "openai",
				BaseURL:              "https://api.deepseek.com",
				APIKey:               "sk-test",
				Models:               []string{"deepseek-chat"},
				EndpointCapabilities: []string{"chat_completions"},
			},
			"relay": {
				Name:                 "relay",
				Protocol:             "openai",
				BaseURL:              "https://aiapi.bluetron.cn",
				APIKey:               "sk-test",
				Models:               []string{"MiniMax*"},
				EndpointCapabilities: []string{"chat_completions"},
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
	registry.RecordRequest("drift", "m", "chat_completions", http.StatusBadRequest, time.Millisecond, "capability_drift")
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
		"credential": "credential_error", "drift": "capability_drift", "unknown": "unknown", "disabled": "disabled",
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
	req.Header.Set("X-AI-Proxy-Admin", "1")
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
	cfg := config.Config{ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true}}
	snapshot := effectivecatalog.Build(cfg, 1, 2, []effectivecatalog.PoolModel{{
		ID:         "gpt-5",
		Operations: []string{config.EndpointCapabilityChatCompletions},
	}}, "2026-08-03T12:00:00Z")
	runtime := &catalogChatGPTRuntimeStub{
		chatGPTAccountRuntimeStub: &chatGPTAccountRuntimeStub{},
		snapshot:                  snapshot,
	}
	registry := metrics.NewRegistry()
	handler := NewHandler("", &testRuntime{cfg: cfg}).WithChatGPTRuntime(runtime).WithMetrics(registry)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers/chatgptweb/probe", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AI-Proxy-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"conclusion":"success"`) || !strings.Contains(rec.Body.String(), `"status":200`) {
		t.Fatalf("builtin probe = %d %s", rec.Code, rec.Body.String())
	}
	if health := registry.ProviderHealthSnapshot(); len(health) != 0 {
		t.Fatalf("builtin catalog check must not record health samples: %#v", health)
	}
}

func TestHandlerUpdatesProvidersPreservesRawSecretAndHotReloads(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{cfg: cfg}
	handler := NewHandler(path, runtime)
	zeroPriority := 0
	body, err := json.Marshal(updateRequest{Providers: []providerInput{{
		Name:                 "openai",
		Protocol:             "openai",
		BaseURL:              "https://gateway.example.com/v1",
		Models:               []string{"gpt-*"},
		EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions},
		Priority:             &zeroPriority,
		Enabled:              true,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/admin/api/providers", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AI-Proxy-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "${ADMIN_TEST_API_KEY}") {
		t.Fatalf("raw API key expression was not preserved:\n%s", raw)
	}
	if !strings.Contains(string(raw), "priority: 0") {
		t.Fatalf("explicit zero priority was not persisted:\n%s", raw)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.updates != 1 {
		t.Fatalf("updates = %d, want 1", runtime.updates)
	}
	provider := runtime.cfg.Providers["openai"]
	if provider.BaseURL != "https://gateway.example.com/v1" || provider.APIKey != "secret-value" || config.EffectiveProviderPriority(provider) != 0 {
		t.Fatalf("runtime provider = %+v", provider)
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
  enabled: true
  provider_enabled: true
  priority: 10
codex_oauth:
  enabled: true
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
	req.Header.Set("X-AI-Proxy-Admin", "1")
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

func TestHandlerListsDisabledBuiltinProviders(t *testing.T) {
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
		if !ok || !provider.Builtin || provider.Enabled || provider.Availability.Status != "disabled" {
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
	put.Header.Set("X-AI-Proxy-Admin", "1")
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
	req.Header.Set("X-AI-Proxy-Admin", "1")
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
	req.Header.Set("X-AI-Proxy-Admin", "1")
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

func TestHandlerManagesHashedClientAPIKeys(t *testing.T) {
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
	create.Header.Set("X-AI-Proxy-Admin", "1")
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
	if strings.Contains(string(raw), created.APIKey) || !strings.Contains(string(raw), "api_key_hash") {
		t.Fatalf("key storage leaked secret: %s", raw)
	}

	list := httptest.NewRequest(http.MethodGet, "/admin/api/client-api-keys", nil)
	list.RemoteAddr = "127.0.0.1:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, list)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), created.APIKey) || !strings.Contains(rec.Body.String(), `"credential_source":"managed"`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}

	disable := httptest.NewRequest(http.MethodPatch, "/admin/api/client-api-keys/ci-agent", strings.NewReader(`{"enabled":false}`))
	disable.RemoteAddr = "127.0.0.1:1234"
	disable.Header.Set("X-AI-Proxy-Admin", "1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, disable)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
	if runtime.cfg.ClientAPIKeys["ci-agent"].Enabled {
		t.Fatal("key remained enabled")
	}
}

func TestHandlerRejectsInvalidProviderChangeWithoutReplacingConfig(t *testing.T) {
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
	body := []byte(`{"providers":[{"name":"openai","protocol":"openai","base_url":"https://api.openai.com/v1","models":["other-*"],"endpoint_capabilities":["chat_completions"],"enabled":true}]}`)
	req := httptest.NewRequest(http.MethodPut, "/admin/api/providers", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AI-Proxy-Admin", "1")
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
		t.Fatal("invalid update replaced config file")
	}
}
