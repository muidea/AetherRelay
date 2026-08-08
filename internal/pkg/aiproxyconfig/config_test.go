package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigFileAndEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-openai-key")
	t.Setenv("AI_PROXY_LISTEN_ADDR", "127.0.0.1:18080")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  port: 9090
  verbose_logging: false
  stream_idle_timeout_seconds: 900
state:
  dir: test-state
  database: ai-proxy.duckdb
  memory_limit: 256MB
  threads: 2
  query_cache_seconds: 15
  interaction_retention: 500
providers:
  deepseek:
    protocol: openai
    base_url: https://api.deepseek.com
    api_key: ${OPENAI_API_KEY}
    endpoints: chat_completions, responses, completions, embeddings
    models: deepseek*
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
  custom:
    protocol: openai
    base_url: https://custom.example
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: custom-*
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:18080" {
		t.Fatalf("listen addr = %s", cfg.ListenAddr)
	}
	if cfg.Providers["deepseek"].APIKey != "env-openai-key" {
		t.Fatalf("api key was not expanded")
	}
	if len(cfg.Providers["deepseek"].Models) != 1 || cfg.Providers["deepseek"].Models[0] != "deepseek*" {
		t.Fatalf("models = %#v", cfg.Providers["deepseek"].Models)
	}
	// fallbacks 已移除:配置中不得声明 fallbacks。
	if cfg.InteractionDir != filepath.Join(filepath.Dir(path), "test-state", "interactions") {
		t.Fatalf("interaction dir = %s", cfg.InteractionDir)
	}
	if cfg.InteractionRetention != 500 {
		t.Fatalf("interaction retention = %d", cfg.InteractionRetention)
	}
	if cfg.VerboseLogging {
		t.Fatalf("debug log should be disabled by config")
	}
	if cfg.StreamIdleTimeout != 900*time.Second {
		t.Fatalf("stream idle timeout = %s", cfg.StreamIdleTimeout)
	}
}

func TestLoadDisabledProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
  deepseek:
    base_url: https://api.deepseek.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    enabled: false
    protocol: openai
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Providers["deepseek"].Disabled {
		t.Fatalf("deepseek should be disabled")
	}
	if cfg.Providers["openai"].Disabled {
		t.Fatalf("openai should be enabled")
	}
}

func TestLoadAllowsChatGPTWebSettingsWithoutManagedProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
chatgpt_web:
  provider_enabled: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load chatgpt-web-only config: %v", err)
	}
	if !EffectiveChatGPTWebProviderEnabled(cfg.ChatGPTWeb) || len(cfg.Providers) != 0 {
		t.Fatalf("config=%+v", cfg)
	}
}

func TestLoadAllowsCodexOAuthAsOnlyProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
codex_oauth:
  refresh_account_interval_minute: 15
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load codex-oauth-only config: %v", err)
	}
	if cfg.CodexOAuth.RefreshAccountIntervalMinute != 15 || len(cfg.Providers) != 0 {
		t.Fatalf("config=%+v", cfg)
	}
}

func TestLoadRejectsAccountPoolLifecycleSwitches(t *testing.T) {
	for _, section := range []string{"chatgpt_web", "codex_oauth"} {
		t.Run(section, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(section+":\n  enabled: false\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), `unknown key "enabled"`) {
				t.Fatalf("Load error=%v", err)
			}
		})
	}
}

func TestLoadBuiltinProviderPriorities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
chatgpt_web:
  provider_enabled: false
  priority: 0
codex_oauth:
  provider_enabled: true
  priority: 135
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := EffectiveChatGPTWebProviderPriority(cfg.ChatGPTWeb); got != 0 {
		t.Fatalf("chatgpt priority=%d want 0", got)
	}
	if got := EffectiveCodexOAuthProviderPriority(cfg.CodexOAuth); got != 135 {
		t.Fatalf("codex priority=%d want 135", got)
	}
	if EffectiveChatGPTWebProviderEnabled(cfg.ChatGPTWeb) {
		t.Fatalf("chatgpt provider routing should be disabled")
	}
	if !EffectiveCodexOAuthProviderEnabled(cfg.CodexOAuth) {
		t.Fatalf("codex provider routing should be enabled")
	}
}

