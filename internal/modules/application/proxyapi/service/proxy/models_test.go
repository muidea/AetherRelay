package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

func TestUnsupportedCodexCompatibilityEndpointsReturnNotFound(t *testing.T) {
	handler := NewHandler(mustHandlerConfig(config.Config{}), usage.NewMemoryStore(), nil, nil)
	for _, testCase := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/v1/responses/ws"},
		{method: http.MethodGet, path: "/backend-api/codex/responses"},
		{method: http.MethodPost, path: "/backend-api/codex/responses"},
		{method: http.MethodPost, path: "/backend-api/codex/responses/compact"},
		{method: http.MethodGet, path: "/backend-api/codex/models"},
		{method: http.MethodPost, path: "/backend-api/codex/models"},
		{method: http.MethodPost, path: "/v1/models"},
	} {
		t.Run(testCase.method+" "+testCase.path, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, testCase.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusNotFound {
				t.Fatalf("CP-EP-004..006/CP-EP-011..012 unsupported compatibility endpoint status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

// CP-CAP-006: Codex manifests project default and maximum context separately.
func TestCodexModelsManifestUsesEffectiveCatalogCapabilities(t *testing.T) {
	cfg := config.Config{ModelMetadata: map[string]config.ModelMetadata{
		"gpt-codex": {
			ID: "gpt-codex", ContextWindowTokens: 400000, MaxContextWindowTokens: 921000,
			ReasoningDeclared: true, ReasoningSupported: true, ReasoningDefaultEffort: "high", ReasoningEfforts: []string{"low", "high"},
			NativeResponsesDeclared: true, NativeResponsesImages: true,
		},
	}}
	snapshot := effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{
		Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: "gpt-codex"}},
	})
	manifest := buildCodexModelsManifest(snapshot, clientaccess.All(), "")
	if len(manifest.Models) != 1 {
		t.Fatalf("CP-EP-013 models=%#v", manifest.Models)
	}
	model := manifest.Models[0]
	if model.Slug != "gpt-codex" || model.ContextWindow != 400000 || model.MaxContextWindow != 921000 || model.DefaultReasoningLevel != "high" ||
		!reflect.DeepEqual(model.SupportedReasoningLevels, []CodexReasoningLevelRecord{{Effort: "low"}, {Effort: "high"}}) ||
		!reflect.DeepEqual(model.InputModalities, []string{"text", "image"}) || !model.PreferWebsockets || model.UseResponsesLite ||
		model.BaseInstructions == "" || model.MinimalClientVersion == "" || model.Visibility != "list" || model.Priority != 1 || !model.SupportedInAPI || model.ServiceTiers == nil {
		t.Fatalf("CP-EP-013/CP-CAP-006 model=%#v", model)
	}
}

func TestCodexModelsManifestExcludesModelsWithoutResponses(t *testing.T) {
	snapshot := effectivecatalog.Snapshot{Candidates: map[string][]effectivecatalog.Candidate{
		"emb": {{ModelID: "emb", RouteOwner: "openai", SupportedEndpoints: []string{"/v1/embeddings"}}},
	}}
	if manifest := buildCodexModelsManifest(snapshot, clientaccess.All(), ""); len(manifest.Models) != 0 {
		t.Fatalf("CP-EP-013 manifest=%#v", manifest)
	}
}

// CP-CAP-007: legacy clients must not receive reasoning levels they cannot parse.
func TestCodexModelsManifestFiltersExtendedReasoningForLegacyClients(t *testing.T) {
	cfg := config.Config{ModelMetadata: map[string]config.ModelMetadata{
		"gpt-codex": {
			ID: "gpt-codex", ReasoningDeclared: true, ReasoningSupported: true,
			ReasoningDefaultEffort: "ultra", ReasoningEfforts: []string{"medium", "high", "max", "ultra"},
		},
	}}
	snapshot := effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{
		Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: "gpt-codex"}},
	})
	legacy := buildCodexModelsManifest(snapshot, clientaccess.All(), "0.143.9")
	if len(legacy.Models) != 1 || !reflect.DeepEqual(legacy.Models[0].SupportedReasoningLevels, []CodexReasoningLevelRecord{{Effort: "medium"}, {Effort: "high"}}) || legacy.Models[0].DefaultReasoningLevel != "medium" {
		t.Fatalf("legacy manifest=%#v", legacy.Models)
	}
	for _, version := range []string{"0.144.0", "v0.149.1", "unparseable", ""} {
		modern := buildCodexModelsManifest(snapshot, clientaccess.All(), version)
		if len(modern.Models) != 1 || !reflect.DeepEqual(modern.Models[0].SupportedReasoningLevels, []CodexReasoningLevelRecord{{Effort: "medium"}, {Effort: "high"}, {Effort: "max"}, {Effort: "ultra"}}) {
			t.Fatalf("modern version=%q manifest=%#v", version, modern.Models)
		}
	}
}

