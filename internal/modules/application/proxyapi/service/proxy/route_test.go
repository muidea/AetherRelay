package proxy

import (
	"net/http"
	"strings"
	"testing"

	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"ai-proxy/internal/pkg/aiproxyconfig"
)

func testRouteConfig() config.Config {
	return config.Config{
		Providers: map[string]config.Provider{
			"openai-full": {
				Name:     "openai-full",
				Protocol: "openai",
				BaseURL:  "https://openai.test/v1",
				APIKey:   "k",
				Models:   []string{"gpt-*", "gpt-test", "text-embedding-*", "text-embedding-test"},
				Endpoints: []string{
					config.ProviderEndpointChatCompletions,
					config.ProviderEndpointResponses,
					config.ProviderEndpointCompletions,
					config.ProviderEndpointEmbeddings,
				},
			},
			"openai-chat-only": {
				Name:      "openai-chat-only",
				Protocol:  "openai",
				BaseURL:   "https://openai-chat.test/v1",
				APIKey:    "k",
				Models:    []string{"chat-only-*", "chat-only-model"},
				Endpoints: []string{config.ProviderEndpointChatCompletions},
			},
			"openai-responses-only": {
				Name: "openai-responses-only", Protocol: "openai", BaseURL: "https://responses.test/v1", APIKey: "k",
				Models: []string{"responses-only-model"}, Endpoints: []string{config.ProviderEndpointResponses},
			},
			"anthropic": {
				Name:      "anthropic",
				Protocol:  "anthropic",
				BaseURL:   "https://anthropic.test",
				APIKey:    "k",
				Models:    []string{"claude-*", "claude-test", "claude-level2", "claude-level3", "claude-tools"},
				Endpoints: []string{config.ProviderEndpointMessages},
			},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"gpt-test": {
				ID: "gpt-test", ContextWindowTokens: 128000, MaxOutputTokens: 4096,
			},
			"text-embedding-test": {
				ID: "text-embedding-test", ContextWindowTokens: 8192, MaxOutputTokens: 8191,
			},
			"chat-only-model": {
				ID: "chat-only-model", ContextWindowTokens: 32000, MaxOutputTokens: 4096,
			},
			"responses-only-model": {
				ID: "responses-only-model", ContextWindowTokens: 128000, MaxOutputTokens: 4096,
				ConversionCapabilities: map[string]config.ConversionCapability{config.ProviderEndpointResponses: {Level: 1, Text: true}},
			},
			"claude-test": {
				ID: "claude-test", ContextWindowTokens: 200000, MaxOutputTokens: 8192,
				ConversionCapabilities: map[string]config.ConversionCapability{config.ProviderEndpointMessages: {Level: 1, Text: true}},
			},
			"claude-level2": {
				ID: "claude-level2", ConversionCapabilities: map[string]config.ConversionCapability{config.ProviderEndpointMessages: {Level: 2, Text: true, Streaming: true}},
			},
			"claude-level3": {
				ID: "claude-level3", ConversionCapabilities: map[string]config.ConversionCapability{config.ProviderEndpointMessages: {Level: 3, Text: true, Tools: true, Images: true}},
			},
			"claude-tools": {
				ID: "claude-tools", ConversionCapabilities: map[string]config.ConversionCapability{config.ProviderEndpointMessages: {Level: 3, Text: true, Tools: true, Streaming: true}},
			},
		},
	}
}

