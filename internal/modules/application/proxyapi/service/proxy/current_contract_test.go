package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

// CP-CAP-007: client_version selects the manifest shape, not a historical capability set.
func TestModelsManifestCapabilitiesDoNotDependOnClientVersion(t *testing.T) {
	cfg := config.Config{ModelMetadata: map[string]config.ModelMetadata{
		"gpt-test": {ID: "gpt-test", ReasoningDeclared: true, ReasoningSupported: true,
			ReasoningDefaultEffort: "ultra", ReasoningEfforts: []string{"high", "max", "ultra"}},
	}}
	h := newTestProxyHandler(mustHandlerConfig(cfg), usage.NewMemoryStore(), nil, nil)
	h.ReplaceEffectiveCatalog(effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{
		Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: "gpt-test"}},
	}))
	var first string
	for _, version := range []string{"0.143.9", "0.162.1", "", "invalid"} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, newRequest(http.MethodGet, "/v1/models?client_version="+version, ""))
		if response.Code != http.StatusOK {
			t.Fatalf("version=%q status=%d body=%s", version, response.Code, response.Body.String())
		}
		if first == "" {
			first = response.Body.String()
		} else if first != response.Body.String() {
			t.Fatalf("version=%q changed manifest: %s", version, response.Body.String())
		}
	}
	var manifest CodexModelsManifest
	if err := json.Unmarshal([]byte(first), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Models) != 1 || manifest.Models[0].DefaultReasoningLevel != "ultra" || !reflect.DeepEqual(manifest.Models[0].SupportedReasoningLevels, []CodexReasoningLevelRecord{{Effort: "high"}, {Effort: "max"}, {Effort: "ultra"}}) {
		t.Fatalf("current capabilities were altered: %s", first)
	}
}

// CP-REQ-010: old top-level function fields must not silently enter another contract.
func TestCodexRequestRejectsRemovedFunctionFields(t *testing.T) {
	for _, field := range []string{`"functions":[]`, `"function_call":"auto"`, `"functions":null`, `"function_call":null`} {
		_, _, _, err := normalizeCodexRequest([]byte(`{"model":"gpt-test","input":"hello",`+field+`}`), false)
		if err == nil || !strings.Contains(err.Error(), "tools and tool_choice") {
			t.Fatalf("field=%s rejection=%v", field, err)
		}
	}
}

func TestHandlerDoesNotCreateTestClientCredentials(t *testing.T) {
	store := usage.NewMemoryStore()
	h := NewHandler(mustHandlerConfig(config.Config{}), store, nil, nil)
	records, err := store.ListClientAPIKeys(t.Context())
	if err != nil || len(records) != 0 {
		t.Fatalf("constructor changed client credentials: %v %v", records, err)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, newRequest(http.MethodGet, "/v1/models", ""))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unprovisioned test key authenticated: %d", response.Code)
	}
}