func TestLoadRejectsBuiltinProviderPriorityOutOfRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
chatgpt_web:
  priority: 1001
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "chatgpt_web.priority") {
		t.Fatalf("Load error=%v", err)
	}
}

func TestLoadAllowsCodexOAuthDiscoveryWithoutConfiguredModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
codex_oauth:
  provider_enabled: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load Codex OAuth discovery config: %v", err)
	}
	if !EffectiveCodexOAuthProviderEnabled(cfg.CodexOAuth) || len(cfg.Providers) != 0 {
		t.Fatalf("config=%+v", cfg)
	}
}

func TestLoadRejectsRemovedCodexOAuthModelsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
codex_oauth:
  models: gpt-5.2-codex
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), `codex_oauth: unknown key "models"`) {
		t.Fatalf("Load error=%v, want removed models config rejection", err)
	}
}

func TestLoadModelMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*,DeepSeek*
model_metadata:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
  DeepSeek-V4-Flash:
    context_window_tokens: 128000
    max_output_tokens: 8192
    native_responses_tools: true
    native_responses_images: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	gpt, ok := cfg.ModelMetadata["gpt-4o"]
	if !ok {
		t.Fatalf("missing gpt-4o metadata entry: %#v", cfg.ModelMetadata)
	}
	if gpt.ID != "gpt-4o" || gpt.ContextWindowTokens != 128000 || gpt.MaxOutputTokens != 16384 {
		t.Fatalf("gpt-4o = %#v", gpt)
	}
	// model id 严格区分大小写:查找键与展示 ID 均保留配置原文
	ds, ok := cfg.ModelMetadata["DeepSeek-V4-Flash"]
	if !ok {
		t.Fatalf("missing DeepSeek-V4-Flash metadata entry: %#v", cfg.ModelMetadata)
	}
	if ds.ID != "DeepSeek-V4-Flash" || ds.ContextWindowTokens != 128000 || ds.MaxOutputTokens != 8192 {
		t.Fatalf("DeepSeek-V4-Flash = %#v", ds)
	}
	if !ds.NativeResponsesDeclared || !ds.NativeResponsesTools || ds.NativeResponsesImages {
		t.Fatalf("DeepSeek native Responses capabilities = %#v", ds)
	}
	if _, ok := cfg.ModelMetadata["deepseek-v4-flash"]; ok {
		t.Fatalf("unexpected lowercased metadata key: %#v", cfg.ModelMetadata)
	}
}

func TestLoadModelConversionCapabilities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
model_metadata:
  demo:
    reasoning_supported: true
    reasoning_default_effort: medium
    reasoning_efforts: [low, medium]
    conversion_capabilities:
      messages:
        profile: level1
      responses:
        profile: level3_reasoning
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.ModelMetadata["demo"].ConversionCapabilities
	if got[ProviderEndpointMessages].Level != 1 || !got[ProviderEndpointMessages].Text || got[ProviderEndpointResponses].Level != 3 || !got[ProviderEndpointResponses].Tools || got[ProviderEndpointResponses].ReasoningAdapter != ReasoningAdapterAnthropicToResponsesEffort {
		t.Fatalf("conversion capabilities = %#v", got)
	}
}

func TestLoadRejectsProviderConversionRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  anthropic:
    protocol: anthropic
    base_url: https://api.anthropic.com
    api_key: test
    endpoints: messages
    models: claude-demo
    conversion_releases: {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown key \"conversion_releases\"") {
		t.Fatalf("error = %v, want removed provider release rejection", err)
	}
}

func TestModelConversionCapabilityMatchesEndpoint(t *testing.T) {
	metadata := ModelMetadata{ConversionCapabilities: map[string]ConversionCapability{
		ProviderEndpointMessages:  {Level: 3, Text: true, Tools: true},
		ProviderEndpointResponses: {Level: 2, Text: true, Streaming: true},
	}}
	if _, ok := ModelConversionCapability(metadata, "/v1/messages", ConversionDirectionResponsesToAnthropic); !ok {
		t.Fatal("messages template did not enable Responses to Anthropic")
	}
	if _, ok := ModelConversionCapability(metadata, "/v1/messages", ConversionDirectionAnthropicToResponses); ok {
		t.Fatal("messages template enabled the opposite direction")
	}
}

func TestLoadRejectsInvalidConversionProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
model_metadata:
  demo:
    conversion_capabilities:
      messages:
        profile: level4
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown profile") {
		t.Fatalf("error = %v, want invalid conversion profile", err)
	}
}

func TestLoadRejectsManualConversionCapabilityFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
model_metadata:
  demo:
    conversion_capabilities:
      messages:
        level: 3
        text: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown key \"level\"") {
		t.Fatalf("error = %v, want manual capability rejection", err)
	}
}

func TestLoadModelConversionReasoningAdapter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
model_metadata:
  demo:
    reasoning_supported: true
    reasoning_default_effort: medium
    reasoning_efforts: [low, medium]
    conversion_capabilities:
      messages:
        profile: level2_reasoning
      responses:
        profile: level2_reasoning
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	capability := cfg.ModelMetadata["demo"].ConversionCapabilities[ProviderEndpointMessages]
	if !ConversionCapabilityUsable(ConversionDirectionResponsesToAnthropic, capability) || capability.ReasoningTargetEffort != "medium" {
		t.Fatalf("capability = %#v", capability)
	}
}

func TestLoadRejectsReasoningProfileWithoutDefaultEffort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
model_metadata:
  demo:
    reasoning_supported: true
    reasoning_efforts: [low]
    conversion_capabilities:
      messages:
        profile: level2_reasoning
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "requires reasoning support and a default effort") {
		t.Fatalf("error = %v, want missing default effort rejection", err)
	}
}

func TestLoadRejectsRemovedModelCatalogSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions
    models: gpt-test
model_catalog:
  gpt-test:
    context_window_tokens: 128000
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), `unknown section "model_catalog"`) {
		t.Fatalf("error = %v, want removed section rejection", err)
	}
}

func TestLoadInteractionRetentionFromState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
state:
  interaction_retention: 321
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InteractionRetention != 321 {
		t.Fatalf("interaction retention = %d", cfg.InteractionRetention)
	}
}

func TestLoadRejectsLegacyStateConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
usage_store:
  path: usage.duckdb
providers:
  local:
    protocol: openai
    base_url: http://127.0.0.1:9000/v1
    allow_unauthenticated: true
    endpoints: chat_completions
    models: local-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown section") {
		t.Fatalf("legacy usage_store error = %v", err)
	}

	t.Setenv("AI_PROXY_USAGE_STORE_PATH", "usage.duckdb")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "legacy state environment variables") {
		t.Fatalf("legacy environment error = %v", err)
	}
}

func TestLoadStreamIdleTimeoutCanBeDisabledFromEnv(t *testing.T) {
	t.Setenv("AI_PROXY_STREAM_IDLE_TIMEOUT_SECONDS", "0")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  stream_idle_timeout_seconds: 120
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StreamIdleTimeout != 0 {
		t.Fatalf("stream idle timeout = %s", cfg.StreamIdleTimeout)
	}
}

func TestLoadStreamFirstEventTimeout(t *testing.T) {
	t.Setenv("AI_PROXY_STREAM_FIRST_EVENT_TIMEOUT_SECONDS", "7")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StreamFirstEventTimeout != 7*time.Second {
		t.Fatalf("stream first event timeout = %s", cfg.StreamFirstEventTimeout)
	}
}

func TestLoadRejectsDefaultProviderConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions
    models: gpt-*
default_provider: openai
model_metadata:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "default_provider is not supported") {
		t.Fatalf("error = %v, want default_provider not supported", err)
	}
}

func TestLoadIgnoresAIProxyDefaultProviderEnv(t *testing.T) {
	// env 不得再注入/覆盖 default_provider 路由语义。
	t.Setenv("AI_PROXY_DEFAULT_PROVIDER", "openai")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions
    models: gpt-*
model_metadata:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = cfg
}

func TestLoadRejectsDefaultProviderEvenIfValidProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  default_provider: openai
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions
    models: gpt-*
