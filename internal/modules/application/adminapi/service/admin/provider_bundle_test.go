package admin

import (
	"encoding/json"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	config "aetherrelay/internal/pkg/aetherrelayconfig"
)

func TestProviderBundleExportUsesPayloadTimestampAndProfileFilename(t *testing.T) {
	runtime := &testRuntime{cfg: config.Config{Providers: map[string]config.Provider{
		"gateway": {
			Name:     "gateway",
			Protocol: "openai",
			BaseURL:  "https://gateway.example/v1",
			APIKey:   "provider-secret",
			Models:   []string{"model-a"},
		},
	}}}
	h := NewHandler("", runtime)

	for _, includeSecrets := range []bool{false, true} {
		name := bundleExportProfileSafe
		if includeSecrets {
			name = bundleExportProfileComplete
		}
		req := httptest.NewRequest(http.MethodPost, "/admin/api/providers/export", strings.NewReader(`{"include_api_keys":`+boolString(includeSecrets)+`}`))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-AetherRelay-Admin", "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("include_secrets=%t status=%d body=%s", includeSecrets, rec.Code, rec.Body.String())
		}
		var payload providerBundle
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("include_secrets=%t decode: %v", includeSecrets, err)
		}
		exportedAt, err := time.Parse(time.RFC3339, payload.ExportedAt)
		if err != nil {
			t.Fatalf("include_secrets=%t exported_at=%q: %v", includeSecrets, payload.ExportedAt, err)
		}
		mediaType, params, err := mime.ParseMediaType(rec.Header().Get("Content-Disposition"))
		if err != nil {
			t.Fatalf("include_secrets=%t parse disposition: %v", includeSecrets, err)
		}
		if mediaType != "attachment" || params["filename"] != bundleExportFilename(bundleExportArtifactProvider, providerBundleSchemaVersion, name, exportedAt) {
			t.Fatalf("include_secrets=%t disposition=%q", includeSecrets, rec.Header().Get("Content-Disposition"))
		}
		if len(payload.Providers) != 1 {
			t.Fatalf("include_secrets=%t providers=%+v", includeSecrets, payload.Providers)
		}
		if includeSecrets != (payload.Providers[0].APIKey != nil) {
			t.Fatalf("include_secrets=%t api_key=%v", includeSecrets, payload.Providers[0].APIKey)
		}
		if includeSecrets && *payload.Providers[0].APIKey != "provider-secret" {
			t.Fatalf("complete export api_key=%q", *payload.Providers[0].APIKey)
		}
		if !includeSecrets && strings.Contains(rec.Body.String(), "provider-secret") {
			t.Fatalf("safe export leaked API key: %s", rec.Body.String())
		}
	}
}

func TestProviderBundleImportLastSameNameWins(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "existing-secret")
	cfg, err := config.Load(writeAdminTestConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	existing := config.Provider{Name: "gateway", Protocol: "openai", BaseURL: "https://existing.example/v1", APIKey: "existing-gateway-secret", Models: []string{"existing-model"}, Endpoints: []string{"chat_completions"}, Priority: 10, Fallback: true}
	config.ConfigureProviderPolicy(&existing, existing.Priority, existing.Fallback)
	cfg.Providers["gateway"] = existing
	cfg, err = config.ReplaceProviders(cfg, cfg.Providers)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{cfg: cfg}
	h := NewHandler("", runtime)
	body := `{"format":"aetherrelay.provider-bundle","schema_version":1,"providers":[{"name":" Gateway ","enabled":true},{"name":"gateway","protocol":"openai","base_url":"https://new.example/v1","api_key":"new-secret","models":["new-model"],"endpoints":["chat_completions"],"priority":42,"enabled":true}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers/import", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AetherRelay-Admin", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("same-name Provider overwrite status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result providerBundleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Added != 0 || result.Updated != 1 || result.Failed != 0 || len(result.Items) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	provider := runtime.ConfigSnapshot().Providers["gateway"]
	if provider.BaseURL != "https://new.example/v1" || provider.APIKey != "new-secret" || len(provider.Models) != 1 || provider.Models[0] != "new-model" || provider.Priority != 42 {
		t.Fatalf("last same-name Provider did not win: %+v", provider)
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
