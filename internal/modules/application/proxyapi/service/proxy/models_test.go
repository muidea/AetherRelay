package proxy

import (
	"reflect"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
)

func TestModelSupportedEndpointsUsesTransportMatrix(t *testing.T) {
	snap := effectivecatalog.Snapshot{Candidates: map[string][]effectivecatalog.Candidate{
		"claude": {
			{ModelID: "claude", RouteOwner: "anthropic", SupportedEndpoints: []string{"/v1/chat/completions", "/v1/messages"}},
			{ModelID: "claude", RouteOwner: "openai", SupportedEndpoints: []string{"/v1/responses"}},
		},
	}}
	got := modelSupportedEndpoints(snap, "claude", clientaccess.All())
	want := []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"}
	if len(got) != len(want) {
		t.Fatalf("supported endpoints=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("supported endpoints=%v, want %v", got, want)
		}
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

func TestModelsProjectsMetadataForCodexOAuthDiscoveredModel(t *testing.T) {
	cfg := config.Config{ModelMetadata: map[string]config.ModelMetadata{
		"gpt-pool": {
			ID: "gpt-pool", ContextWindowTokens: 400000, MaxOutputTokens: 128000,
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
	if record.ContextWindowTokens != 400000 || record.MaxOutputTokens != 128000 || record.Capabilities == nil || record.Capabilities.Reasoning == nil || record.Capabilities.Reasoning.DefaultEffort != "none" || !reflect.DeepEqual(record.Capabilities.Reasoning.Efforts, []string{"none", "low"}) {
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
	wantEndpoints := []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"}
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