model_metadata:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "default_provider is not supported") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadParsesMetricsFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `server:
  listen_addr: 127.0.0.1:9090
  metrics_remote_access: true
  metrics_allowed_cidrs: 10.0.0.0/8, 192.168.0.0/16
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:9090" {
		t.Fatalf("listen = %q", cfg.ListenAddr)
	}
	if !cfg.MetricsRemoteAccess {
		t.Fatalf("MetricsRemoteAccess = false, want true")
	}
	if len(cfg.MetricsAllowedCIDRs) != 2 {
		t.Fatalf("cidrs = %v, want 2 entries", cfg.MetricsAllowedCIDRs)
	}
}

func TestLoadAllowsNonLoopbackWithoutClientKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  listen_addr: 0.0.0.0:8080
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "0.0.0.0:8080" {
		t.Fatalf("listen = %q", cfg.ListenAddr)
	}
}

func TestLoadRejectsInboundAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  listen_addr: 127.0.0.1:8080
  inbound_api_key: secret-key
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected inbound_api_key to be rejected")
	}
	if !strings.Contains(err.Error(), "inbound_api_key") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsUsageFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  usage_file: usage.csv
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected usage_file to be rejected")
	}
	if !strings.Contains(err.Error(), "usage_file") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsClientAPIKeysSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("client_api_keys:\n  legacy:\n    api_key: ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown section") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRejectsLegacyClientKeySection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
client_api_keys:
  default:
    api_key: sk-x
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown section") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRejectsLegacyClientKeySectionWithNestedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
client_api_keys:
  a:
    api_key: same-secret
  b:
    api_key: same-secret
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected duplicate secret error")
	}
	if !strings.Contains(err.Error(), "unknown section") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsInboundEnv(t *testing.T) {
	t.Setenv("AI_PROXY_INBOUND_API_KEY", "x")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "AI_PROXY_INBOUND_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  typo_key: true
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected unknown key error")
	}
	if !strings.Contains(err.Error(), "unknown config key") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsInvalidBool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  verbose_logging: maybe
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected invalid bool error")
	}
	if !strings.Contains(err.Error(), "invalid boolean") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsUnknownProtocol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom:
    base_url: https://example.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    protocol: graphql
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected unknown protocol error")
	}
	if !strings.Contains(err.Error(), "unknown protocol") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsFallbacksConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions
    fallbacks: backup
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "use priority and fallback") {
		t.Fatalf("error = %v, want migration guidance", err)
	}
}

func TestIsLoopbackListenAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080": true,
		"[::1]:8080":     true,
		"localhost:8080": true,
		":8080":          false,
		"0.0.0.0:8080":   false,
		"192.168.1.1:80": false,
		"":               false,
	}
	for addr, want := range cases {
		if got := IsLoopbackListenAddr(addr); got != want {
			t.Fatalf("IsLoopbackListenAddr(%q)=%v want %v", addr, got, want)
		}
	}
}

func TestLoadMetricsRemoteAccessFromEnv(t *testing.T) {
	t.Setenv("AI_PROXY_METRICS_REMOTE_ACCESS", "true")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MetricsRemoteAccess {
		t.Fatalf("MetricsRemoteAccess = false, want true from env")
	}
}

func TestLoadLogFormatFromConfigAndEnv(t *testing.T) {
	t.Setenv("AI_PROXY_LOG_FORMAT", "json")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  log_format: text
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogFormat != "json" {
		t.Fatalf("log format = %q, want env override json", cfg.LogFormat)
	}
}

func TestLoadRejectsInvalidEnv(t *testing.T) {
	t.Setenv("AI_PROXY_VERBOSE_LOGGING", "maybe")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected invalid env bool to fail")
	}
	if !strings.Contains(err.Error(), "AI_PROXY_VERBOSE_LOGGING") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsInvalidMaxBodyEnv(t *testing.T) {
	t.Setenv("AI_PROXY_MAX_REQUEST_BODY_BYTES", "invalid")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected invalid env int to fail")
	}
	if !strings.Contains(err.Error(), "AI_PROXY_MAX_REQUEST_BODY_BYTES") {
		t.Fatalf("error = %q", err)
	}
}

func TestAddrFromPortUsesLoopback(t *testing.T) {
	if got := addrFromPort("8080"); got != "127.0.0.1:8080" {
		t.Fatalf("addrFromPort(8080) = %q", got)
	}
	if got := addrFromPort(":9090"); got != "127.0.0.1:9090" {
		t.Fatalf("addrFromPort(:9090) = %q", got)
	}
	if got := addrFromPort("0.0.0.0:8080"); got != "0.0.0.0:8080" {
		t.Fatalf("addrFromPort full = %q", got)
	}
}

func TestLoadRejectsInvalidBaseURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  bad:
    protocol: openai
    base_url: not-a-url
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected invalid base_url error")
	}
	if !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsNonHTTPBaseURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  bad:
    protocol: openai
    base_url: ftp://example.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected scheme error")
	}
}

func TestLoadRejectsCaseFoldProviderDuplicate(t *testing.T) {
	// 通过直接构造 Config 测 normalize:大小写折叠后重复 provider 应启动失败。
	cfg := Config{
		ListenAddr: "127.0.0.1:8080",
		Providers: map[string]Provider{
			"OpenAI": {Name: "OpenAI", Protocol: "openai", BaseURL: "https://api.openai.com", APIKey: "a"},
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://api.openai.com", APIKey: "b"},
		},
	}
	if err := normalize(&cfg, ""); err == nil {
		t.Fatal("expected case-fold duplicate provider error")
	}
}

func TestLoadAllowsCaseDifferentModelMetadataIDs(t *testing.T) {
	// model ID 严格区分大小写:DeepSeek-V4-Flash 与 deepseek-v4-flash / GPT-4o 与 gpt-4o 是不同模型。
	cfg := Config{
		ListenAddr: "127.0.0.1:8080",
		Providers: map[string]Provider{
			"openai": {
				Name: "openai", Protocol: "openai", BaseURL: "https://api.openai.com", APIKey: "a",
				Models:    []string{"gpt-*", "GPT-*"},
				Endpoints: []string{ProviderEndpointChatCompletions, ProviderEndpointEmbeddings},
			},
		},
		ModelMetadata: map[string]ModelMetadata{
			"GPT-4o": {
				ID: "GPT-4o", ContextWindowTokens: 128000, MaxOutputTokens: 16384,
			},
			"gpt-4o": {
				ID: "gpt-4o", ContextWindowTokens: 8192, MaxOutputTokens: 4096,
			},
		},
	}
	if err := normalize(&cfg, ""); err != nil {
		t.Fatalf("normalize case-different models: %v", err)
	}
	if err := validate(cfg); err != nil {
		t.Fatalf("validate case-different models: %v", err)
	}
	if _, ok := cfg.ModelMetadata["GPT-4o"]; !ok {
		t.Fatal("missing GPT-4o")
	}
	if _, ok := cfg.ModelMetadata["gpt-4o"]; !ok {
		t.Fatal("missing gpt-4o")
	}
}

func TestProviderMatchesModelRequiresExplicitPatterns(t *testing.T) {
	provider := Provider{Name: "deepseek", Protocol: "openai"}
	if ProviderMatchesModel("deepseek", provider, "deepseek-chat") {
		t.Fatal("provider name/protocol must not infer model patterns")
	}
	provider.Models = []string{"deepseek-*"}
	if !ProviderMatchesModel("deepseek", provider, "deepseek-chat") {
		t.Fatal("explicit case-sensitive model pattern should match")
	}
}

func TestLoadRejectsEnabledProviderWithoutModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "models is required") {
		t.Fatalf("error = %v, want explicit models requirement", err)
	}
}

func TestLoadRejectsExactModelMetadataDuplicate(t *testing.T) {
	cfg := Config{
		ListenAddr: "127.0.0.1:8080",
		Providers: map[string]Provider{
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://api.openai.com", APIKey: "a"},
		},
		ModelMetadata: map[string]ModelMetadata{
			"gpt-4o": {ID: "gpt-4o"},
		},
	}
	// 模拟 map 键与 info.ID 不同但归一化后撞上同一 id 的情况:
	// 再塞一个 name 不同、ID 相同的条目(通过二次写入 ensure 路径不方便,直接调 normalize 前构造)。
	cfg.ModelMetadata["alias"] = ModelMetadata{ID: "gpt-4o"}
	if err := normalize(&cfg, ""); err == nil {
		t.Fatal("expected exact duplicate model error")
	}
}

func TestLoadRejectsInvalidSLOWebhook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  listen_addr: 127.0.0.1:8080
  slo_violation_webhook: not-a-url
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected invalid webhook error")
	}
	if !strings.Contains(err.Error(), "slo_violation_webhook") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsNonHTTPWebhook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  listen_addr: 127.0.0.1:8080
  slo_violation_webhook: ftp://hooks.example/secret
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected scheme error")
	}
}

func TestLoadAcceptsModelMetadataWithoutCapabilities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-4o
model_metadata:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsRemovedModelCapabilities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-4o
model_metadata:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    capabilities: text_generation
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), `unknown key "capabilities"`) {
		t.Fatalf("error = %v, want removed capabilities rejection", err)
	}
}

func TestLoadRejectsRemovedCapabilityConfigurationKeys(t *testing.T) {
	t.Run("provider endpoint_capabilities", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions
    models: gpt-4o
`), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), `unknown key "endpoint_capabilities"`) {
			t.Fatalf("error = %v, want removed endpoint_capabilities rejection", err)
		}
	})

	t.Run("model operations", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions
    models: gpt-4o
model_metadata:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
`), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), `unknown key "operations"`) {
			t.Fatalf("error = %v, want removed operations rejection", err)
		}
	})
}

func TestLoadAllowsUnusedModelMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
model_metadata:
  orphan-model:
    context_window_tokens: 128000
    max_output_tokens: 16384
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.ModelMetadata["orphan-model"]; !ok {
		t.Fatalf("unused metadata was not preserved: %#v", cfg.ModelMetadata)
	}
}

func TestLoadPreservesMetadataForPatternOnlyProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  primary:
    protocol: openai
    base_url: https://a.example
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: shared-*
    priority: 200
  backup:
    protocol: openai
    base_url: https://b.example
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: shared-*
    priority: 10
    fallback: true
model_metadata:
  shared-model:
    context_window_tokens: 128000
    max_output_tokens: 16384
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	info := cfg.ModelMetadata["shared-model"]
	if info.ContextWindowTokens != 128000 || info.MaxOutputTokens != 16384 {
		t.Fatalf("metadata = %#v", info)
	}
}

func TestLoadRejectsInvalidMetadataCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions, responses, completions, embeddings
    models: gpt-*
model_metadata:
  gpt-4o:
    context_window_tokens: 1000
    max_output_tokens: 1000
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "max_output_tokens must be less than context_window_tokens") {
		t.Fatalf("error = %v, want capacity relation error", err)
	}
}

func TestLoadRejectsMissingEndpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom-openai:
    protocol: openai
    base_url: https://example.com
    api_key: test
    models: gpt-*
model_metadata:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "endpoints is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsUnknownEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom-openai:
    protocol: openai
    base_url: https://example.com
    api_key: test
    endpoints: chat_completions, widgets
    models: gpt-*
model_metadata:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown endpoint") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadMetadataDoesNotRequireEndpointIntersection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom-openai:
    protocol: openai
    base_url: https://example.com
    api_key: test
    endpoints: chat_completions
    models: emb-*
model_metadata:
  emb-model:
    context_window_tokens: 8192
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if cfg.ModelMetadata["emb-model"].MaxOutputTokens != 0 {
		t.Fatalf("max output tokens = %d, want omitted metadata to remain zero", cfg.ModelMetadata["emb-model"].MaxOutputTokens)
	}
}

func TestLoadDoesNotMaterializeExactProviderModelIntoMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  embedding-e5:
    protocol: openai
    base_url: https://example.com
    api_key: test
    endpoints: embeddings
    models: intfloat/multilingual-e5-small
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ModelMetadata) != 0 {
		t.Fatalf("provider model must not create metadata: %#v", cfg.ModelMetadata)
	}
}

func TestLoadDoesNotMaterializeProviderWildcardPattern(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  embedding-e5:
    protocol: openai
    base_url: https://example.com
    api_key: test
    endpoints: embeddings
    models: intfloat/*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ModelMetadata) != 0 {
		t.Fatalf("wildcard pattern must not publish a model ID: %#v", cfg.ModelMetadata)
	}
}

func TestLoadAllowsMetadataEntryWithoutCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://example.com
    api_key: test
    endpoints: chat_completions
    models: gpt-*
model_metadata:
  gpt-test:
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	info := cfg.ModelMetadata["gpt-test"]
	if info.ID != "gpt-test" || info.ContextWindowTokens != 0 || info.MaxOutputTokens != 0 {
		t.Fatalf("metadata defaults = %#v", info)
	}
}

func TestLoadRejectsNegativeMetadataCapacity(t *testing.T) {
	for _, field := range []string{"context_window_tokens", "max_output_tokens"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := fmt.Sprintf(`
providers:
  openai:
    protocol: openai
    base_url: https://example.com
    api_key: test
    endpoints: chat_completions
    models: gpt-test
model_metadata:
  gpt-test:
    %s: -1
`, field)
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatalf("negative %s was accepted", field)
			}
		})
	}
}

func TestLoadPreservesExplicitZeroProviderPriority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  zero:
    protocol: openai
    base_url: https://example.com
    api_key: test
    endpoints: chat_completions
    models: gpt-*
    priority: 0
  defaulted:
    protocol: openai
    base_url: https://fallback.example
    api_key: test
    endpoints: chat_completions
    models: gpt-*
  no-fallback:
    protocol: openai
    base_url: https://no-fallback.example
    api_key: test
    endpoints: chat_completions
    models: other-*
    fallback: false
model_metadata:
  gpt-test:
    context_window_tokens: 8192
    max_output_tokens: 4096
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := EffectiveProviderPriority(cfg.Providers["zero"]); got != 0 {
		t.Fatalf("explicit zero priority = %d, want 0", got)
	}
	if got := EffectiveProviderPriority(cfg.Providers["defaulted"]); got != DefaultProviderPriority {
		t.Fatalf("default priority = %d, want %d", got, DefaultProviderPriority)
	}
	if !EffectiveProviderFallback(cfg.Providers["defaulted"]) || EffectiveProviderFallback(cfg.Providers["no-fallback"]) {
		t.Fatalf("fallback defaults = defaulted:%t no-fallback:%t", EffectiveProviderFallback(cfg.Providers["defaulted"]), EffectiveProviderFallback(cfg.Providers["no-fallback"]))
	}
}

func TestResolveProviderTransportsReturnsOrderedOpenAIMessagesCandidates(t *testing.T) {
	provider := Provider{
		Protocol:  "openai",
		Endpoints: []string{ProviderEndpointChatCompletions, ProviderEndpointResponses},
	}

	got := ResolveProviderTransports(provider, "/v1/messages")
	want := []ProviderTransport{
		{UpstreamEndpoint: "/v1/responses", Mode: ConversionDirectionAnthropicToResponses},
		{UpstreamEndpoint: "/v1/chat/completions", Mode: "anthropic_to_openai"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("transports = %#v, want %#v", got, want)
	}

	first, ok := ResolveProviderTransport(provider, "/v1/messages")
	if !ok || first != want[0] {
		t.Fatalf("first transport = %#v, %t; want %#v, true", first, ok, want[0])
	}
}

func TestLoadNormalizesEndpointsOrderAndDedupe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom-openai:
    protocol: openai
    base_url: https://example.com
    api_key: test
    endpoints: embeddings, chat_completions, embeddings, RESPONSES
    models: gpt-*
model_metadata:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	caps := cfg.Providers["custom-openai"].Endpoints
	if len(caps) != 3 || caps[0] != ProviderEndpointChatCompletions || caps[1] != ProviderEndpointResponses || caps[2] != ProviderEndpointEmbeddings {
		t.Fatalf("caps = %#v", caps)
	}
}

func TestLoadRejectsOpenAIMessagesCapability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom-openai:
    protocol: openai
    base_url: https://example.com
    api_key: test
    endpoints: messages
    models: gpt-*
model_metadata:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "messages") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsAnthropicEmbeddingsCapability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom-anthropic:
    protocol: anthropic
    base_url: https://example.com
    api_key: test
    endpoints: embeddings
    models: claude-*
model_metadata:
  claude-x:
    context_window_tokens: 200000
    max_output_tokens: 8192
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "anthropic") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsRemoteEmptyAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  remote:
    protocol: openai
    base_url: https://api.example.com
    endpoints: chat_completions
    models: m-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "empty api_key") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsAllowUnauthenticatedOnRemote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  remote:
    protocol: openai
    base_url: https://api.example.com
    allow_unauthenticated: true
    endpoints: chat_completions
    models: m-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsAllowUnauthenticatedWithAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  local:
    protocol: openai
    base_url: http://127.0.0.1:9000/v1
    api_key: should-not-be-set
    allow_unauthenticated: true
    endpoints: chat_completions
    models: local-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadAllowsLoopbackUnauthenticated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  local:
    protocol: openai
    base_url: http://127.0.0.1:9000/v1
    allow_unauthenticated: true
    endpoints: chat_completions
    models: local-*
model_metadata:
  local-model:
    context_window_tokens: 8000
    max_output_tokens: 1000
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Providers["local"].AllowUnauthenticated {
		t.Fatal("expected allow_unauthenticated")
	}
}

func TestLoadRejectsMissingProtocol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
    endpoints: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// raw file has no protocol field
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "protocol is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadStateConfigResolvesWorkspaceFromConfigFile(t *testing.T) {
	configDir := t.TempDir()
	path := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
state:
  dir: runtime
  database: proxy.duckdb
  interaction_retention: 15
chatgpt_web:
providers:
  local:
    protocol: openai
    base_url: http://127.0.0.1:9000/v1
    allow_unauthenticated: true
    endpoints: chat_completions
    models: local-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.State.Dir, filepath.Join(configDir, "runtime"); got != want {
		t.Fatalf("state dir = %q, want %q", got, want)
	}
	if got, want := cfg.State.Database, filepath.Join(configDir, "runtime", "proxy.duckdb"); got != want {
		t.Fatalf("state database = %q, want %q", got, want)
	}
	if got := cfg.InteractionRetention; got != 15 {
		t.Fatalf("interaction retention = %d, want 15", got)
	}
}

func TestLoadRejectsRemoteStateDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
state:
  dir: https://storage.example/state
providers:
  local:
    protocol: openai
    base_url: http://127.0.0.1:9000/v1
    allow_unauthenticated: true
    endpoints: chat_completions
    models: local-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "local directory path") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsExplicitChatGPTWebProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `server:
  listen_addr: 127.0.0.1:18080
providers:
  chatgptweb:
    enabled: true
    protocol: chatgptweb
    endpoints: chat_completions, images
    models: gpt-*
model_metadata:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "chatgptweb is reserved") {
		t.Fatalf("expected reserved chatgptweb error, got %v", err)
	}
}

