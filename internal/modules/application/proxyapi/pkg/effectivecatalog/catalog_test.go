package effectivecatalog

import (
	"os"
	"path/filepath"
	"testing"

	"ai-proxy/internal/pkg/aiproxyconfig"
)

func TestBuildOmitsRoutingDisabledBuiltinProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `
chatgpt_web:
  enabled: true
  provider_enabled: false
  priority: 240
providers:
  static:
    enabled: true
    protocol: openai
    base_url: http://127.0.0.1:8081
    allow_unauthenticated: true
    endpoints: chat_completions
    models: static-model
model_metadata:
  static-model:
    context_window_tokens: 8192
    max_output_tokens: 1024
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	snap := Build(cfg, 1, 1, []PoolModel{{ID: "gpt-5"}}, "2026-08-03T00:00:00Z")
	if snap.BuiltinProvider.Enabled || snap.BuiltinProvider.Status != StatusDisabled {
		t.Fatalf("builtin provider=%+v", snap.BuiltinProvider)
	}
	if candidates := snap.CandidatesFor("gpt-5"); len(candidates) != 0 {
		t.Fatalf("disabled builtin candidates=%+v", candidates)
	}
}

func TestBuildUsesConfiguredBuiltinPriority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `
codex_oauth:
  enabled: true
  provider_enabled: true
  priority: 240
providers:
  static:
    enabled: true
    protocol: openai
    base_url: http://127.0.0.1:8081
    allow_unauthenticated: true
    endpoints: chat_completions
    models: gpt-5.3-codex
model_metadata:
  gpt-5.3-codex:
    context_window_tokens: 8192
    max_output_tokens: 1024
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	snap := BuildWithCodex(cfg, CatalogInput{}, CatalogInput{Version: 1, AvailableAccounts: 1, Models: []PoolModel{{ID: "gpt-5.3-codex"}}})
	candidates := snap.CandidatesFor("gpt-5.3-codex")
	if len(candidates) != 2 || candidates[0].RouteOwner != CodexOAuthProviderID || candidates[0].Priority != 240 {
		t.Fatalf("candidates=%+v", candidates)
	}
}

func TestBuildIncludesStaticAndBuiltinCandidatesForSameModel(t *testing.T) {
	cfg := config.Config{
		ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true},
		ModelMetadata: map[string]config.ModelMetadata{
			"gpt-4o": {
				ID:                  "gpt-4o",
				ContextWindowTokens: 128000, MaxOutputTokens: 16384,
			},
		},
		Providers: map[string]config.Provider{
			"openai": {Name: "openai", Protocol: "openai", Models: []string{"gpt-4o"}, Disabled: false, Endpoints: []string{config.ProviderEndpointChatCompletions}},
		},
	}
	snap := Build(cfg, 3, 2, []PoolModel{
		{ID: "gpt-4o"},
		{ID: "gpt-5"},
		{ID: "gpt-image-2"},
	}, "2026-07-26T00:00:00Z")

	if snap.BuiltinProvider.Status != StatusReady {
		t.Fatalf("status=%s want ready", snap.BuiltinProvider.Status)
	}
	if snap.BuiltinProvider.ConflictCount != 1 || len(snap.BuiltinProvider.ConflictModels) != 1 || snap.BuiltinProvider.ConflictModels[0] != "gpt-4o" {
		t.Fatalf("conflicts=%+v", snap.BuiltinProvider.ConflictModels)
	}
	if model, ok := snap.BuiltinModels["gpt-4o"]; !ok || !model.ConflictWithStatic {
		t.Fatal("conflicting builtin model must remain visible as a route candidate")
	}
	route, ok := snap.Lookup("gpt-4o")
	if !ok || route.Builtin || route.RouteOwner != "openai" {
		t.Fatalf("static route=%+v ok=%v", route, ok)
	}
	candidates := snap.CandidatesFor("gpt-4o")
	if len(candidates) != 2 || candidates[0].RouteOwner != "openai" || candidates[1].RouteOwner != BuiltinProviderID {
		t.Fatalf("gpt-4o candidates=%+v", candidates)
	}
	if candidates[0].Priority != config.DefaultProviderPriority || candidates[1].Priority != ChatGPTWebPriority || candidates[1].Fallback {
		t.Fatalf("candidate policy=%+v", candidates)
	}
	route, ok = snap.Lookup("gpt-5")
	if !ok || !route.Builtin || route.RouteOwner != BuiltinProviderID {
		t.Fatalf("builtin route=%+v ok=%v", route, ok)
	}
	ids := snap.SortedModelIDs()
	if len(ids) != 3 {
		t.Fatalf("ids=%v", ids)
	}
}