func TestResolveTransportPlanMatrix(t *testing.T) {
	cfg := testRouteConfig()
	cases := []struct {
		name             string
		path             string
		model            string
		wantMode         string
		wantUpstreamPath string
		wantOwner        string
		wantCode         string
	}{
		{
			name: "openai chat native",
			path: "/v1/chat/completions", model: "gpt-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/chat/completions", wantOwner: "openai-full",
		},
		{
			name: "openai chat to anthropic conversion",
			path: "/v1/chat/completions", model: "claude-test",
			wantMode: TransportModeOpenAIToAnthropic, wantUpstreamPath: "/v1/messages", wantOwner: "anthropic",
		},
		{
			name: "anthropic messages native",
			path: "/v1/messages", model: "claude-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/messages", wantOwner: "anthropic",
		},
		{
			name: "anthropic messages to openai conversion",
			path: "/v1/messages", model: "gpt-test",
			wantMode: TransportModeAnthropicToOpenAI, wantUpstreamPath: "/v1/chat/completions", wantOwner: "openai-full",
		},
		{
			name: "anthropic messages to responses is not implemented",
			path: "/v1/messages", model: "responses-only-model",
			wantMode: TransportModeAnthropicToResponses, wantUpstreamPath: "/v1/responses", wantOwner: "openai-responses-only",
		},
		{
			name: "responses native",
			path: "/v1/responses", model: "gpt-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/responses", wantOwner: "openai-full",
		},
		{
			name: "completions native",
			path: "/v1/completions", model: "gpt-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/completions", wantOwner: "openai-full",
		},
		{
			name: "embeddings native",
			path: "/v1/embeddings", model: "text-embedding-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/embeddings", wantOwner: "openai-full",
		},
		{
			name: "responses not available on chat-only endpoint",
			path: "/v1/responses", model: "chat-only-model",
			// RouteOwner 只声明 chat_completions，没有 responses endpoint。
			wantCode: ErrorCodeEndpointUnsupported,
		},
		{
			name: "embeddings not available on chat-only endpoint",
			path: "/v1/embeddings", model: "chat-only-model",
			wantCode: ErrorCodeEndpointUnsupported,
		},
		{
			name: "embeddings not available via anthropic conversion",
			path: "/v1/embeddings", model: "claude-test",
			wantCode: ErrorCodeEndpointUnsupported,
		},
		{
			name: "responses not available via anthropic",
			path: "/v1/responses", model: "claude-test",
			wantMode: TransportModeResponsesToAnthropic, wantUpstreamPath: "/v1/messages", wantOwner: "anthropic",
		},
		{
			name: "level 2 text SSE conversion",
			path: "/v1/responses", model: "claude-level2", wantMode: TransportModeResponsesToAnthropic, wantUpstreamPath: "/v1/messages", wantOwner: "anthropic",
		},
		{
			name: "level 3 with unsupported images remains closed",
			path: "/v1/responses", model: "claude-level3", wantCode: ErrorCodeEndpointUnsupported,
		},
		{
			name: "level 3 function tools conversion",
			path: "/v1/responses", model: "claude-tools", wantMode: TransportModeResponsesToAnthropic, wantUpstreamPath: "/v1/messages", wantOwner: "anthropic",
		},
		{
			name: "model required",
			path: "/v1/chat/completions", model: "",
			wantCode: ErrorCodeModelRequired,
		},
		{
			name: "model not found",
			path: "/v1/chat/completions", model: "missing-model",
			wantCode: ErrorCodeModelNotFound,
		},
		{
			name: "provider endpoint determines model serviceability",
			path: "/v1/chat/completions", model: "text-embedding-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/chat/completions", wantOwner: "openai-full",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, apiErr := ResolveTransportPlan(cfg, effectivecatalog.FromStatic(cfg), http.MethodPost, tc.path, tc.model)
			if tc.wantCode != "" {
				if apiErr == nil {
					t.Fatalf("expected error %s, got plan %#v", tc.wantCode, plan)
				}
				if apiErr.Code != tc.wantCode {
					t.Fatalf("code = %q want %q msg=%s", apiErr.Code, tc.wantCode, apiErr.Message)
				}
				return
			}
			if apiErr != nil {
				t.Fatalf("unexpected error: %#v", apiErr)
			}
			if plan.Mode != tc.wantMode {
				t.Fatalf("mode = %q want %q", plan.Mode, tc.wantMode)
			}
			if plan.UpstreamEndpoint != tc.wantUpstreamPath {
				t.Fatalf("upstream = %q want %q", plan.UpstreamEndpoint, tc.wantUpstreamPath)
			}
			if plan.RouteOwner != tc.wantOwner {
				t.Fatalf("owner = %q want %q", plan.RouteOwner, tc.wantOwner)
			}
			if plan.ClientEndpoint != strings.TrimRight(tc.path, "/") {
				t.Fatalf("client endpoint = %q", plan.ClientEndpoint)
			}
			if plan.ModelID != tc.model {
				t.Fatalf("model = %q", plan.ModelID)
			}
		})
	}
}