func TestLoadRejectsProtocolChatGPTWebUnderOtherName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `server:
  listen_addr: 127.0.0.1:18080
providers:
  my-web:
    enabled: true
    protocol: chatgptweb
    endpoints: chat_completions
    models: gpt-*
model_metadata:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "chatgptweb is reserved") {
		t.Fatalf("expected reserved chatgptweb error, got %v", err)
	}
}

func TestLoadTemporaryChatDefaultsAndOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// chatgpt_web without temporary_chat block still gets design defaults.
	if err := os.WriteFile(path, []byte("chatgpt_web:\n  provider_enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	tc := cfg.ChatGPTWeb.TemporaryChat
	if !tc.Enabled || tc.RetentionDays != 30 || tc.MaxConversations != 2000 || tc.MaxMessagesPerConversation != 200 || tc.MaxMessageBytes != 262144 || tc.TurnTimeoutSeconds != 300 {
		t.Fatalf("defaults=%+v", tc)
	}

	path2 := filepath.Join(dir, "config2.yaml")
	body := `chatgpt_web:
  temporary_chat:
    enabled: false
    retention_days: 7
    max_conversations: 10
    max_messages_per_conversation: 20
    max_message_bytes: 4096
    turn_timeout_seconds: 60
`
	if err := os.WriteFile(path2, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatal(err)
	}
	tc2 := cfg2.ChatGPTWeb.TemporaryChat
	if tc2.Enabled || tc2.RetentionDays != 7 || tc2.MaxConversations != 10 || tc2.MaxMessagesPerConversation != 20 || tc2.MaxMessageBytes != 4096 || tc2.TurnTimeoutSeconds != 60 {
		t.Fatalf("overrides=%+v", tc2)
	}
}

func TestLoadRejectsInvalidTemporaryChatRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `chatgpt_web:
  temporary_chat:
    retention_days: 0
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected retention_days validation failure")
	}
}
