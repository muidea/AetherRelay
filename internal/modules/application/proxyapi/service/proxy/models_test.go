package proxy

import (
	"reflect"
	"testing"

	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	config "ai-proxy/internal/pkg/aiproxyconfig"
)

func TestModelSupportedEndpointsUsesTransportMatrix(t *testing.T) {
	snap := effectivecatalog.Snapshot{Candidates: map[string][]effectivecatalog.Candidate{
		"claude": {
			{ModelID: "claude", RouteOwner: "anthropic", SupportedEndpoints: []string{"/v1/chat/completions", "/v1/messages"}},
			{ModelID: "claude", RouteOwner: "openai", SupportedEndpoints: []string{"/v1/responses"}},
		},
	}}
	got := modelSupportedEndpoints(snap, "claude")
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
	got := modelSupportedEndpoints(snap, "gpt")
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
				ConversionReleases: map[string]map[string]config.ProviderConversionRelease{"claude-test": {TransportModeResponsesToAnthropic: {Enabled: true, Verified: true}}},
			},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"claude-test": {
				ID: "claude-test", ReasoningDeclared: true, ReasoningSupported: true, ReasoningEfforts: []string{"low"},
				ConversionCapabilities: map[string]config.ConversionCapability{
					TransportModeResponsesToAnthropic: {
						Level: 2, Text: true, Streaming: true, Reasoning: true,
						ReasoningAdapter: config.ReasoningAdapterResponsesToAnthropicAdaptive, ReasoningTargetEffort: "low",
					},
				},
			},
		},
	}
	response := buildModelsListResponse(effectivecatalog.Build(cfg, 0, 0, nil, ""))
	if len(response.Data) != 1 || response.Data[0].Capabilities == nil || response.Data[0].Capabilities.Conversions == nil {
		t.Fatalf("models = %#v", response)
	}
	capability := response.Data[0].Capabilities.Conversions.ResponsesToAnthropic
	if capability == nil || !capability.Reasoning || capability.ReasoningMode != "degrade" {
		t.Fatalf("conversion capability = %#v", capability)
	}
}

func TestModelsOnlyProjectsConversionDirectionsWithEligibleProviders(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"responses": {
				Name: "responses", Protocol: "openai", BaseURL: "https://example.invalid", APIKey: "test",
				Models: []string{"shared-model"}, Endpoints: []string{config.ProviderEndpointChatCompletions, config.ProviderEndpointResponses},
				ConversionReleases: map[string]map[string]config.ProviderConversionRelease{"shared-model": {TransportModeAnthropicToResponses: {Enabled: true, Verified: true}}},
			},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"shared-model": {ID: "shared-model", ConversionCapabilities: map[string]config.ConversionCapability{
				TransportModeResponsesToAnthropic: {Level: 1, Text: true},
				TransportModeAnthropicToResponses: {Level: 1, Text: true},
			}},
		},
	}
	response := buildModelsListResponse(effectivecatalog.Build(cfg, 0, 0, nil, ""))
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