func TestResolveTransportPlansOrdersCandidatesAndHonorsFallbackPolicy(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"primary": {
				Name: "primary", Protocol: "openai", BaseURL: "https://primary.test", APIKey: "k", Models: []string{"shared"}, Priority: 200,
				Endpoints: []string{config.ProviderEndpointChatCompletions},
			},
			"backup": {
				Name: "backup", Protocol: "openai", BaseURL: "https://backup.test", APIKey: "k", Models: []string{"shared"}, Priority: 20,
				Endpoints: []string{config.ProviderEndpointChatCompletions},
			},
			"standby-disabled": {
				Name: "standby-disabled", Protocol: "openai", BaseURL: "https://disabled.test", APIKey: "k", Models: []string{"shared"}, Priority: 10,
				Endpoints: []string{config.ProviderEndpointChatCompletions},
			},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"shared": {
				ID: "shared", ContextWindowTokens: 8192, MaxOutputTokens: 4096,
			},
		},
	}
	plans, apiErr := ResolveTransportPlans(cfg, effectivecatalog.FromStatic(cfg), http.MethodPost, "/v1/chat/completions", "shared")
	if apiErr != nil {
		t.Fatalf("ResolveTransportPlans: %#v", apiErr)
	}
	if len(plans) != 3 {
		t.Fatalf("plans=%+v", plans)
	}
	for index, want := range []string{"primary", "backup", "standby-disabled"} {
		if plans[index].RouteOwner != want {
			t.Fatalf("plan[%d]=%q want %q", index, plans[index].RouteOwner, want)
		}
	}
	if plans[0].Priority != 200 || plans[1].Priority != 20 || plans[2].Priority != 10 {
		t.Fatalf("priorities=%+v", plans)
	}
}

func TestValidateConversionRequestRejectsFeatures(t *testing.T) {
	plan := TransportPlan{
		ModelID:          "claude-test",
		RouteOwner:       "anthropic",
		ClientProtocol:   ClientProtocolOpenAI,
		ClientEndpoint:   "/v1/chat/completions",
		UpstreamProtocol: "anthropic",
		UpstreamEndpoint: "/v1/messages",
		Mode:             TransportModeOpenAIToAnthropic,
	}

	cases := []struct {
		name    string
		body    map[string]any
		feature string
	}{
		{
			name: "tools",
			body: map[string]any{
				"model": "claude-test",
				"messages": []any{
					map[string]any{"role": "user", "content": "hi"},
				},
				"tools": []any{map[string]any{"type": "function"}},
			},
			feature: "tools",
		},
		{
			name: "response_format",
			body: map[string]any{
				"model":           "claude-test",
				"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
				"response_format": map[string]any{"type": "json_object"},
			},
			feature: "response_format",
		},
		{
			name: "image content",
			body: map[string]any{
				"model": "claude-test",
				"messages": []any{
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x"}},
						},
					},
				},
			},
			feature: "image_url",
		},
		{
			name: "tool role",
			body: map[string]any{
				"model": "claude-test",
				"messages": []any{
					map[string]any{"role": "tool", "content": "result"},
				},
			},
			feature: "tool role",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apiErr := ValidateConversionRequest(plan, tc.body)
			if apiErr == nil {
				t.Fatal("expected conversion_unsupported")
			}
			if apiErr.Code != ErrorCodeConversionUnsupported {
				t.Fatalf("code = %q", apiErr.Code)
			}
			if apiErr.Feature == "" && !strings.Contains(apiErr.Message, tc.feature) {
				t.Fatalf("feature/message missing %q: feature=%q msg=%s", tc.feature, apiErr.Feature, apiErr.Message)
			}
			if len(apiErr.UnsupportedFeatures) != 1 || apiErr.UnsupportedFeatures[0] == "" {
				t.Fatalf("unsupported_features = %#v, want one bounded feature", apiErr.UnsupportedFeatures)
			}
			if apiErr.ClientEndpoint != plan.ClientEndpoint {
				t.Fatalf("client_endpoint = %q", apiErr.ClientEndpoint)
			}
		})
	}

	// native plan 不做 conversion preflight
	native := plan
	native.Mode = TransportModeNative
	if err := ValidateConversionRequest(native, map[string]any{"tools": []any{}}); err != nil {
		t.Fatalf("native plan should not reject tools in ValidateConversionRequest: %#v", err)
	}
}