// CP-CAP-007: a known empty legacy result remains distinct from an unknown
// reasoning manifest while the now-invalid default is omitted.
func TestCodexModelsManifestPreservesFullyFilteredLegacyReasoning(t *testing.T) {
	cfg := config.Config{ModelMetadata: map[string]config.ModelMetadata{
		"gpt-codex": {
			ID: "gpt-codex", ReasoningDeclared: true, ReasoningSupported: true,
			ReasoningDefaultEffort: "ultra", ReasoningEfforts: []string{"max", "ultra"},
		},
	}}
	snapshot := effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{
		Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: "gpt-codex"}},
	})
	manifest := buildCodexModelsManifest(snapshot, clientaccess.All(), "0.143.9")
	if len(manifest.Models) != 1 || manifest.Models[0].DefaultReasoningLevel != "" || len(manifest.Models[0].SupportedReasoningLevels) != 0 {
		t.Fatalf("legacy manifest=%#v", manifest.Models)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "default_reasoning_level") || !strings.Contains(string(encoded), `"supported_reasoning_levels":[]`) {
		t.Fatalf("CP-CAP-007 filtered reasoning levels must be an explicit empty array: %s", encoded)
	}
}

// CP-CAP-008: known models use a trusted client manifest profile while
// transport claims remain intersected with the effective route catalog.
func TestCodexModelsManifestUsesTrustedAstraProfile(t *testing.T) {
	snapshot := effectivecatalog.BuildWithCodex(config.Config{}, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{
		Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: "gpt-6-astra"}},
	})
	manifest := buildCodexModelsManifest(snapshot, clientaccess.All(), "0.153.4")
	if len(manifest.Models) != 1 {
		t.Fatalf("models=%#v", manifest.Models)
	}
	model := manifest.Models[0]
	wantLevels := []CodexReasoningLevelRecord{
		{Effort: "low", Description: "Fast responses with lighter reasoning"},
		{Effort: "medium", Description: "Balances speed and reasoning depth for everyday tasks"},
		{Effort: "high", Description: "Greater reasoning depth for complex problems"},
		{Effort: "xhigh", Description: "Extra high reasoning depth for complex problems"},
		{Effort: "max", Description: "Maximum reasoning depth for the hardest problems"},
		{Effort: "ultra", Description: "Maximum reasoning with automatic task delegation"},
	}
	if model.DisplayName != "GPT-6-Astra" || model.Description != "Our most capable model for complex, demanding work." ||
		model.ContextWindow != 272000 || model.MaxContextWindow != 872000 || model.MinimalClientVersion != "0.153.0" ||
		!model.UseResponsesLite || !model.PreferWebsockets || !model.SupportsImageDetailOriginal || !model.SupportsSearchTool ||
		model.MultiAgentVersion != "v2" || model.MultiAgentReasoningEffort != "xhigh" || model.CompHash != "3000" || model.DefaultReasoningLevel != "medium" ||
		!reflect.DeepEqual(model.SupportedReasoningLevels, wantLevels) || !reflect.DeepEqual(model.InputModalities, []string{"text", "image"}) || len(model.ServiceTiers) != 1 {
		t.Fatalf("CP-CAP-008 model=%#v", model)
	}
	tier, ok := model.ServiceTiers[0].(map[string]any)
	if !ok || tier["id"] != "priority" || tier["name"] != "Fast" || tier["description"] != "2x speed, increased usage" {
		t.Fatalf("CP-CAP-008 service tier=%#v", model.ServiceTiers)
	}
}

