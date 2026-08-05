package proxy

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"ai-proxy/internal/pkg/aiproxyarchive"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetrics"
	"ai-proxy/internal/pkg/aiproxyusage"
)

// mustHandlerConfig 为测试构造已解析 Config:补齐 endpoints，并将历史夹具中的
// metadata ID 显式加入匹配 Provider，避免 metadata 在测试中充当模型来源。
func mustHandlerConfig(cfg config.Config) config.Config {
	// 所有业务请求均需有效客户端 Key。未专门覆盖认证场景的测试统一使用此默认凭据；
	// 认证测试可显式替换或删除它。
	if cfg.ClientAPIKeys == nil {
		cfg.ClientAPIKeys = map[string]config.ClientAPIKey{}
	}
	if _, ok := cfg.ClientAPIKeys["test-client"]; !ok {
		cfg.ClientAPIKeys["test-client"] = config.ClientAPIKey{ID: "test-client", APIKey: "test-client-key", Enabled: true}
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.Provider{}
	}
	if cfg.ModelMetadata == nil {
		cfg.ModelMetadata = map[string]config.ModelMetadata{}
	}
	seedCommonModels := len(cfg.ModelMetadata) == 0
	enabledProviders := 0
	for _, provider := range cfg.Providers {
		if !provider.Disabled {
			enabledProviders++
		}
	}
	for name, provider := range cfg.Providers {
		if provider.Name == "" {
			provider.Name = name
		}
		if provider.Protocol == "" {
			if name == "anthropic" {
				provider.Protocol = "anthropic"
			} else {
				provider.Protocol = "openai"
			}
		}
		if len(provider.Endpoints) == 0 && !provider.Disabled {
			if provider.Protocol == "anthropic" {
				provider.Endpoints = []string{config.ProviderEndpointMessages}
			} else {
				provider.Endpoints = []string{
					config.ProviderEndpointChatCompletions,
					config.ProviderEndpointResponses,
					config.ProviderEndpointCompletions,
					config.ProviderEndpointEmbeddings,
					config.ProviderEndpointImages,
				}
			}
		}
		// 仅测试 helper 兼容历史单 provider 夹具:显式补通配 pattern。
		// 生产配置仍由 config.Load 强制要求 provider.models 显式声明。
		if len(provider.Models) == 0 && !provider.Disabled && enabledProviders == 1 {
			provider.Models = []string{"*"}
		}
		cfg.Providers[name] = provider
	}
	modelIDs := make([]string, 0, len(cfg.ModelMetadata))
	for id := range cfg.ModelMetadata {
		modelIDs = append(modelIDs, id)
	}
	for _, modelID := range modelIDs {
		for _, owner := range matchingEnabled(cfg.Providers, modelID) {
			provider := cfg.Providers[owner]
			if !hasExactModel(provider.Models, modelID) {
				provider.Models = append(provider.Models, modelID)
				cfg.Providers[owner] = provider
			}
		}
	}
	if seedCommonModels {
		for _, modelID := range []string{
			"gpt-test", "deepseek-chat", "claude-test", "gpt-5.4", "kimi-k2",
			"healthcheck", "gpt-4o", "shared-model",
		} {
			matches := matchingEnabled(cfg.Providers, modelID)
			if len(matches) != 1 {
				continue
			}
			owner := matches[0]
			provider := cfg.Providers[owner]
			if !hasExactModel(provider.Models, modelID) {
				provider.Models = append(provider.Models, modelID)
				cfg.Providers[owner] = provider
			}
		}
	}
	for id, info := range cfg.ModelMetadata {
		if info.ID == "" {
			info.ID = id
		}
		if info.ContextWindowTokens > 0 && info.MaxOutputTokens > 0 && info.MaxOutputTokens >= info.ContextWindowTokens {
			info.MaxOutputTokens = info.ContextWindowTokens - 1
		}
		cfg.ModelMetadata[id] = info
	}
	return cfg
}

func hasExactModel(patterns []string, modelID string) bool {
	for _, pattern := range patterns {
		if pattern == modelID && !containsStar(pattern) {
			return true
		}
	}
	return false
}

func containsStar(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '*' {
			return true
		}
	}
	return false
}