func TestResolveTransportPlansCarriesConversionLevel(t *testing.T) {
	cfg := config.Config{Providers: map[string]config.Provider{
		"anthropic": {Name: "anthropic", Protocol: "anthropic", Models: []string{"claude-test"}, Endpoints: []string{config.ProviderEndpointMessages}},
	}, ModelMetadata: map[string]config.ModelMetadata{
		"claude-test": {ID: "claude-test", ConversionCapabilities: map[string]config.ConversionCapability{
			config.ProviderEndpointMessages: {Level: 3, Text: true, Tools: true},
		}},
	}}
	plans, apiErr := ResolveTransportPlans(cfg, effectivecatalog.FromStatic(cfg), http.MethodPost, "/v1/responses", "claude-test")
	if apiErr != nil || len(plans) != 1 {
		t.Fatalf("plans=%+v err=%v", plans, apiErr)
	}
	if plans[0].ConversionLevel != 3 {
		t.Fatalf("conversion level = %d, want 3", plans[0].ConversionLevel)
	}
}

func TestResolveTransportPlansPrefersNativeResponsesOverHigherPriorityConversion(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"conversion": {Name: "conversion", Protocol: "anthropic", Models: []string{"shared-model"}, Endpoints: []string{config.ProviderEndpointMessages}, Priority: 200, Fallback: true},
			"native":     {Name: "native", Protocol: "openai", Models: []string{"shared-model"}, Endpoints: []string{config.ProviderEndpointResponses}, Priority: 100, Fallback: true},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"shared-model": {ID: "shared-model", ConversionCapabilities: map[string]config.ConversionCapability{
				config.ProviderEndpointMessages: {Level: 2, Text: true, Streaming: true},
			}},
		},
	}
	plans, apiErr := ResolveTransportPlans(cfg, effectivecatalog.FromStatic(cfg), http.MethodPost, "/v1/responses", "shared-model")
	if apiErr != nil || len(plans) != 2 {
		t.Fatalf("plans=%+v err=%v", plans, apiErr)
	}
	if plans[0].Mode != TransportModeNative || plans[0].RouteOwner != "native" || plans[1].Mode != TransportModeResponsesToAnthropic {
		t.Fatalf("plans=%+v, want native before conversion", plans)
	}
}

func TestResolveTransportPlansExposesResponsesConversionBeforeLegacyChatForFullOpenAIProvider(t *testing.T) {
	const modelID = "gpt-full"
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"openai": {
				Name: "openai", Protocol: "openai", Models: []string{modelID},
				Endpoints: []string{config.ProviderEndpointChatCompletions, config.ProviderEndpointResponses},
			},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			modelID: {ID: modelID, ConversionCapabilities: map[string]config.ConversionCapability{
				config.ProviderEndpointResponses: {Level: 3, Text: true, Tools: true, Streaming: true},
			}},
		},
	}
	plans, apiErr := ResolveTransportPlans(cfg, effectivecatalog.FromStatic(cfg), http.MethodPost, "/v1/messages", modelID)
	if apiErr != nil || len(plans) != 2 {
		t.Fatalf("plans=%+v err=%+v", plans, apiErr)
	}
	if plans[0].Mode != TransportModeAnthropicToResponses || plans[0].UpstreamEndpoint != "/v1/responses" || plans[0].ConversionLevel != 3 {
		t.Fatalf("first plan=%+v, want released Level 3 Responses conversion", plans[0])
	}
	if plans[1].Mode != TransportModeAnthropicToOpenAI || plans[1].UpstreamEndpoint != "/v1/chat/completions" {
		t.Fatalf("fallback plan=%+v, want legacy Chat conversion", plans[1])
	}
}

