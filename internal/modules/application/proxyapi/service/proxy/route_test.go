package proxy

import (
	"net/http"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	"aetherrelay/internal/pkg/aetherrelayconfig"
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
					config.ProviderEndpointResponses,
					config.ProviderEndpointEmbeddings,
				},
			},
			"openai-images-only": {
				Name:      "openai-images-only",
				Protocol:  "openai",
				BaseURL:   "https://openai-images.test/v1",
				APIKey:    "k",
				Models:    []string{"images-only-*", "images-only-model"},
				Endpoints: []string{config.ProviderEndpointImages},
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
				ID: "gpt-test", ContextWindowTokens: 128000, MaxOutputTokens: 4096, ConversionCapabilities: map[string]config.ConversionCapability{config.ProviderEndpointResponses: {Level: 1, Text: true}},
			},
			"text-embedding-test": {
				ID: "text-embedding-test", ContextWindowTokens: 8192, MaxOutputTokens: 8191,
			},
			"images-only-model": {
				ID: "images-only-model", ContextWindowTokens: 32000, MaxOutputTokens: 4096,
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

func TestResolveTransportPlansScopesCandidatesBeforePriority(t *testing.T) {
	cfg := config.Config{Providers: map[string]config.Provider{
		"primary": {Name: "primary", Protocol: "openai", BaseURL: "https://primary.test", APIKey: "k", Models: []string{"shared"}, Priority: 200, Endpoints: []string{config.ProviderEndpointResponses}},
		"backup":  {Name: "backup", Protocol: "openai", BaseURL: "https://backup.test", APIKey: "k", Models: []string{"shared"}, Priority: 100, Endpoints: []string{config.ProviderEndpointResponses}},
	}}
	policy, err := clientaccess.Selected([]string{"backup"})
	if err != nil {
		t.Fatal(err)
	}
	plans, apiErr := ResolveTransportPlansForAccess(cfg, effectivecatalog.FromStatic(cfg), policy, http.MethodPost, "/v1/responses", "shared")
	if apiErr != nil {
		t.Fatalf("ResolveTransportPlansForAccess: %#v", apiErr)
	}
	if len(plans) != 1 || plans[0].RouteOwner != "backup" {
		t.Fatalf("plans=%+v", plans)
	}
	policy, err = clientaccess.Selected([]string{"missing"})
	if err != nil {
		t.Fatal(err)
	}
	_, apiErr = ResolveTransportPlansForAccess(cfg, effectivecatalog.FromStatic(cfg), policy, http.MethodPost, "/v1/responses", "shared")
	if apiErr == nil || apiErr.Code != ErrorCodeModelNotFound {
		t.Fatalf("unauthorized model error=%#v", apiErr)
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
			name: "retired chat endpoint",
			path: "/v1/chat/completions", model: "gpt-test",
			wantCode: ErrorCodeEndpointUnsupported,
		},
		{
			name: "responses to anthropic conversion",
			path: "/v1/responses", model: "claude-test",
			wantMode: TransportModeResponsesToAnthropic, wantUpstreamPath: "/v1/messages", wantOwner: "anthropic",
		},
		{
			name: "anthropic messages native",
			path: "/v1/messages", model: "claude-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/messages", wantOwner: "anthropic",
		},
		{
			name: "anthropic messages to openai conversion",
			path: "/v1/messages", model: "gpt-test",
			wantMode: TransportModeAnthropicToResponses, wantUpstreamPath: "/v1/responses", wantOwner: "openai-full",
		},
		{
			name: "anthropic messages to responses conversion",
			path: "/v1/messages", model: "responses-only-model",
			wantMode: TransportModeAnthropicToResponses, wantUpstreamPath: "/v1/responses", wantOwner: "openai-responses-only",
		},
		{
			name: "responses native",
			path: "/v1/responses", model: "gpt-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/responses", wantOwner: "openai-full",
		},
		{
			name: "retired completions endpoint",
			path: "/v1/completions", model: "gpt-test",
			wantCode: ErrorCodeEndpointUnsupported,
		},
		{
			name: "embeddings native",
			path: "/v1/embeddings", model: "text-embedding-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/embeddings", wantOwner: "openai-full",
		},
		{
			name: "responses not available on images-only endpoint",
			path: "/v1/responses", model: "images-only-model",
			// RouteOwner 只声明 images，没有 responses endpoint。
			wantCode: ErrorCodeEndpointUnsupported,
		},
		{
			name: "embeddings not available on images-only endpoint",
			path: "/v1/embeddings", model: "images-only-model",
			wantCode: ErrorCodeEndpointUnsupported,
		},
		{
			name: "embeddings not available via anthropic conversion",
			path: "/v1/embeddings", model: "claude-test",
			wantCode: ErrorCodeEndpointUnsupported,
		},
		{
			name: "responses available via declared anthropic conversion",
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
			path: "/v1/responses", model: "",
			wantCode: ErrorCodeModelRequired,
		},
		{
			name: "model not found",
			path: "/v1/responses", model: "missing-model",
			wantCode: ErrorCodeModelNotFound,
		},
		{
			name: "provider endpoint determines model serviceability",
			path: "/v1/responses", model: "text-embedding-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/responses", wantOwner: "openai-full",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plans, apiErr := ResolveTransportPlans(cfg, effectivecatalog.FromStatic(cfg), http.MethodPost, tc.path, tc.model)
			if tc.wantCode != "" {
				if apiErr == nil {
					t.Fatalf("expected error %s, got plans %#v", tc.wantCode, plans)
				}
				if apiErr.Code != tc.wantCode {
					t.Fatalf("code = %q want %q msg=%s", apiErr.Code, tc.wantCode, apiErr.Message)
				}
				return
			}
			if apiErr != nil {
				t.Fatalf("unexpected error: %#v", apiErr)
			}
			plan := plans[0]
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
				Endpoints: []string{config.ProviderEndpointResponses},
			},
			"backup": {
				Name: "backup", Protocol: "openai", BaseURL: "https://backup.test", APIKey: "k", Models: []string{"shared"}, Priority: 20,
				Endpoints: []string{config.ProviderEndpointResponses},
			},
			"standby-disabled": {
				Name: "standby-disabled", Protocol: "openai", BaseURL: "https://disabled.test", APIKey: "k", Models: []string{"shared"}, Priority: 10,
				Endpoints: []string{config.ProviderEndpointResponses},
			},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"shared": {
				ID: "shared", ContextWindowTokens: 8192, MaxOutputTokens: 4096,
			},
		},
	}
	plans, apiErr := ResolveTransportPlans(cfg, effectivecatalog.FromStatic(cfg), http.MethodPost, "/v1/responses", "shared")
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
	if got := ClientProtocolForPath("/v1/responses"); got != ClientProtocolOpenAI {
		t.Fatalf("chat protocol = %q", got)
	}
	if got := ClientProtocolForPath("/v1/embeddings"); got != ClientProtocolOpenAI {
		t.Fatalf("embeddings protocol = %q", got)
	}
	if got := ClientProtocolForPath("/v1/responses/input_tokens"); got != ClientProtocolOpenAI {
		t.Fatalf("responses input tokens protocol = %q", got)
	}
}

func TestConvertUpstreamErrorForClient(t *testing.T) {
	plan := TransportPlan{
		ModelID:          "gpt-test",
		ClientProtocol:   ClientProtocolAnthropic,
		ClientEndpoint:   "/v1/messages",
		UpstreamProtocol: "openai",
		Mode:             TransportModeAnthropicToResponses,
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