func TestCodexAstraManifestHonorsExplicitMetadata(t *testing.T) {
	for _, supported := range []bool{true, false} {
		t.Run(map[bool]string{true: "restricted", false: "disabled"}[supported], func(t *testing.T) {
			metadata := config.ModelMetadata{
				ID: "gpt-6-astra", ReasoningDeclared: true, ReasoningSupported: supported,
				ReasoningDefaultEffort: "high", ReasoningEfforts: []string{"low", "high"},
				NativeResponsesDeclared: true, NativeResponsesImages: false,
				ContextWindowTokens: 64000, MaxContextWindowTokens: 128000,
			}
			if !supported {
				metadata.ReasoningDefaultEffort, metadata.ReasoningEfforts = "", nil
			}
			cfg := config.Config{ModelMetadata: map[string]config.ModelMetadata{metadata.ID: metadata}}
			snapshot := effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{
				Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: metadata.ID}},
			})
			model := buildCodexModelsManifest(snapshot, clientaccess.All(), "0.153.4").Models[0]
			if model.ContextWindow != 64000 || model.MaxContextWindow != 128000 ||
				!reflect.DeepEqual(model.InputModalities, []string{"text"}) || model.SupportsImageDetailOriginal {
				t.Fatalf("explicit capacity/image metadata ignored: %+v", model)
			}
			if model.DefaultReasoningLevel != metadata.ReasoningDefaultEffort || len(model.SupportedReasoningLevels) != len(metadata.ReasoningEfforts) || model.MultiAgentReasoningEffort != metadata.ReasoningDefaultEffort {
				t.Fatalf("explicit reasoning metadata ignored: %+v", model)
			}
			handler := NewHandler(mustHandlerConfig(cfg), usage.NewMemoryStore(), nil, nil)
			for index, level := range model.SupportedReasoningLevels {
				if level.Effort != metadata.ReasoningEfforts[index] {
					t.Fatalf("unexpected effort: %+v", level)
				}
				if err := handler.applyModelReasoning(metadata.ID, map[string]any{"reasoning": map[string]any{"effort": level.Effort}}); err != nil {
					t.Fatalf("advertised effort rejected: %v", err)
				}
			}
			if err := handler.applyModelReasoning(metadata.ID, map[string]any{"reasoning": map[string]any{"effort": "ultra"}}); err == nil {
				t.Fatal("undeclared effort accepted")
			}
		})
	}
}

// CP-EP-015: every accessible Responses route projects local preflight support.
func TestModelSupportedEndpointsUsesTransportMatrix(t *testing.T) {
	snap := effectivecatalog.Snapshot{Candidates: map[string][]effectivecatalog.Candidate{
		"claude": {
			{ModelID: "claude", RouteOwner: "anthropic", SupportedEndpoints: []string{"/v1/chat/completions", "/v1/messages"}},
			{ModelID: "claude", RouteOwner: "openai", SupportedEndpoints: []string{"/v1/responses"}},
		},
	}}
	got := modelSupportedEndpoints(snap, "claude", clientaccess.All())
	want := []string{"/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/responses/input_tokens"}
	if len(got) != len(want) {
		t.Fatalf("supported endpoints=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("supported endpoints=%v, want %v", got, want)
		}
	}
}