func TestResolveTransportPlansRequiresMatchingEndpointTemplate(t *testing.T) {
	const modelID = "claude-unreleased"
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"anthropic": {Name: "anthropic", Protocol: "anthropic", Models: []string{modelID}, Endpoints: []string{config.ProviderEndpointMessages}},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			modelID: {ID: modelID, ConversionCapabilities: map[string]config.ConversionCapability{
				config.ProviderEndpointResponses: {Level: 2, Text: true, Streaming: true},
			}},
		},
	}
	plans, apiErr := ResolveTransportPlans(cfg, effectivecatalog.FromStatic(cfg), http.MethodPost, "/v1/responses", modelID)
	if len(plans) != 0 || apiErr == nil || apiErr.Code != ErrorCodeEndpointUnsupported {
		t.Fatalf("plans=%+v err=%+v", plans, apiErr)
	}
}

func TestResolveTransportPlansTemplateAppliesAcrossProviders(t *testing.T) {
	const modelID = "claude-shared-release"
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"first":  {Name: "first", Protocol: "anthropic", Models: []string{modelID}, Endpoints: []string{config.ProviderEndpointMessages}},
			"second": {Name: "second", Protocol: "anthropic", Models: []string{modelID}, Endpoints: []string{config.ProviderEndpointMessages}},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			modelID: {ID: modelID, ConversionCapabilities: map[string]config.ConversionCapability{
				config.ProviderEndpointMessages: {Level: 3, Text: true, Tools: true, Streaming: true},
			}},
		},
	}
	plans, apiErr := ResolveTransportPlans(cfg, effectivecatalog.FromStatic(cfg), http.MethodPost, "/v1/responses", modelID)
	if apiErr != nil || len(plans) != 2 {
		t.Fatalf("plans=%+v err=%+v", plans, apiErr)
	}
}

func TestClientProtocolForPath(t *testing.T) {
	if got := ClientProtocolForPath("/v1/messages"); got != ClientProtocolAnthropic {
		t.Fatalf("messages protocol = %q", got)
	}
	if got := ClientProtocolForPath("/v1/chat/completions"); got != ClientProtocolOpenAI {
		t.Fatalf("chat protocol = %q", got)
	}
	if got := ClientProtocolForPath("/v1/embeddings"); got != ClientProtocolOpenAI {
		t.Fatalf("embeddings protocol = %q", got)
	}
}

func TestConvertUpstreamErrorForClient(t *testing.T) {
	plan := TransportPlan{
		ModelID:          "gpt-test",
		ClientProtocol:   ClientProtocolAnthropic,
		ClientEndpoint:   "/v1/messages",
		UpstreamProtocol: "openai",
		Mode:             TransportModeAnthropicToOpenAI,
	}
	body, ct := convertUpstreamErrorForClient(plan, 429, []byte(`{"error":{"message":"rate limited","type":"rate_limit"}}`), "openai-full")
	if ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(string(body), `"type":"error"`) {
		t.Fatalf("anthropic envelope missing: %s", body)
	}
	if !strings.Contains(string(body), "rate limited") {
		t.Fatalf("message missing: %s", body)
	}
	if strings.Contains(string(body), "sk-") {
		t.Fatal("must not leak secrets")
	}

	plan.ClientProtocol = ClientProtocolOpenAI
	body, _ = convertUpstreamErrorForClient(plan, 500, []byte(`{"type":"error","error":{"message":"boom","type":"api_error"}}`), "anthropic")
	if !strings.Contains(string(body), `"type":"upstream_error"`) {
		t.Fatalf("openai envelope missing: %s", body)
	}
}