func TestMetadataDoesNotPublishModel(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"openai": {Name: "openai", Protocol: "openai", Models: []string{"gpt-*"}, Endpoints: []string{config.ProviderEndpointChatCompletions}},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"gpt-ghost": {ID: "gpt-ghost", ContextWindowTokens: 128000, MaxOutputTokens: 16384},
		},
	}
	snap := FromStatic(cfg)
	if _, ok := snap.Lookup("gpt-ghost"); ok || len(snap.SortedModelIDs()) != 0 {
		t.Fatalf("metadata-only model was published: %+v", snap.CandidatesFor("gpt-ghost"))
	}
}

func TestMetadataEnrichesDiscoveredModelWithoutCreatingStaticCandidate(t *testing.T) {
	cfg := config.Config{
		ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true},
		ModelMetadata: map[string]config.ModelMetadata{
			"gpt-5": {ID: "gpt-5", ContextWindowTokens: 400000, MaxOutputTokens: 128000},
		},
	}
	snap := Build(cfg, 1, 1, []PoolModel{{ID: "gpt-5"}}, "2026-08-05T00:00:00Z")
	route, ok := snap.Lookup("gpt-5")
	if !ok || !route.Builtin || route.RouteOwner != BuiltinProviderID {
		t.Fatalf("route = %+v, ok = %t", route, ok)
	}
	if route.ContextWindowTokens != 400000 || route.MaxOutputTokens != 128000 {
		t.Fatalf("metadata was not applied: %+v", route)
	}
	if candidates := snap.CandidatesFor("gpt-5"); len(candidates) != 1 || !candidates[0].Builtin {
		t.Fatalf("metadata created an extra candidate: %+v", candidates)
	}
}

func TestReconfigurePreservesBuiltinModelsAcrossStaticConfigUpdate(t *testing.T) {
	initial := Build(config.Config{ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true}}, 4, 1, []PoolModel{{
		ID: "gpt-5",
	}}, "2026-07-26T00:00:00Z")
	updated := Reconfigure(config.Config{
		ChatGPTWeb:    config.ChatGPTWebConfig{Enabled: true},
		ModelMetadata: map[string]config.ModelMetadata{"gpt-5": {ID: "gpt-5"}},
		Providers: map[string]config.Provider{
			"openai": {Name: "openai", Protocol: "openai", Models: []string{"gpt-5"}, Endpoints: []string{config.ProviderEndpointChatCompletions}},
		},
	}, initial)
	if route, ok := updated.Lookup("gpt-5"); !ok || route.Builtin || route.RouteOwner != "openai" {
		t.Fatalf("static route after reconfigure=%+v ok=%v", route, ok)
	}
	if updated.BuiltinProvider.ConflictCount != 1 || updated.BuiltinProvider.ModelCount != 1 {
		t.Fatalf("reconfigured provider=%+v", updated.BuiltinProvider)
	}
	candidates := updated.CandidatesFor("gpt-5")
	if len(candidates) != 2 || candidates[0].RouteOwner != "openai" || candidates[1].RouteOwner != BuiltinProviderID {
		t.Fatalf("reconfigured candidates=%+v", candidates)
	}
}

func TestBuildDisabledAndEmptyStates(t *testing.T) {
	off := Build(config.Config{ChatGPTWeb: config.ChatGPTWebConfig{Enabled: false}}, 0, 0, nil, "")
	if off.BuiltinProvider.Status != StatusDisabled {
		t.Fatalf("disabled status=%s", off.BuiltinProvider.Status)
	}
	onEmpty := Build(config.Config{ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true}}, 0, 0, nil, "")
	if onEmpty.BuiltinProvider.Status != StatusEmpty && onEmpty.BuiltinProvider.Status != StatusDiscovering {
		t.Fatalf("empty status=%s", onEmpty.BuiltinProvider.Status)
	}
	discovering := Build(config.Config{ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true}}, 0, 1, nil, "")
	if discovering.BuiltinProvider.Status != StatusDiscovering {
		t.Fatalf("discovering status=%s", discovering.BuiltinProvider.Status)
	}
	emptyWithStaleModel := Build(config.Config{ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true}}, 3, 0, []PoolModel{{ID: "stale"}}, "")
	if _, ok := emptyWithStaleModel.Lookup("stale"); ok || len(emptyWithStaleModel.CandidatesFor("stale")) != 0 {
		t.Fatalf("unavailable account-pool models must not be routable: %+v", emptyWithStaleModel)
	}
}