func TestCodexModelSupportedEndpointsIncludeAdapterEntrypoints(t *testing.T) {
	cfg := mustHandlerConfig(config.Config{})
	snap := effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: "gpt-codex"}}})
	got := modelSupportedEndpoints(snap, "gpt-codex", clientaccess.All())
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/responses/input_tokens", "/v1/responses/compact"} {
		found := false
		for _, item := range got {
			if item == path {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("CP-EP-007..008 supported endpoints=%v missing %s", got, path)
		}
	}
	response := buildModelsListResponse(snap, clientaccess.All())
	if len(response.Data) != 1 || response.Data[0].Capabilities == nil || response.Data[0].Capabilities.Codex == nil {
		t.Fatalf("CP-CAP-002/003 models=%#v", response.Data)
	}
	codex := response.Data[0].Capabilities.Codex
	if codex.Compact != "supported" || codex.Websocket != "supported" || codex.FunctionTools != "unknown" || codex.ParallelTools != "unknown" || codex.ImageInput != "unknown" {
		t.Fatalf("CP-CAP-002/003 codex=%#v", codex)
	}
}

func TestModelSupportedEndpointsIncludesChatGPTWebSearchAndImages(t *testing.T) {
	snap := effectivecatalog.Snapshot{Candidates: map[string][]effectivecatalog.Candidate{
		"gpt": {{ModelID: "gpt", RouteOwner: effectivecatalog.BuiltinProviderID, Builtin: true, SupportedEndpoints: []string{"/v1/chat/completions", "/v1/responses", "/v1/search", "/v1/images/generations", "/v1/images/edits"}}},
	}}
	got := modelSupportedEndpoints(snap, "gpt", clientaccess.All())
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/search", "/v1/images/generations", "/v1/images/edits"} {
		found := false
		for _, item := range got {
			if item == path {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("supported endpoints=%v missing %s", got, path)
		}
	}
}

func TestModelsProjectsDegradedReasoningConversion(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"anthropic": {
				Name: "anthropic", Protocol: "anthropic", BaseURL: "https://example.invalid", APIKey: "test",
				Models: []string{"claude-test"}, Endpoints: []string{config.ProviderEndpointMessages},
			},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"claude-test": {
				ID: "claude-test", ReasoningDeclared: true, ReasoningSupported: true, ReasoningEfforts: []string{"low"},
				ConversionCapabilities: map[string]config.ConversionCapability{
					config.ProviderEndpointMessages: {
						Level: 2, Text: true, Streaming: true, Reasoning: true,
						ReasoningAdapter: config.ReasoningAdapterResponsesToAnthropicAdaptive, ReasoningTargetEffort: "low",
					},
				},
			},
		},
	}
	response := buildModelsListResponse(effectivecatalog.Build(cfg, 0, 0, nil, ""), clientaccess.All())
	if len(response.Data) != 1 || response.Data[0].Capabilities == nil || response.Data[0].Capabilities.Conversions == nil {
		t.Fatalf("models = %#v", response)
	}
	capability := response.Data[0].Capabilities.Conversions.ResponsesToAnthropic
	if capability == nil || !capability.Reasoning || capability.ReasoningMode != "degrade" {
		t.Fatalf("conversion capability = %#v", capability)
	}
}

// CP-CAP-006: OpenAI-compatible model records retain both context capacities.
func TestModelsProjectsMetadataForCodexOAuthDiscoveredModel(t *testing.T) {
	cfg := config.Config{ModelMetadata: map[string]config.ModelMetadata{
		"gpt-pool": {
			ID: "gpt-pool", ContextWindowTokens: 400000, MaxContextWindowTokens: 921000, MaxOutputTokens: 128000,
			ReasoningDeclared: true, ReasoningSupported: true, ReasoningDefaultEffort: "none", ReasoningEfforts: []string{"none", "low"},
		},
	}}
	snapshot := effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{
		Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: "gpt-pool"}},
	})
	response := buildModelsListResponse(snapshot, clientaccess.All())
	if len(response.Data) != 1 || response.Data[0].ID != "gpt-pool" {
		t.Fatalf("models=%#v", response.Data)
	}
	record := response.Data[0]
	if record.ContextWindowTokens != 400000 || record.MaxContextWindowTokens != 921000 || record.MaxOutputTokens != 128000 || record.Capabilities == nil || record.Capabilities.Reasoning == nil || record.Capabilities.Reasoning.DefaultEffort != "none" || !reflect.DeepEqual(record.Capabilities.Reasoning.Efforts, []string{"none", "low"}) {
		t.Fatalf("Codex OAuth metadata=%#v", record)
	}
}

