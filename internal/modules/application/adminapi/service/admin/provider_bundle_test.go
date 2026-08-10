package admin

import (
	"encoding/json"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	config "ai-proxy/internal/pkg/aiproxyconfig"
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
		req.Header.Set("X-AI-Proxy-Admin", "1")
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

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