func TestBuiltinProviderViewEndpoints(t *testing.T) {
	provider := BuiltinProviderView()
	if provider.Protocol != BuiltinProviderID || len(provider.Endpoints) != 3 {
		t.Fatalf("provider=%+v", provider)
	}
}

func TestBuildCodexOAuthUsesDiscoveredModelsAndStaticConflictRule(t *testing.T) {
	cfg := config.Config{
		CodexOAuth: config.CodexOAuthConfig{Enabled: true},
		ModelMetadata: map[string]config.ModelMetadata{
			"gpt-5.2": {ID: "gpt-5.2"},
		},
		Providers: map[string]config.Provider{
			"static": {Name: "static", Protocol: "openai", Models: []string{"gpt-5.2"}, Endpoints: []string{config.ProviderEndpointResponses}},
		},
	}
	snap := BuildWithCodex(cfg, CatalogInput{}, CatalogInput{Version: 1, AvailableAccounts: 1, Models: []PoolModel{{ID: "gpt-5.2"}, {ID: "gpt-5.2-codex"}}})
	if snap.CodexOAuthProvider.Status != StatusReady || snap.CodexOAuthProvider.ConflictCount != 1 {
		t.Fatalf("Codex provider=%+v", snap.CodexOAuthProvider)
	}
	if route, ok := snap.Lookup("gpt-5.2"); !ok || route.RouteOwner != "static" || route.Builtin {
		t.Fatalf("static conflict route=%+v ok=%v", route, ok)
	}
	candidates := snap.CandidatesFor("gpt-5.2")
	if len(candidates) != 2 || candidates[0].RouteOwner != "static" || candidates[1].RouteOwner != CodexOAuthProviderID {
		t.Fatalf("same-model candidates=%+v", candidates)
	}
	if route, ok := snap.Lookup("gpt-5.2-codex"); !ok || route.RouteOwner != CodexOAuthProviderID || !route.Builtin {
		t.Fatalf("Codex route=%+v ok=%v", route, ok)
	}
	provider := BuiltinProviderViewFor(CodexOAuthProviderID)
	if provider.Protocol != CodexOAuthProviderID || len(provider.Endpoints) != 1 || provider.Endpoints[0] != config.ProviderEndpointResponses {
		t.Fatalf("Codex synthetic provider=%+v", provider)
	}
}

func TestBuildCodexOAuthPublishesAllDiscoveredModels(t *testing.T) {
	cfg := config.Config{CodexOAuth: config.CodexOAuthConfig{Enabled: true}}
	snap := BuildWithCodex(cfg, CatalogInput{}, CatalogInput{Version: 4, AvailableAccounts: 2, UpdatedAt: "2026-07-30T00:00:00Z", Models: []PoolModel{{ID: "gpt-5.3-codex", OwnedBy: "openai"}, {ID: "gpt-5.3-codex-mini", OwnedBy: "openai"}}})
	if snap.CodexOAuthProvider.Status != StatusReady || snap.CodexOAuthProvider.ModelCount != 2 || snap.CodexOAuthVersion != 4 {
		t.Fatalf("Codex provider=%+v version=%d", snap.CodexOAuthProvider, snap.CodexOAuthVersion)
	}
	if route, ok := snap.Lookup("gpt-5.3-codex"); !ok || route.RouteOwner != CodexOAuthProviderID || route.OwnedBy != "openai" {
		t.Fatalf("Codex discovered route=%+v ok=%v", route, ok)
	}
	if route, ok := snap.Lookup("gpt-5.3-codex-mini"); !ok || route.RouteOwner != CodexOAuthProviderID || route.OwnedBy != "openai" {
		t.Fatalf("Codex discovered route=%+v ok=%v", route, ok)
	}
}