func TestModelsOnlyProjectsConversionDirectionsWithEligibleProviders(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"responses": {
				Name: "responses", Protocol: "openai", BaseURL: "https://example.invalid", APIKey: "test",
				Models: []string{"shared-model"}, Endpoints: []string{config.ProviderEndpointChatCompletions, config.ProviderEndpointResponses},
			},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"shared-model": {ID: "shared-model", ConversionCapabilities: map[string]config.ConversionCapability{
				config.ProviderEndpointMessages:  {Level: 1, Text: true},
				config.ProviderEndpointResponses: {Level: 1, Text: true},
			}},
		},
	}
	response := buildModelsListResponse(effectivecatalog.Build(cfg, 0, 0, nil, ""), clientaccess.All())
	if len(response.Data) != 1 || response.Data[0].Capabilities == nil || response.Data[0].Capabilities.Conversions == nil {
		t.Fatalf("models = %#v", response)
	}
	conversions := response.Data[0].Capabilities.Conversions
	if conversions.AnthropicToResponses == nil {
		t.Fatalf("anthropic_to_responses was not projected: %#v", conversions)
	}
	if conversions.ResponsesToAnthropic != nil {
		t.Fatalf("responses_to_anthropic projected without an Anthropic provider: %#v", conversions)
	}
	wantEndpoints := []string{"/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/responses/input_tokens"}
	if !reflect.DeepEqual(response.Data[0].SupportedEndpoints, wantEndpoints) {
		t.Fatalf("supported endpoints = %v, want %v", response.Data[0].SupportedEndpoints, wantEndpoints)
	}
}

func TestModelsAreScopedToAuthorizedProviders(t *testing.T) {
	snap := effectivecatalog.Snapshot{
		Candidates: map[string][]effectivecatalog.Candidate{
			"shared": {
				{ModelID: "shared", RouteOwner: "primary", ContextWindowTokens: 100, SupportedEndpoints: []string{"/v1/responses"}, ConversionModes: []string{"anthropic_to_responses"}},
				{ModelID: "shared", RouteOwner: "backup", ContextWindowTokens: 50, SupportedEndpoints: []string{"/v1/chat/completions"}},
			},
			"private": {{ModelID: "private", RouteOwner: "primary", SupportedEndpoints: []string{"/v1/responses"}}},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"shared": {ID: "shared", ConversionCapabilities: map[string]config.ConversionCapability{
				config.ProviderEndpointResponses: {Level: 1, Text: true},
			}},
		},
	}
	policy, err := clientaccess.Selected([]string{"backup"})
	if err != nil {
		t.Fatal(err)
	}
	response := buildModelsListResponse(snap, policy)
	if len(response.Data) != 1 || response.Data[0].ID != "shared" {
		t.Fatalf("models=%#v", response.Data)
	}
	if !reflect.DeepEqual(response.Data[0].SupportedEndpoints, []string{"/v1/chat/completions"}) {
		t.Fatalf("endpoints=%v", response.Data[0].SupportedEndpoints)
	}
	if response.Data[0].ContextWindowTokens != 50 {
		t.Fatalf("context window=%d", response.Data[0].ContextWindowTokens)
	}
	if response.Data[0].Capabilities != nil && response.Data[0].Capabilities.Conversions != nil {
		t.Fatalf("unauthorized provider contributed conversion=%#v", response.Data[0].Capabilities.Conversions)
	}
}
