package effectivecatalog

import (
	"testing"

	"ai-proxy/internal/pkg/aiproxyconfig"
)

func TestBuildStaticWinsOverBuiltinAndOmitsConflicts(t *testing.T) {
	cfg := config.Config{
		ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-4o": {
				ID: "gpt-4o", RouteOwner: "openai", Operations: []string{"chat_completions"},
				ContextWindowTokens: 128000, MaxOutputTokens: 16384,
			},
		},
		Providers: map[string]config.Provider{
			"openai": {Name: "openai", Protocol: "openai", Disabled: false},
		},
	}
	snap := Build(cfg, 3, 2, []PoolModel{
		{ID: "gpt-4o", Operations: []string{"chat_completions", "image_generations"}},
		{ID: "gpt-5", Operations: []string{"chat_completions"}},
		{ID: "gpt-image-2", Operations: []string{"chat_completions", "image_generations"}},
	}, "2026-07-26T00:00:00Z")

	if snap.BuiltinProvider.Status != StatusDegraded {
		t.Fatalf("status=%s want degraded", snap.BuiltinProvider.Status)
	}
	if snap.BuiltinProvider.ConflictCount != 1 || len(snap.BuiltinProvider.ConflictModels) != 1 || snap.BuiltinProvider.ConflictModels[0] != "gpt-4o" {
		t.Fatalf("conflicts=%+v", snap.BuiltinProvider.ConflictModels)
	}
	if model, ok := snap.BuiltinModels["gpt-4o"]; !ok || !model.ConflictWithStatic {
		t.Fatal("conflicting builtin model must be retained as a non-routable conflict")
	}
	route, ok := snap.Lookup("gpt-4o")
	if !ok || route.Builtin || route.RouteOwner != "openai" {
		t.Fatalf("static route=%+v ok=%v", route, ok)
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

func TestReconfigurePreservesBuiltinModelsAcrossStaticConfigUpdate(t *testing.T) {
	initial := Build(config.Config{ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true}}, 4, 1, []PoolModel{{
		ID: "gpt-5", Operations: []string{"chat_completions"},
	}}, "2026-07-26T00:00:00Z")
	updated := Reconfigure(config.Config{
		ChatGPTWeb:   config.ChatGPTWebConfig{Enabled: true},
		ModelCatalog: map[string]config.ModelInfo{"gpt-5": {ID: "gpt-5", RouteOwner: "openai", Operations: []string{"chat_completions"}}},
	}, initial)
	if route, ok := updated.Lookup("gpt-5"); !ok || route.Builtin || route.RouteOwner != "openai" {
		t.Fatalf("static route after reconfigure=%+v ok=%v", route, ok)
	}
	if updated.BuiltinProvider.ConflictCount != 1 || updated.BuiltinProvider.ModelCount != 0 {
		t.Fatalf("reconfigured provider=%+v", updated.BuiltinProvider)
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
}

func TestBuiltinProviderViewCapabilities(t *testing.T) {
	provider := BuiltinProviderView()
	if provider.Protocol != BuiltinProviderID || len(provider.EndpointCapabilities) != 3 {
		t.Fatalf("provider=%+v", provider)
	}
}
