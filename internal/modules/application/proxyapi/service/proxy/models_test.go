package proxy

import (
	"net/http"
	"net/http/httptest"
	"reflect"
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
	manifest := buildCodexModelsManifest(snapshot, clientaccess.All())
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
	if manifest := buildCodexModelsManifest(snapshot, clientaccess.All()); len(manifest.Models) != 0 {
		t.Fatalf("CP-EP-013 manifest=%#v", manifest)
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