func matchingEnabled(providers map[string]config.Provider, modelID string) []string {
	var matches []string
	for name, provider := range providers {
		if provider.Disabled {
			continue
		}
		if config.ProviderMatchesModel(name, provider, modelID) {
			matches = append(matches, name)
		}
	}
	return matches
}

func newTestHandler(cfg config.Config, usageFile string) *Handler {
	cfg = mustHandlerConfig(cfg)
	if cfg.InteractionDir == "" {
		cfg.InteractionDir = filepath.Join(filepath.Dir(usageFile), "interactions")
	}
	interactionRecorder, err := archive.NewRecorder(cfg.InteractionDir)
	if err != nil {
		panic(err)
	}
	return NewHandler(cfg, usage.NewMemoryStore(), interactionRecorder, metrics.NewRegistry())
}

// memoryStore 从 Handler 取出测试用 MemoryStore。
func memoryStore(h *Handler) *usage.MemoryStore {
	if h == nil || h.usageStore == nil {
		return nil
	}
	ms, _ := h.usageStore.(*usage.MemoryStore)
	return ms
}

// readUsageFromStore 将 MemoryStore 事件转为近似旧 CSV 的行(含表头),供既有断言复用。
func readUsageFromStore(t *testing.T, h *Handler) [][]string {
	t.Helper()
	ms := memoryStore(h)
	if ms == nil {
		t.Fatal("handler has no MemoryStore")
	}
	page, err := ms.Events(context.Background(), usage.EventFilter{
		UsageFilter: usage.UsageFilter{AllTime: true},
		PageSize:    100,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Events 按时间倒序;CSV 旧语义近似按写入序,这里再正序。
	evs := append([]usage.Event(nil), page.Events...)
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	header := []string{
		"event_id", "round_id", "time", "provider", "model", "operation", "client_endpoint", "upstream_protocol",
		"upstream_endpoint", "conversion_mode", "input_tokens", "output_tokens", "total_tokens",
		"duration_ms", "stream", "estimated", "http_status", "outcome",
		"cached_input_tokens", "cache_creation_input_tokens", "cache_hit_rate", "api_key_id",
	}
	rows := [][]string{header}
	for _, e := range evs {
		hit := "0"
		if e.InputTokens > 0 && e.CachedInputTokens > 0 {
			hit = fmt.Sprintf("%.4f", float64(e.CachedInputTokens)/float64(e.InputTokens))
		} else if e.InputTokens > 0 {
			hit = "0.0000"
		}
		// match previous CSV formatting for zero cache hit when no cached
		if e.CachedInputTokens == 0 {
			// old CSV used CacheHitRate which is 0 -> "0" or "0.0000"? check - was fmt %g-ish via sprintf
			hit = "0"
			if e.InputTokens > 0 {
				// old code: fmt.Sprintf with cache hit rate float - often "0" for 0
				hit = "0"
			}
		}
		rows = append(rows, []string{
			e.EventID,
			fmt.Sprintf("%d", e.RoundID),
			e.StartedAt.Format(time.RFC3339),
			e.Provider,
			e.Model,
			e.Operation,
			e.ClientEndpoint,
			e.UpstreamProtocol,
			e.UpstreamEndpoint,
			e.ConversionMode,
			fmt.Sprintf("%d", e.InputTokens),
			fmt.Sprintf("%d", e.OutputTokens),
			fmt.Sprintf("%d", e.TotalTokens),
			fmt.Sprintf("%d", e.DurationMS),
			fmt.Sprintf("%t", e.Stream),
			fmt.Sprintf("%t", e.Estimated),
			fmt.Sprintf("%d", e.HTTPStatus),
			e.Outcome,
			fmt.Sprintf("%d", e.CachedInputTokens),
			fmt.Sprintf("%d", e.CacheCreationInputTokens),
			hit,
			e.APIKeyID,
		})
	}
	return rows
}

// withClientKey 注册并切换客户端 Key 索引(测试热更新)。
func withClientKey(h *Handler, id, secret string) {
	if h.cfg.ClientAPIKeys == nil {
		h.cfg.ClientAPIKeys = map[string]config.ClientAPIKey{}
	}
	h.cfg.ClientAPIKeys[id] = config.ClientAPIKey{ID: id, APIKey: secret, Enabled: true}
	h.clientKeyIndex.Store(buildClientKeyIndex(h.cfg))
}
