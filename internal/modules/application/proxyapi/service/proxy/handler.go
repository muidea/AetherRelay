package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/textproto"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptimage"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptsearch"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"aetherrelay/internal/pkg/aetherrelayarchive"
	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	"aetherrelay/internal/pkg/aetherrelayclientauth"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelaymetrics"
	"aetherrelay/internal/pkg/aetherrelaymetricsport"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

type Handler struct {
	cfgMu               sync.RWMutex
	clientMu            sync.RWMutex
	cfg                 config.Config
	effectiveCatalog    atomic.Pointer[effectivecatalog.Snapshot]
	clientKeyIndex      atomic.Pointer[clientauth.Index]
	usageStore          usage.Store
	interactionRecorder *archive.Recorder
	metricsRegistry     metricsport.Port
	driftTracker        *FingerprintDriftTracker
	client              *http.Client
	chatGPTText         chatgpttext.Executor
	chatGPTSearch       chatgptsearch.Executor
	chatGPTImage        chatgptimage.Executor
	codexResponses      codexresponses.Executor
	codexWebsockets     atomic.Int64
}

func (h *Handler) currentClient() *http.Client {
	h.clientMu.RLock()
	client := h.client
	h.clientMu.RUnlock()
	return client
}

// WithChatGPTTextExecutor binds proxyapi's owner-local ChatGPT execution
// port. The HTTP service never receives an EventHub or an upstream client.
func (h *Handler) WithChatGPTTextExecutor(executor chatgpttext.Executor) *Handler {
	h.cfgMu.Lock()
	h.chatGPTText = executor
	h.cfgMu.Unlock()
	return h
}

// WithChatGPTSearchExecutor binds proxyapi's owner-local forced Web search
// port. It is deliberately separate from text completion so protocol adapters
// cannot accidentally treat arbitrary tools as supported.
func (h *Handler) WithChatGPTSearchExecutor(executor chatgptsearch.Executor) *Handler {
	h.cfgMu.Lock()
	h.chatGPTSearch = executor
	h.cfgMu.Unlock()
	return h
}

// WithChatGPTImageExecutor binds the local synchronous image orchestration port.
func (h *Handler) WithChatGPTImageExecutor(executor chatgptimage.Executor) *Handler {
	h.cfgMu.Lock()
	h.chatGPTImage = executor
	h.cfgMu.Unlock()
	return h
}

// WithCodexResponsesExecutor binds proxyapi's native Codex Responses use case.
// The HTTP adapter remains EventHub-free and never receives OAuth credentials.
func (h *Handler) WithCodexResponsesExecutor(executor codexresponses.Executor) *Handler {
	h.cfgMu.Lock()
	h.codexResponses = executor
	h.cfgMu.Unlock()
	return h
}

type archiveRoundKey struct{}

type usageCompletionKey struct{}

type internalFeatureIdentityKey struct{}

type usageCompletion struct{ done atomic.Bool }

func withArchiveRound(ctx context.Context, round *archive.Round) context.Context {
	return context.WithValue(ctx, archiveRoundKey{}, round)
}

func archiveRoundFromContext(ctx context.Context) *archive.Round {
	round, _ := ctx.Value(archiveRoundKey{}).(*archive.Round)
	return round
}

func withUsageCompletion(ctx context.Context, completion *usageCompletion) context.Context {
	return context.WithValue(ctx, usageCompletionKey{}, completion)
}

func usageCompletionFromContext(ctx context.Context) *usageCompletion {
	completion, _ := ctx.Value(usageCompletionKey{}).(*usageCompletion)
	return completion
}

func withInternalFeatureIdentity(ctx context.Context, _ string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	// All Admin feature/tool requests share one durable local scope.  The
	// caller-provided owner is still used by feature-specific persistence (for
	// example temporary-chat/search history), but must not create ad-hoc client
	// API-key IDs or image-library scopes.
	return context.WithValue(ctx, internalFeatureIdentityKey{}, clientauth.ClientIdentity{KeyID: config.BuiltinClientAPIKeyID, ProviderAccess: clientaccess.All()})
}

func internalFeatureIdentity(ctx context.Context) (clientauth.ClientIdentity, bool) {
	identity, ok := ctx.Value(internalFeatureIdentityKey{}).(clientauth.ClientIdentity)
	return identity, ok && strings.TrimSpace(identity.KeyID) != ""
}

// ConfigSnapshot 返回当前生效配置的独立快照，避免管理接口读取切片或 map 时
// 与后续配置切换共享可变底层数据。
func (h *Handler) ConfigSnapshot() config.Config {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	cfg := h.cfg
	cfg.MetricsAllowedCIDRs = append([]string(nil), h.cfg.MetricsAllowedCIDRs...)
	cfg.Providers = make(map[string]config.Provider, len(h.cfg.Providers))
	for name, provider := range h.cfg.Providers {
		provider.Models = append([]string(nil), provider.Models...)
		provider.Endpoints = append([]string(nil), provider.Endpoints...)
		cfg.Providers[name] = provider
	}
	cfg.ModelMetadata = make(map[string]config.ModelMetadata, len(h.cfg.ModelMetadata))
	for id, info := range h.cfg.ModelMetadata {
		cfg.ModelMetadata[id] = info
	}
	return cfg
}

// currentConfig returns a shallow immutable-by-convention snapshot. Config
// reload replaces maps rather than mutating them, so a request can safely use
// this value after the short read-lock has been released.
func (h *Handler) currentConfig() config.Config {
	h.cfgMu.RLock()
	cfg := h.cfg
	h.cfgMu.RUnlock()
	return cfg
}

// EffectiveCatalog returns the current request-time routing snapshot. When no
// snapshot has been published yet, a static-only view is synthesized from cfg.
func (h *Handler) EffectiveCatalog() effectivecatalog.Snapshot {
	if snap := h.effectiveCatalog.Load(); snap != nil {
		return *snap
	}
	h.cfgMu.RLock()
	cfg := h.cfg
	h.cfgMu.RUnlock()
	return effectivecatalog.FromStatic(cfg)
}

// ReplaceEffectiveCatalog atomically publishes a new request-time catalog.
// /v1/models and ResolveTransportPlan must observe the same generation.
func (h *Handler) ReplaceEffectiveCatalog(snap effectivecatalog.Snapshot) {
	copySnap := snap
	h.effectiveCatalog.Store(&copySnap)
}

// UpdateConfig 在完整请求边界之间原子切换运行时配置。
// 已进入代理处理的请求继续使用旧配置，新请求使用新配置。
// Client API keys are reloaded from the usage store; the usage-store path is
// not hot-swappable.
func (h *Handler) UpdateConfig(cfg config.Config) error {
	idx, err := clientauth.PrepareIndex(nil)
	if err != nil {
		return err
	}
	if h.usageStore != nil {
		records, err := h.usageStore.ListClientAPIKeys(context.Background())
		if err != nil {
			return fmt.Errorf("load client api keys: %w", err)
		}
		idx, err = prepareClientKeyIndexFromRecords(records)
		if err != nil {
			return err
		}
	}
	if err := requireResolvedConfig(cfg); err != nil {
		return err
	}
	h.cfgMu.RLock()
	changedProviders := changedProviderNames(h.cfg.Providers, cfg.Providers)
	h.cfgMu.RUnlock()
	if h.metricsRegistry != nil {
		for _, provider := range changedProviders {
			if err := h.metricsRegistry.ResetProviderHealth(provider); err != nil {
				return fmt.Errorf("reset provider %q health: %w", provider, err)
			}
		}
	}
	previousCatalog := h.EffectiveCatalog()
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	h.cfg = cfg
	// Preserve constrained discovery data while proxyapi refreshes the
	// account-pool authority. This keeps /v1/models and request routing on the
	// same effective directory throughout a configuration hot reload.
	h.ReplaceEffectiveCatalog(effectivecatalog.Reconfigure(cfg, previousCatalog))
	h.clientKeyIndex.Store(idx)
	h.clientMu.Lock()
	h.client = newHTTPClient(cfg.RequestTimeout)
	h.clientMu.Unlock()
	return nil
}

func changedProviderNames(current, next map[string]config.Provider) []string {
	names := make(map[string]struct{}, len(current)+len(next))
	for name := range current {
		names[name] = struct{}{}
	}
	for name := range next {
		names[name] = struct{}{}
	}
	changed := make([]string, 0, len(names))
	for name := range names {
		if !reflect.DeepEqual(current[name], next[name]) {
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
	return changed
}

// NewHandler 装配代理处理器。usageStore 可为 nil(仅健康检查/测试),业务请求 Start 将失败。
func NewHandler(cfg config.Config, usageStore usage.Store, interactionRecorder *archive.Recorder, metricsSource any) *Handler {
	if ms, ok := usageStore.(*usage.MemoryStore); ok {
		if records, err := ms.ListClientAPIKeys(context.Background()); err == nil && len(records) == 0 {
			sum := sha256.Sum256([]byte("test-client-key"))
			_ = ms.CreateClientAPIKey(context.Background(), usage.ClientAPIKeyRecord{ID: "test-client", Hash: "sha256:" + hex.EncodeToString(sum[:]), Enabled: true, CreatedAt: time.Now().UTC(), ProviderAccess: clientaccess.All()})
		}
	}
	if usageStore != nil {
	}
	if err := requireResolvedConfig(cfg); err != nil {
		panic("proxy.NewHandler: " + err.Error() + "; call config.Load or tests.MustHandlerConfig")
	}
	h := &Handler{
		cfg:                 cfg,
		usageStore:          usageStore,
		interactionRecorder: interactionRecorder,
		metricsRegistry:     metricsport.AsPort(metricsSource),
		driftTracker:        NewFingerprintDriftTracker(2),
		client:              newHTTPClient(cfg.RequestTimeout),
	}
	idx, err := clientauth.PrepareIndex(nil)
	if err != nil {
		panic("proxy.NewHandler: prepare empty client key index: " + err.Error())
	}
	if usageStore != nil {
		records, err := usageStore.ListClientAPIKeys(context.Background())
		if err != nil {
			panic("proxy.NewHandler: load client api keys: " + err.Error())
		}
		idx, err = prepareClientKeyIndexFromRecords(records)
		if err != nil {
			panic("proxy.NewHandler: " + err.Error())
		}
	}
	h.clientKeyIndex.Store(idx)
	h.ReplaceEffectiveCatalog(effectivecatalog.FromStatic(cfg))
	return h
}

func prepareClientKeyIndexFromRecords(records map[string]usage.ClientAPIKeyRecord) (*clientauth.Index, error) {
	keys := make([]clientauth.KeyEntry, 0, len(records))
	for id, r := range records {
		// builtin-local is an internally injected scope, never an external
		// bearer credential.  Keep that boundary even if a stale database row
		// somehow still contains a digest.
		if config.IsBuiltinClientAPIKeyID(id) {
			continue
		}
		if r.Hash != "" && r.Enabled && r.RevokedAt == nil {
			keys = append(keys, clientauth.KeyEntry{ID: id, APIKeyHash: r.Hash, Enabled: true, ProviderAccess: r.ProviderAccess})
		}
	}
	return clientauth.PrepareIndex(keys)
}

func buildClientKeyIndexFromRecords(records map[string]usage.ClientAPIKeyRecord) *clientauth.Index {
	idx, err := prepareClientKeyIndexFromRecords(records)
	if err != nil {
		return clientauth.BuildIndex(nil)
	}
	return idx
}

func (h *Handler) PrepareClientKeyIndex(records map[string]usage.ClientAPIKeyRecord) (*clientauth.Index, error) {
	return prepareClientKeyIndexFromRecords(records)
}

func (h *Handler) ActivateClientKeyIndex(index *clientauth.Index) {
	if index == nil {
		index = clientauth.BuildIndex(nil)
	}
	h.clientKeyIndex.Store(index)
}

func (h *Handler) EffectiveCatalogSnapshot() effectivecatalog.Snapshot {
	return h.EffectiveCatalog()
}

// requireResolvedConfig 要求 Config 已通过 config.Load 的 authority 合同。
// Handler 不合成 model、不补默认容量、不从 metadata 推断路由。
// 对绕过 Load 的调用方做 fail-fast 全量校验。
func requireResolvedConfig(cfg config.Config) error {
	if cfg.Providers == nil {
		return fmt.Errorf("providers is nil")
	}
	if cfg.ModelMetadata == nil {
		cfg.ModelMetadata = map[string]config.ModelMetadata{}
	}
	for name, provider := range cfg.Providers {
		if provider.Disabled {
			continue
		}
		switch provider.Protocol {
		case "openai", "anthropic":
		case "chatgptweb", "codexoauth":
			return fmt.Errorf("provider %q: builtin provider protocol %q is reserved", name, provider.Protocol)
		case "":
			return fmt.Errorf("provider %q: protocol unresolved", name)
		default:
			return fmt.Errorf("provider %q: unknown protocol %q", name, provider.Protocol)
		}
		if len(provider.Models) == 0 {
			return fmt.Errorf("provider %q: models unresolved", name)
		}
		if len(provider.Endpoints) == 0 {
			return fmt.Errorf("provider %q: endpoints unresolved", name)
		}
		if err := assertUniqueSortedKnownList("provider "+name+" endpoints", provider.Endpoints, knownProviderEndpoints()); err != nil {
			return err
		}
		if provider.Protocol == "openai" {
			for _, capName := range provider.Endpoints {
				if capName == config.ProviderEndpointMessages {
					return fmt.Errorf("provider %q: endpoints messages invalid for openai protocol", name)
				}
			}
		}
		if provider.Protocol == "anthropic" {
			for _, capName := range provider.Endpoints {
				if capName != config.ProviderEndpointMessages {
					return fmt.Errorf("provider %q: endpoints %q invalid for anthropic protocol", name, capName)
				}
			}
		}
		if provider.Protocol == "chatgptweb" {
			for _, capName := range provider.Endpoints {
				if capName != config.ProviderEndpointChatCompletions && capName != config.ProviderEndpointResponses && capName != config.ProviderEndpointImages {
					return fmt.Errorf("provider %q: endpoints %q invalid for chatgptweb protocol", name, capName)
				}
			}
		}
	}
	for id, info := range cfg.ModelMetadata {
		if strings.TrimSpace(info.ID) == "" {
			return fmt.Errorf("model_metadata.%s: missing id", id)
		}
		if info.ID != id {
			return fmt.Errorf("model_metadata.%s: id mismatch %q", id, info.ID)
		}
		if info.ContextWindowTokens < 0 {
			return fmt.Errorf("model_metadata.%s: context_window_tokens must be zero or positive", id)
		}
		if info.MaxOutputTokens < 0 {
			return fmt.Errorf("model_metadata.%s: max_output_tokens must be zero or positive", id)
		}
		if info.ContextWindowTokens > 0 && info.MaxOutputTokens > 0 && info.MaxOutputTokens >= info.ContextWindowTokens {
			return fmt.Errorf("model_metadata.%s: max_output_tokens must be less than context_window_tokens", id)
		}
	}
	return nil
}

func knownProviderEndpoints() map[string]int {
	return map[string]int{
		config.ProviderEndpointChatCompletions: 0,
		config.ProviderEndpointMessages:        1,
		config.ProviderEndpointResponses:       2,
		config.ProviderEndpointCompletions:     3,
		config.ProviderEndpointEmbeddings:      4,
		config.ProviderEndpointImages:          5,
	}
}

// assertUniqueSortedKnownList 要求 values 仅含 known 键、无重复、且按 known 秩稳定升序。
func assertUniqueSortedKnownList(label string, values []string, known map[string]int) error {
	seen := map[string]struct{}{}
	prevRank := -1
	for _, value := range values {
		rank, ok := known[value]
		if !ok {
			return fmt.Errorf("%s: unknown value %q", label, value)
		}
		if _, dup := seen[value]; dup {
			return fmt.Errorf("%s: duplicate value %q", label, value)
		}
		if rank < prevRank {
			return fmt.Errorf("%s: not in stable sorted order", label)
		}
		seen[value] = struct{}{}
		prevRank = rank
	}
	return nil
}

func newHTTPClient(requestTimeout time.Duration) *http.Client {
	client := &http.Client{}
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := transport.Clone()
		if requestTimeout > 0 {
			cloned.ResponseHeaderTimeout = requestTimeout
		}
		client.Transport = cloned
	}
	return client
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := ensureRequestID(r)
	r = attachRequestID(w, r, requestID)

	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	}
	if !isSupportedInbound(r.Method, r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	// 客户端身份解析:缺失、未知、禁用或冲突 Key 均返回 401(不计 usage)。
	identity, internal := internalFeatureIdentity(r.Context())
	if !internal {
		var err error
		identity, err = clientauth.ResolveHeaders(r.Header, h.clientKeyIndex.Load())
		if err != nil {
			writeClientProtocolError(w, http.StatusUnauthorized, clientProtocolFromRequest(r), APIError{
				Code:           ErrorCodeAuthenticationFailed,
				Message:        "missing or invalid client api key",
				ClientProtocol: clientProtocolFromRequest(r),
				ClientEndpoint: NormalizeClientEndpoint(r.URL.Path),
			})
			return
		}
	}
	if strings.TrimSpace(identity.KeyID) == "" {
		writeClientProtocolError(w, http.StatusUnauthorized, clientProtocolFromRequest(r), APIError{
			Code:           ErrorCodeAuthenticationFailed,
			Message:        "missing or invalid client api key",
			ClientProtocol: clientProtocolFromRequest(r),
			ClientEndpoint: NormalizeClientEndpoint(r.URL.Path),
		})
		return
	}
	// Client key lifecycle metadata is persisted separately from the key
	// definition. A failed metadata write must never block an authenticated
	// request or expose storage details to the caller.
	if h.usageStore != nil && !internal {
		_ = h.usageStore.TouchClientAPIKey(r.Context(), identity.KeyID, time.Now().UTC())
	}
	r = r.WithContext(clientauth.WithClientIdentity(r.Context(), identity))

	// round 与 event 在读取 body / 访问上游前建立。event ID 不复用客户端可控的
	// X-Request-ID；round_id 用于将 usage_events 和本地归档精确关联。
	round, err := h.startRound(identity.KeyID)
	if err != nil {
		writeClientProtocolError(w, http.StatusInternalServerError, clientProtocolFromRequest(r), APIError{
			Code: ErrorCodeProxyInternalError, Message: "start interaction archive failed",
			ClientProtocol: clientProtocolFromRequest(r), ClientEndpoint: NormalizeClientEndpoint(r.URL.Path),
		})
		return
	}
	if round != nil {
		round.SetRequestID(requestID)
		round.SetAPIKeyID(identity.KeyID)
		round.SetIgnoredFeatures(codexIgnoredHeaderNames(r))
		defer round.Abort()
	}
	r = r.WithContext(withArchiveRound(r.Context(), round))
	eventID := newRequestID()
	if eventID == "" {
		writeClientProtocolError(w, http.StatusServiceUnavailable, clientProtocolFromRequest(r), APIError{
			Code: ErrorCodeUsageStoreUnavailable, Message: "usage store unavailable",
			ClientProtocol: clientProtocolFromRequest(r), ClientEndpoint: NormalizeClientEndpoint(r.URL.Path),
		})
		return
	}
	r = r.WithContext(withUsageCompletion(withUsageEventID(r.Context(), eventID), &usageCompletion{}))

	// 在读取 body / 访问上游前持久化 started;失败则 503。
	if !h.beginUsage(w, r, eventID, round) {
		return
	}
	// 所有已 Start 的请求都有兜底 Complete，处理器内的正常路径会先完成它。
	defer h.completePendingUsage(r, round)

	switch {
	case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
		h.handleChatCompletions(w, r, requestID)
	case (r.URL.Path == "/v1/images/generations" || r.URL.Path == "/v1/images/edits") && r.Method == http.MethodPost:
		h.handleImages(w, r, requestID)
	case r.URL.Path == "/v1/messages" && r.Method == http.MethodPost:
		h.handleAnthropicMessages(w, r, requestID)
	case r.URL.Path == "/v1/responses/compact" && r.Method == http.MethodPost:
		h.handleCodexCompact(w, r, requestID)
	case r.URL.Path == "/v1/responses" && r.Method == http.MethodGet:
		h.handleCodexWebsocket(w, r, requestID)
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
		h.handleModels(w, r, requestID)
	case r.URL.Path == "/v1/search" && r.Method == http.MethodPost:
		h.handleSearch(w, r, requestID)
	default:
		// OpenAI 白名单透传:/v1/responses,/v1/completions,/v1/embeddings
		h.forwardRaw(w, r, requestID)
	}
}

// beginUsage 同步写入 usage started event。失败时写 503 并返回 false。
// 注意:round 在各 handler 内创建;此处先用 round_id=0 登记,Complete 时不依赖 round。
func (h *Handler) beginUsage(w http.ResponseWriter, r *http.Request, eventID string, round *archive.Round) bool {
	if h.usageStore == nil {
		writeClientProtocolError(w, http.StatusServiceUnavailable, clientProtocolFromRequest(r), APIError{
			Code:           ErrorCodeUsageStoreUnavailable,
			Message:        "usage store unavailable",
			ClientProtocol: clientProtocolFromRequest(r),
			ClientEndpoint: NormalizeClientEndpoint(r.URL.Path),
		})
		return false
	}
	identity, internal := internalFeatureIdentity(r.Context())
	if !internal {
		identity = clientauth.ClientIdentityFromContext(r.Context())
	}
	path := NormalizeClientEndpoint(r.URL.Path)
	rec := usage.StartRecord{
		EventID:        eventID,
		StartedAt:      time.Now().UTC(),
		APIKeyID:       identity.KeyID,
		Operation:      RouteLabel(r),
		Route:          RouteLabel(r),
		ClientEndpoint: path,
		ClientProtocol: ClientProtocolForPath(path),
	}
	if round != nil {
		rec.RoundID = int64(round.ID)
	}
	if err := h.usageStore.Start(r.Context(), rec); err != nil {
		if h.metricsRegistry != nil {
			h.metricsRegistry.RecordUsageStoreWriteError("start")
		}
		writeClientProtocolError(w, http.StatusServiceUnavailable, clientProtocolFromRequest(r), APIError{
			Code:           ErrorCodeUsageStoreUnavailable,
			Message:        "usage store unavailable",
			ClientProtocol: clientProtocolFromRequest(r),
			ClientEndpoint: path,
		})
		return false
	}
	if h.metricsRegistry != nil {
		h.metricsRegistry.SetUsageStoreHealthy(h.usageStore.Healthy())
	}
	return true
}

// completePendingUsage 仅在处理器漏掉结算（如未来新增的早退路径）时执行。
// 已完成 event 的条件更新会返回 ErrEventNotStarted，因此不会产生重复入账。
func (h *Handler) completePendingUsage(r *http.Request, round *archive.Round) {
	if completion := usageCompletionFromContext(r.Context()); completion != nil && completion.done.Load() {
		return
	}
	eventID := usageEventIDFromContext(r.Context())
	if eventID == "" || h.usageStore == nil {
		return
	}
	startedAt := time.Now()
	upstreamDuration := time.Duration(0)
	if round != nil {
		if !round.StartedAt.IsZero() {
			startedAt = round.StartedAt
		}
		upstreamDuration = round.UpstreamDuration
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := h.usageStore.Complete(ctx, usage.CompleteRecord{
		EventID: eventID, CompletedAt: time.Now().UTC(), HTTPStatus: http.StatusInternalServerError,
		Outcome: "error", ErrorCode: ErrorCodeProxyInternalError,
		Duration: time.Since(startedAt), UpstreamDuration: upstreamDuration,
		UpstreamStatus: func() int {
			if round != nil {
				return round.UpstreamStatus
			}
			return 0
		}(),
		UpstreamContentType: func() string {
			if round != nil {
				return round.UpstreamContentType
			}
			return ""
		}(),
		UpstreamContentLength: func() int64 {
			if round != nil {
				return round.UpstreamContentLength
			}
			return 0
		}(),
		UpstreamTransferEncoding: func() string {
			if round != nil {
				return round.UpstreamTransferEncoding
			}
			return ""
		}(),
	})
	if err == nil && h.metricsRegistry != nil {
		h.metricsRegistry.RecordClientUsage(clientauth.ClientIdentityFromContext(r.Context()).KeyID, 0, 0)
		h.metricsRegistry.SetUsageStoreHealthy(h.usageStore.Healthy())
	}
	if err != nil && !errors.Is(err, usage.ErrEventNotStarted) {
		if h.metricsRegistry != nil {
			h.metricsRegistry.RecordUsageStoreWriteError("complete")
		}
		slog.Error("usage store fallback complete failed", slog.String("event_id", eventID), slog.Any("error", err))
	}
}

// completeUsage 结算已 Start 的 event;失败只记日志/降级,不改变已写出的 HTTP 响应。
func (h *Handler) completeUsage(r *http.Request, requestID string, provider, model string, stream bool, status int, duration time.Duration, tok tokenUsage, outcome, errorCode string, round *archive.Round) bool {
	if h.usageStore == nil || requestID == "" {
		return false
	}
	if r != nil {
		if completion := usageCompletionFromContext(r.Context()); completion != nil {
			if !completion.done.CompareAndSwap(false, true) {
				return false
			}
		}
	}
	rec := usage.CompleteRecord{
		EventID:                  requestID,
		CompletedAt:              time.Now().UTC(),
		Provider:                 provider,
		Model:                    model,
		InputTokens:              int64(tok.PromptTokens),
		OutputTokens:             int64(tok.CompletionTokens),
		CachedInputTokens:        int64(tok.CachedInputTokens),
		CacheCreationInputTokens: int64(tok.CacheCreationInputTokens),
		HTTPStatus:               status,
		Outcome:                  outcome,
		ErrorCode:                errorCode,
		Duration:                 duration,
		Stream:                   stream,
		Estimated:                tok.Estimated,
	}
	if round != nil {
		rec.UpstreamProtocol = round.UpstreamProtocol
		rec.UpstreamEndpoint = round.UpstreamEndpoint
		rec.ConversionMode = round.ConversionMode
		rec.ConversionLevel = round.ConversionLevel
		rec.ConversionDuration = round.ConversionDuration
		rec.ConversionDegraded = round.ConversionDegraded
		rec.IgnoredFeatures = append([]string(nil), round.IgnoredFeatures...)
		rec.UnsupportedFeatures = append([]string(nil), round.UnsupportedFeatures...)
		rec.UpstreamDuration = round.UpstreamDuration
		rec.UpstreamStatus = round.UpstreamStatus
		rec.UpstreamContentType = round.UpstreamContentType
		rec.UpstreamContentLength = round.UpstreamContentLength
		rec.UpstreamTransferEncoding = round.UpstreamTransferEncoding
	}
	// When interaction archive is disabled, builtin settlement still needs
	// stable plan labels so usage dashboards can filter provider traffic.
	if rec.UpstreamProtocol == "" {
		switch provider {
		case effectivecatalog.BuiltinProviderID:
			rec.UpstreamProtocol = effectivecatalog.BuiltinProviderID
			if rec.UpstreamEndpoint == "" {
				rec.UpstreamEndpoint = chatGPTWebUsageEndpoint(r)
			}
			if rec.ConversionMode == "" {
				if r != nil && r.URL != nil && NormalizeClientEndpoint(r.URL.Path) == "/v1/responses" {
					rec.ConversionMode = TransportModeChatGPTWebResponses
				} else {
					rec.ConversionMode = TransportModeNative
				}
			}
		case effectivecatalog.CodexOAuthProviderID:
			rec.UpstreamProtocol = effectivecatalog.CodexOAuthProviderID
			if rec.UpstreamEndpoint == "" {
				rec.UpstreamEndpoint = "codex_oauth_responses"
			}
			if rec.ConversionMode == "" {
				rec.ConversionMode = TransportModeCodexOAuthResponses
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.usageStore.Complete(ctx, rec); err != nil {
		if h.metricsRegistry != nil {
			h.metricsRegistry.RecordUsageStoreWriteError("complete")
		}
		slog.Error("usage store complete failed",
			slog.String("event_id", requestID),
			slog.String("api_key_id", clientauth.ClientIdentityFromContext(r.Context()).KeyID),
			slog.Any("error", err),
		)
		return false
	}
	if h.metricsRegistry != nil {
		h.metricsRegistry.SetUsageStoreHealthy(h.usageStore.Healthy())
	}
	return true
}

func isRequestTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "exceeds limit")
}

// readLimitedBody 使用 MaxBytesReader 读取请求体,超限返回明确错误。
func (h *Handler) readLimitedBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	limit := h.currentConfig().MaxRequestBodyBytes
	if limit <= 0 {
		limit = config.DefaultMaxRequestBodyBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, fmt.Errorf("request body exceeds limit of %d bytes", limit)
		}
		return nil, err
	}
	return body, nil
}

// streamLimits 返回流式累计与单行上限。
func (h *Handler) streamLimits() (maxStream, maxLine int64) {
	maxStream = h.currentConfig().MaxStreamBytes
	if maxStream <= 0 {
		maxStream = config.DefaultMaxStreamBytes
	}
	maxLine = h.currentConfig().MaxSSELineBytes
	if maxLine <= 0 {
		maxLine = config.DefaultMaxSSELineBytes
	}
	return maxStream, maxLine
}

// readSSELine 读取一行 SSE,单行超过 maxLine 返回错误。
func readSSELine(reader *bufio.Reader, maxLine int64) ([]byte, error) {
	if maxLine <= 0 {
		maxLine = config.DefaultMaxSSELineBytes
	}
	var buf []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(chunk) > 0 {
			if int64(len(buf)+len(chunk)) > maxLine {
				return nil, fmt.Errorf("SSE line exceeds limit of %d bytes", maxLine)
			}
			buf = append(buf, chunk...)
		}
		if err == nil {
			return buf, nil
		}
		if err == bufio.ErrBufferFull {
			// ReadSlice 缓冲满,继续累积直到换行或超限。
			continue
		}
		if len(buf) > 0 {
			return buf, err
		}
		return nil, err
	}
}

// readLimitedUpstream 读取上游响应体并施加大小上限。
func (h *Handler) readLimitedUpstream(body io.Reader) ([]byte, error) {
	return h.readLimitedUpstreamContext(context.Background(), io.NopCloser(body), 0)
}

func isEventStreamContentType(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

// readLimitedUpstreamContext reads a buffered response while honoring both
// downstream cancellation and an idle timeout. A buffered response must not
// wait forever for EOF after an upstream has sent only a partial body.
func (h *Handler) readLimitedUpstreamContext(ctx context.Context, body io.ReadCloser, idleTimeout time.Duration) ([]byte, error) {
	limit := h.currentConfig().MaxUpstreamResponseBytes
	if limit <= 0 {
		limit = config.DefaultMaxUpstreamResponseBytes
	}
	if ctx == nil {
		ctx = context.Background()
	}
	type readResult struct {
		data []byte
		err  error
	}
	results := make(chan readResult, 1)
	data := make([]byte, 0, 64*1024)
	readNext := func() {
		go func() {
			readBuf := make([]byte, 32*1024)
			n, err := body.Read(readBuf)
			results <- readResult{data: append([]byte(nil), readBuf[:n]...), err: err}
		}()
	}
	readNext()
	var timer *time.Timer
	var timerC <-chan time.Time
	if idleTimeout > 0 {
		timer = time.NewTimer(idleTimeout)
		timerC = timer.C
		defer timer.Stop()
	}
	select {
	case <-ctx.Done():
		_ = body.Close()
		return nil, ctx.Err()
	case <-timerC:
		_ = body.Close()
		return nil, fmt.Errorf("upstream response body idle timeout after %s", idleTimeout.Truncate(time.Millisecond))
	case result := <-results:
		data = append(data, result.data...)
		if int64(len(data)) > limit {
			_ = body.Close()
			return nil, fmt.Errorf("upstream response exceeds limit of %d bytes", limit)
		}
		if result.err == io.EOF {
			return data, nil
		}
		if result.err != nil {
			return nil, result.err
		}
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idleTimeout)
		}
		readNext()
		for {
			select {
			case <-ctx.Done():
				_ = body.Close()
				return nil, ctx.Err()
			case <-timerC:
				_ = body.Close()
				return nil, fmt.Errorf("upstream response body idle timeout after %s", idleTimeout.Truncate(time.Millisecond))
			case result := <-results:
				data = append(data, result.data...)
				if int64(len(data)) > limit {
					_ = body.Close()
					return nil, fmt.Errorf("upstream response exceeds limit of %d bytes", limit)
				}
				if result.err == io.EOF {
					return data, nil
				}
				if result.err != nil {
					return nil, result.err
				}
				if timer != nil {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(idleTimeout)
				}
				readNext()
			}
		}
	}
}

// isSupportedInbound 限制客户端只能访问标准 OpenAI / Anthropic path。
func isSupportedInbound(method, path string) bool {
	switch path {
	case "/v1/responses":
		return method == http.MethodPost || method == http.MethodGet
	case "/v1/chat/completions", "/v1/messages", "/v1/responses/compact", "/v1/completions", "/v1/embeddings", "/v1/images/generations", "/v1/images/edits", "/v1/search":
		return method == http.MethodPost
	case "/v1/models":
		return method == http.MethodGet
	default:
		return false
	}
}

func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request, requestID string) {
	start := time.Now()
	round := archiveRoundFromContext(r.Context())
	bodyBytes, err := h.readLimitedBody(w, r)
	if err != nil {
		status := http.StatusBadRequest
		code := ErrorCodeInvalidRequest
		if isRequestTooLarge(err) {
			status = http.StatusRequestEntityTooLarge
			code = ErrorCodeRequestTooLarge
		}
		h.writeArchivedAPIError(w, round, r, start, "", "", false, status, APIError{
			Code: code, Message: err.Error(), ClientProtocol: clientProtocolFromRequest(r),
			ClientEndpoint: NormalizeClientEndpoint(r.URL.Path),
		})
		return
	}
	defer r.Body.Close()

	if len(bodyBytes) > 0 {
		stableHash, fingerprint := ComputeRequestFingerprint(bodyBytes)
		round.SetFingerprint(stableHash, fingerprint)
	}
	if err := h.writeArchiveRequest(round, bodyBytes); err != nil {
		log.Printf("archive request: %v", err)
	}
	h.archiveAndLogClientRequest(round, r, len(bodyBytes))

	var body map[string]any
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		h.writeArchivedError(w, round, r, start, "", "", false, http.StatusBadRequest, "invalid JSON request body")
		return
	}

	model, _ := body["model"].(string)
	stream, _ := body["stream"].(bool)
	plans, apiErr := h.resolveTransportPlans(r, model)
	if apiErr != nil {
		h.writeArchivedAPIError(w, round, r, start, "", model, stream, statusForAPIError(apiErr), *apiErr)
		return
	}
	plan := plans[0]
	if round != nil && plan.IsConversion() {
		round.SetTransportPlan(RouteLabel(r), plan.ClientEndpoint, plan.ClientProtocol, plan.UpstreamProtocol, plan.UpstreamEndpoint, plan.Mode)
		round.SetConversionLevel(plan.ConversionLevel)
	}
	if plan.UpstreamProtocol == effectivecatalog.BuiltinProviderID {
		// chatgptweb is an in-memory builtin provider, never a static
		// cfg.Providers entry. Route it before looking up static providers.
		h.archiveAndLogTransportPlan(round, r, plan, effectivecatalog.BuiltinProviderView(), stream)
		h.handleChatGPTWebChatCompletions(w, r, start, plan.RouteOwner, model, stream, body)
		return
	}
	if plan.Mode == TransportModeChatToCodex && plan.UpstreamProtocol == effectivecatalog.CodexOAuthProviderID {
		h.handleChatToCodex(w, r, start, plan, model, stream, body)
		return
	}
	candidates, preflightErr := h.prepareOpenAIChatCandidates(plans, bodyBytes, body, stream, r.URL.RawQuery, r.Method)
	if len(candidates) == 0 {
		if preflightErr != nil {
			h.writeArchivedAPIError(w, round, r, start, plan.RouteOwner, model, stream, statusForAPIError(preflightErr), *preflightErr)
			return
		}
		h.writeArchivedAPIError(w, round, r, start, plan.RouteOwner, model, stream, http.StatusBadRequest, APIError{Code: ErrorCodeEndpointUnsupported, Message: fmt.Sprintf("no compatible HTTP provider can serve %s", plan.ClientEndpoint), Model: model, ClientEndpoint: plan.ClientEndpoint, ClientProtocol: plan.ClientProtocol, UpstreamProtocol: plan.UpstreamProtocol})
		return
	}
	result, selected, err := h.doPreparedHTTPCandidates(r, round, candidates)
	if err != nil {
		if errors.Is(err, context.Canceled) && errors.Is(r.Context().Err(), context.Canceled) {
			h.recordAndPrint(round, r, plan.RouteOwner, model, stream, 0, time.Since(start), tokenUsage{}, "client_canceled")
			return
		}
		h.writeArchivedError(w, round, r, start, plan.RouteOwner, model, stream, http.StatusBadGateway, err.Error())
		return
	}
	resp := result.Response
	providerName := result.ProviderName
	provider := result.Provider
	selectedPlan := selected.Plan
	h.archiveAndLogTransportPlan(round, r, selectedPlan, provider, stream)
	if result.Cancel != nil {
		defer result.Cancel()
	}
	defer resp.Body.Close()
	if !stream && isEventStreamContentType(resp.Header.Get("Content-Type")) {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, "upstream_protocol_error: stream=false request returned event-stream")
		return
	}
	if selectedPlan.Mode == TransportModeOpenAIToAnthropic {
		if resp.StatusCode >= http.StatusBadRequest {
			h.writeConversionUpstreamError(w, r, resp, round, start, selectedPlan, providerName, model, stream)
			return
		}
		if stream {
			h.handleAnthropicStream(w, r, resp, round, start, providerName, model, body, r.Context(), result.Cancel)
			return
		}
		h.handleAnthropicBuffered(w, r, resp, round, start, providerName, model, body)
		return
	}
	if stream && resp.StatusCode < http.StatusBadRequest {
		h.handleStreamResponse(w, resp, round, start, providerName, model, body, r.Context(), result.Cancel, r)
		return
	}
	h.handleBufferedResponse(w, resp, round, start, providerName, model, stream, body, r)
}

func (h *Handler) startRound(apiKeyID string) (*archive.Round, error) {
	if h.interactionRecorder == nil {
		return nil, nil
	}
	if strings.TrimSpace(apiKeyID) == "" {
		return h.interactionRecorder.Start()
	}
	return h.interactionRecorder.StartForAPIKey(apiKeyID)
}

func (h *Handler) writeArchivedError(w http.ResponseWriter, round *archive.Round, r *http.Request, start time.Time, provider, model string, stream bool, status int, message string) {
	// 自由文本失败统一收敛为 typed envelope,避免 text/plain。
	code := ErrorCodeInvalidRequest
	switch status {
	case http.StatusRequestEntityTooLarge:
		code = ErrorCodeRequestTooLarge
	case http.StatusInternalServerError:
		code = ErrorCodeProxyInternalError
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		code = ErrorCodeUpstreamUnavailable
	default:
		if strings.Contains(strings.ToLower(message), "conversion") {
			code = ErrorCodeConversionUnsupported
		} else if status >= 500 {
			code = ErrorCodeProxyInternalError
		}
	}
	apiErr := APIError{
		Code:           code,
		Message:        message,
		Model:          model,
		ClientEndpoint: "",
		ClientProtocol: clientProtocolFromRequest(r),
	}
	if r != nil && r.URL != nil {
		apiErr.ClientEndpoint = NormalizeClientEndpoint(r.URL.Path)
	}
	if round != nil {
		round.SetUnsupportedFeatures(apiErr.UnsupportedFeatures)
		if apiErr.ClientEndpoint == "" {
			apiErr.ClientEndpoint = round.ClientEndpoint
		}
		if round.ClientProtocol != "" {
			apiErr.ClientProtocol = round.ClientProtocol
		}
		apiErr.UpstreamProtocol = round.UpstreamProtocol
	}
	h.writeArchivedAPIError(w, round, r, start, provider, model, stream, status, apiErr)
}

func (h *Handler) writeArchivedAPIError(w http.ResponseWriter, round *archive.Round, r *http.Request, start time.Time, provider, model string, stream bool, status int, apiErr APIError) {
	if apiErr.ClientProtocol == "" {
		apiErr.ClientProtocol = clientProtocolFromRequest(r)
	}
	if apiErr.ClientEndpoint == "" && r != nil && r.URL != nil {
		apiErr.ClientEndpoint = NormalizeClientEndpoint(r.URL.Path)
	}
	if apiErr.Model == "" {
		apiErr.Model = model
	}
	writeClientProtocolError(w, status, apiErr.ClientProtocol, apiErr)
	var body []byte
	if strings.EqualFold(apiErr.ClientProtocol, ClientProtocolAnthropic) {
		msg := apiErr.Message
		if apiErr.Code != "" && !strings.Contains(msg, apiErr.Code) {
			msg = apiErr.Code + ": " + msg
		}
		body, _ = json.Marshal(AnthropicErrorResponse{
			Type: "error",
			Error: AnthropicError{
				Type:    anthropicErrorType(apiErr.Code),
				Message: msg,
			},
		})
	} else {
		if apiErr.Type == "" {
			apiErr.Type = openAIErrorType(apiErr.Code)
		}
		body, _ = json.Marshal(APIErrorResponse{Error: apiErr})
	}
	body = append(body, '\n')
	if round != nil {
		if len(apiErr.UnsupportedFeatures) > 0 {
			round.UnsupportedFeatures = append([]string(nil), apiErr.UnsupportedFeatures...)
		}
		if err := h.writeArchiveResponse(round, "response.json", body); err != nil {
			log.Printf("archive api error response: %v", err)
		}
	}
	duration := time.Since(start)
	usage := tokenUsage{}
	msg := apiErr.Code + ": " + apiErr.Message
	h.recordAndPrint(round, r, provider, model, stream, status, duration, usage, msg)
	h.writeArchiveMetadata(round, provider, model, stream, status, duration, usage, "response.json", msg, "", "")
}

func chatGPTWebUsageEndpoint(r *http.Request) string {
	if r != nil && r.URL != nil && NormalizeClientEndpoint(r.URL.Path) == "/v1/responses" {
		return "chatgptweb_responses"
	}
	if r != nil && r.URL != nil && NormalizeClientEndpoint(r.URL.Path) == "/v1/search" {
		return "chatgptweb_search"
	}
	if r != nil && r.URL != nil && strings.HasPrefix(r.URL.Path, "/v1/images/") {
		return "chatgptweb_images"
	}
	return "chatgptweb"
}

func (h *Handler) forwardRaw(w http.ResponseWriter, r *http.Request, requestID string) {
	start := time.Now()
	round := archiveRoundFromContext(r.Context())
	body, err := h.readLimitedBody(w, r)
	if err != nil {
		status := http.StatusBadRequest
		code := ErrorCodeInvalidRequest
		if isRequestTooLarge(err) {
			status = http.StatusRequestEntityTooLarge
			code = ErrorCodeRequestTooLarge
		}
		h.writeArchivedAPIError(w, round, r, start, "", "", false, status, APIError{
			Code: code, Message: err.Error(), ClientProtocol: clientProtocolFromRequest(r),
			ClientEndpoint: NormalizeClientEndpoint(r.URL.Path),
		})
		return
	}
	defer r.Body.Close()
	rawBody, rawModel, rawStream := parseRawRequestBody(body)
	conversionBody := rawBody
	if NormalizeClientEndpoint(r.URL.Path) == "/v1/responses" {
		var request map[string]any
		if err := json.Unmarshal(body, &request); err == nil {
			// Validate an explicitly supplied reasoning object before routing, but
			// defer default-effort injection until we know the selected plan is
			// native. A conversion candidate must not receive a synthetic
			// reasoning field that it is required to reject.
			if _, present := request["reasoning"]; present {
				if err := h.applyModelReasoning(rawModel, request); err != nil {
					h.writeArchivedAPIError(w, round, r, start, "", rawModel, rawStream, http.StatusBadRequest, *err)
					return
				}
			}
		}
	}
	if err := h.writeArchiveRequest(round, body); err != nil {
		log.Printf("archive raw request: %v", err)
	}
	h.archiveAndLogClientRequest(round, r, len(body))
	// responses/completions/embeddings 仅允许矩阵中的 native 组合;TransportPlan 统一裁决。
	plans, apiErr := h.resolveTransportPlans(r, rawModel)
	if apiErr != nil {
		h.writeArchivedAPIError(w, round, r, start, "", rawModel, rawStream, statusForAPIError(apiErr), *apiErr)
		return
	}
	plan := plans[0]
	if !plan.IsConversion() {
		nativeRequest := map[string]any{}
		if err := json.Unmarshal(body, &nativeRequest); err == nil {
			rawBody = nativeRequest
		}
		metadata, hasMetadata := h.currentConfig().ModelMetadata[rawModel]
		_, hadReasoning := rawBody["reasoning"]
		injectDefaultReasoning := hasMetadata && metadata.ReasoningDeclared && metadata.ReasoningSupported && !hadReasoning && metadata.ReasoningDefaultEffort != ""
		if err := h.applyModelReasoning(rawModel, rawBody); err != nil {
			h.writeArchivedAPIError(w, round, r, start, "", rawModel, rawStream, http.StatusBadRequest, *err)
			return
		}
		if injectDefaultReasoning {
			if updated, err := json.Marshal(rawBody); err == nil {
				body = updated
				h.debugfRound(round, r, "native request default reasoning injected model=%s upstream_body_bytes=%d", rawModel, len(body))
			}
		}
	}
	// Record the declared conversion plan before semantic preflight so a
	// conversion_unsupported response still carries the real level in usage,
	// archive and metrics. A later selected candidate overwrites this snapshot.
	if round != nil && plan.IsConversion() {
		round.SetTransportPlan(RouteLabel(r), plan.ClientEndpoint, plan.ClientProtocol, plan.UpstreamProtocol, plan.UpstreamEndpoint, plan.Mode)
		round.SetConversionLevel(plan.ConversionLevel)
	}
	if plan.Mode == TransportModeChatGPTWebResponses && plan.UpstreamProtocol == effectivecatalog.BuiltinProviderID {
		h.archiveAndLogTransportPlan(round, r, plan, effectivecatalog.BuiltinProviderView(), rawStream)
		h.handleChatGPTWebResponses(w, r, start, plan.RouteOwner, rawModel, rawStream, rawBody)
		return
	}
	if plan.Mode == TransportModeCodexOAuthResponses && plan.UpstreamProtocol == effectivecatalog.CodexOAuthProviderID {
		codexBody, normalizedBody, ignored, features, normalizeErr := normalizeCodexHTTPRequest(body, false, r.Header)
		if normalizeErr != nil {
			h.writeArchivedError(w, round, r, start, plan.RouteOwner, rawModel, rawStream, http.StatusBadRequest, normalizeErr.Error())
			return
		}
		if round != nil {
			round.SetIgnoredFeatures(uniqueSortedFeatures(append(round.IgnoredFeatures, ignored...)))
		}
		sessionHash := codexSessionHash(r, rawModel, rawBody)
		codexBody, _, normalizeErr = ensureCodexPromptCacheKey(codexBody, normalizedBody, sessionHash)
		if normalizeErr != nil {
			h.writeArchivedError(w, round, r, start, plan.RouteOwner, rawModel, rawStream, http.StatusInternalServerError, normalizeErr.Error())
			return
		}
		if !rawStream {
			response, codexErr := h.codexResponses.CompleteCodexResponses(r.Context(), codexresponses.Request{Model: rawModel, Body: codexBody, SessionHash: sessionHash, RemoteCompactionV2: features.RemoteCompactionV2, ResponsesLite: features.ResponsesLite})
			if codexErr == nil {
				h.archiveAndLogTransportPlan(round, r, plan, effectivecatalog.BuiltinProviderViewFor(plan.RouteOwner), false)
				h.writeCodexOAuthCompleteSuccess(w, r, round, start, plan.RouteOwner, rawModel, rawBody, response)
				return
			}
			if failure, ok := codexresponses.AsFailure(codexErr); ok && failure.Kind == codexresponses.KindInvalidRequest {
				h.archiveAndLogTransportPlan(round, r, plan, effectivecatalog.BuiltinProviderViewFor(plan.RouteOwner), false)
				h.writeCodexResponsesError(w, r, round, start, plan.RouteOwner, rawModel, false, codexErr)
				return
			}
			h.recordCandidateFailure(r, plan, http.StatusBadGateway, time.Since(start))
			fallbackResult, selected, fallbackErr := h.doNativeUpstreamCandidates(r, round, plans[1:], body, len(body), false, r.URL.RawQuery, r.Method)
			if fallbackErr == nil && fallbackResult.Response != nil {
				if fallbackResult.Cancel != nil {
					defer fallbackResult.Cancel()
				}
				defer fallbackResult.Response.Body.Close()
				h.archiveAndLogTransportPlan(round, r, selected, fallbackResult.Provider, false)
				h.handleBufferedResponse(w, fallbackResult.Response, round, start, fallbackResult.ProviderName, rawModel, false, rawBody, r)
				return
			}
			h.archiveAndLogTransportPlan(round, r, plan, effectivecatalog.BuiltinProviderViewFor(plan.RouteOwner), false)
			h.writeCodexResponsesError(w, r, round, start, plan.RouteOwner, rawModel, false, codexErr)
			return
		}
		h.archiveAndLogTransportPlan(round, r, plan, effectivecatalog.BuiltinProviderViewFor(plan.RouteOwner), true)
		h.handleCodexOAuthResponses(w, r, start, plan.RouteOwner, rawModel, codexBody, rawBody, true, sessionHash, features)
		return
	}
	if hasTransportMode(plans, TransportModeResponsesToAnthropic) {
		candidates, preflightErr := h.prepareOpenAIResponsesCandidates(plans, body, conversionBody, rawStream, r.URL.RawQuery, r.Method)
		if len(candidates) == 0 {
			if preflightErr != nil {
				h.writeArchivedAPIError(w, round, r, start, plan.RouteOwner, rawModel, rawStream, http.StatusBadRequest, *preflightErr)
				return
			}
			h.writeArchivedAPIError(w, round, r, start, plan.RouteOwner, rawModel, rawStream, http.StatusBadRequest, APIError{Code: ErrorCodeConversionUnsupported, Message: "Responses to Anthropic conversion is unavailable", Feature: "responses_to_anthropic", UnsupportedFeatures: []string{"responses_to_anthropic"}})
			return
		}
		result, selected, err := h.doPreparedHTTPCandidates(r, round, candidates)
		if err != nil {
			if errors.Is(err, context.Canceled) && errors.Is(r.Context().Err(), context.Canceled) {
				h.settleConversionClientCanceled(round, r, selected.Plan.RouteOwner, rawModel, rawStream, start)
				return
			}
			h.writeArchivedError(w, round, r, start, plan.RouteOwner, rawModel, false, http.StatusBadGateway, err.Error())
			return
		}
		if result.Cancel != nil {
			defer result.Cancel()
		}
		defer result.Response.Body.Close()
		h.archiveAndLogTransportPlan(round, r, selected.Plan, result.Provider, rawStream)
		markConversionDegraded(round, selected.DegradedFeatures)
		if selected.Plan.Mode == TransportModeNative {
			h.handlePreparedNativeResponses(w, r, round, start, result, selected.Plan, rawModel, rawStream, rawBody)
			return
		}
		if result.Response.StatusCode >= http.StatusBadRequest {
			h.writeConversionUpstreamError(w, r, result.Response, round, start, selected.Plan, result.ProviderName, rawModel, rawStream)
			return
		}
		if rawStream {
			if err := h.handleResponsesToAnthropicStream(w, r, result.Response, round, start, result.ProviderName, rawModel, selected.ConversionCapability); err != nil {
				log.Printf("responses→anthropic stream: %v", err)
			}
			return
		}
		h.handleResponsesToAnthropic(w, r, result.Response, round, start, result.ProviderName, rawModel, selected.ConversionCapability)
		return
	}
	if plan.Mode == TransportModeAnthropicToResponses {
		// This endpoint is handled by handleAnthropicMessages, not forwardRaw.
		h.writeArchivedAPIError(w, round, r, start, plan.RouteOwner, rawModel, rawStream, http.StatusBadRequest, APIError{Code: ErrorCodeConversionUnsupported, Message: "Anthropic to Responses conversion must use the Messages handler", Feature: "anthropic_to_responses"})
		return
	}
	if plan.Mode != TransportModeNative {
		h.writeArchivedAPIError(w, round, r, start, plan.RouteOwner, rawModel, rawStream, http.StatusBadRequest, APIError{
			Code:             ErrorCodeEndpointUnsupported,
			Message:          fmt.Sprintf("endpoint %q only supports native transport; conversion is not available", plan.ClientEndpoint),
			Model:            rawModel,
			ClientEndpoint:   plan.ClientEndpoint,
			ClientProtocol:   plan.ClientProtocol,
			UpstreamProtocol: plan.UpstreamProtocol,
		})
		return
	}
	provider, ok := h.currentConfig().Providers[plan.RouteOwner]
	if !ok {
		h.writeArchivedAPIError(w, round, r, start, plan.RouteOwner, rawModel, rawStream, http.StatusServiceUnavailable, APIError{
			Code:             ErrorCodeProviderUnavailable,
			Message:          fmt.Sprintf("provider %q is not configured", plan.RouteOwner),
			Model:            rawModel,
			ClientEndpoint:   plan.ClientEndpoint,
			ClientProtocol:   plan.ClientProtocol,
			UpstreamProtocol: plan.UpstreamProtocol,
		})
		return
	}
	providerName := plan.RouteOwner
	h.debugfRound(round, r, "raw proxy client request method=%s path=%s query=%q provider=%s mode=%s remote=%s body_bytes=%d headers=%s",
		r.Method,
		r.URL.Path,
		r.URL.RawQuery,
		providerName,
		plan.Mode,
		r.RemoteAddr,
		len(body),
		headerSummary(sanitizeHeaders(r.Header)),
	)
	// 标准推理端点统一以请求体 stream=true 作为 SSE 开关。Accept 只描述客户端
	// 可接受的响应类型，不能把原本的非流式请求隐式改成长连接。
	streamRequest := rawStream
	result, plan, err := h.doNativeUpstreamCandidates(r, round, plans, body, len(body), streamRequest, r.URL.RawQuery, r.Method)
	if err != nil {
		if errors.Is(err, context.Canceled) && errors.Is(r.Context().Err(), context.Canceled) {
			h.recordAndPrint(round, r, providerName, rawModel, rawStream, 0, time.Since(start), tokenUsage{}, "client_canceled")
			return
		}
		if codexPlan, ok := codexFallbackPlan(plans, plan); ok {
			h.recordCandidateFailure(r, plan, http.StatusBadGateway, result.Duration)
			h.archiveAndLogTransportPlan(round, r, codexPlan, effectivecatalog.BuiltinProviderViewFor(codexPlan.RouteOwner), rawStream)
			codexBody, normalizedBody, ignored, features, normalizeErr := normalizeCodexHTTPRequest(body, false, r.Header)
			if normalizeErr != nil {
				h.writeArchivedError(w, round, r, start, codexPlan.RouteOwner, rawModel, rawStream, http.StatusBadRequest, normalizeErr.Error())
				return
			}
			if round != nil {
				round.SetIgnoredFeatures(uniqueSortedFeatures(append(round.IgnoredFeatures, ignored...)))
			}
			sessionHash := codexSessionHash(r, rawModel, rawBody)
			codexBody, _, keyErr := ensureCodexPromptCacheKey(codexBody, normalizedBody, sessionHash)
			if keyErr != nil {
				h.writeArchivedError(w, round, r, start, codexPlan.RouteOwner, rawModel, rawStream, http.StatusInternalServerError, keyErr.Error())
				return
			}
			h.handleCodexOAuthResponses(w, r, start, codexPlan.RouteOwner, rawModel, codexBody, rawBody, rawStream, sessionHash, features)
			return
		}
		h.writeArchivedError(w, round, r, start, providerName, rawModel, rawStream, http.StatusBadGateway, err.Error())
		return
	}
	if retryableUpstreamStatus(result.Response.StatusCode) {
		if codexPlan, ok := codexFallbackPlan(plans, plan); ok {
			h.recordCandidateFailure(r, plan, result.Response.StatusCode, result.Duration)
			_ = result.Response.Body.Close()
			if result.Cancel != nil {
				result.Cancel()
			}
			h.archiveAndLogTransportPlan(round, r, codexPlan, effectivecatalog.BuiltinProviderViewFor(codexPlan.RouteOwner), rawStream)
			codexBody, normalizedBody, ignored, features, normalizeErr := normalizeCodexHTTPRequest(body, false, r.Header)
			if normalizeErr != nil {
				h.writeArchivedError(w, round, r, start, codexPlan.RouteOwner, rawModel, rawStream, http.StatusBadRequest, normalizeErr.Error())
				return
			}
			if round != nil {
				round.SetIgnoredFeatures(uniqueSortedFeatures(append(round.IgnoredFeatures, ignored...)))
			}
			sessionHash := codexSessionHash(r, rawModel, rawBody)
			codexBody, _, keyErr := ensureCodexPromptCacheKey(codexBody, normalizedBody, sessionHash)
			if keyErr != nil {
				h.writeArchivedError(w, round, r, start, codexPlan.RouteOwner, rawModel, rawStream, http.StatusInternalServerError, keyErr.Error())
				return
			}
			h.handleCodexOAuthResponses(w, r, start, codexPlan.RouteOwner, rawModel, codexBody, rawBody, rawStream, sessionHash, features)
			return
		}
	}
	resp := result.Response
	providerName = result.ProviderName
	provider = result.Provider
	h.archiveAndLogTransportPlan(round, r, plan, provider, rawStream)
	if result.Cancel != nil {
		defer result.Cancel()
	}
	defer resp.Body.Close()
	h.debugfRound(round, r, "raw proxy upstream response provider=%s protocol=%s status=%d upstream_duration=%s total_duration=%s content_type=%q",
		providerName,
		provider.Protocol,
		resp.StatusCode,
		result.Duration.Truncate(time.Millisecond),
		time.Since(start).Truncate(time.Millisecond),
		resp.Header.Get("Content-Type"),
	)
	responsePath := responseFileName(resp.Header.Get("Content-Type"), strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "event-stream"))
	if strings.HasSuffix(responsePath, ".sse") {
		copyResponseHeader(w.Header(), resp.Header)
		prepareSSEHeaders(w.Header())
		w.WriteHeader(resp.StatusCode)
		usage, fullPath, streamErr := h.copyAndArchiveRawStream(w, resp, round, providerName, provider, rawModel, rawBody, r.Context(), result.Cancel, plan.UpstreamEndpoint)
		duration := time.Since(start)
		errMsg := ""
		if streamErr != nil {
			errMsg = streamErr.Error()
		}
		h.recordAndPrintFail(round, r, providerName, rawModel, true, resp.StatusCode, duration, usage, streamErr)
		h.writeArchiveMetadata(round, providerName, rawModel, true, resp.StatusCode, duration, usage, responsePath, errMsg, fullPath, outcomeFromStreamFail(streamErr, resp.StatusCode))
		return
	}
	responseBody, err := h.readLimitedUpstreamContext(r.Context(), resp.Body, h.currentConfig().UpstreamBodyIdleTimeout)
	if err != nil {
		if errors.Is(err, context.Canceled) && errors.Is(r.Context().Err(), context.Canceled) {
			h.recordAndPrint(round, r, providerName, rawModel, rawStream, 0, time.Since(start), tokenUsage{}, "client_canceled")
			return
		}
		h.writeArchivedError(w, round, r, start, providerName, rawModel, rawStream, http.StatusBadGateway, err.Error())
		return
	}
	responseBody, responseHeader, err := h.decodedResponseBodyAndHeader(responseBody, resp.Header)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, rawModel, rawStream, http.StatusBadGateway, err.Error())
		return
	}
	copyResponseHeader(w.Header(), responseHeader)
	w.WriteHeader(resp.StatusCode)
	if len(responseBody) > 0 {
		_, _ = w.Write(responseBody)
	}
	if err := h.writeArchiveResponse(round, responsePath, responseBody); err != nil {
		log.Printf("archive raw response: %v", err)
	}
	usage := tokenUsage{}
	if resp.StatusCode < 400 {
		usage = usageFromRawResponse(provider, responseBody, rawBody)
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, providerName, rawModel, rawStream, resp.StatusCode, duration, usage, "")
	h.writeArchiveMetadata(round, providerName, rawModel, rawStream, resp.StatusCode, duration, usage, responsePath, "", "", "")
}

func hasTransportMode(plans []TransportPlan, mode string) bool {
	for _, plan := range plans {
		if plan.Mode == mode {
			return true
		}
	}
	return false
}

func (h *Handler) handlePreparedNativeResponses(w http.ResponseWriter, r *http.Request, round *archive.Round, start time.Time, result upstreamResult, plan TransportPlan, model string, stream bool, requestBody map[string]any) {
	resp := result.Response
	if stream && isEventStreamContentType(resp.Header.Get("Content-Type")) {
		copyResponseHeader(w.Header(), resp.Header)
		prepareSSEHeaders(w.Header())
		w.WriteHeader(resp.StatusCode)
		usage, fullPath, streamErr := h.copyAndArchiveRawStream(w, resp, round, result.ProviderName, result.Provider, model, requestBody, r.Context(), result.Cancel, plan.UpstreamEndpoint)
		duration := time.Since(start)
		errMessage := ""
		if streamErr != nil {
			errMessage = streamErr.Error()
		}
		h.recordAndPrintFail(round, r, result.ProviderName, model, true, resp.StatusCode, duration, usage, streamErr)
		h.writeArchiveMetadata(round, result.ProviderName, model, true, resp.StatusCode, duration, usage, "response.sse", errMessage, fullPath, outcomeFromStreamFail(streamErr, resp.StatusCode))
		return
	}
	h.handleBufferedResponse(w, resp, round, start, result.ProviderName, model, stream, requestBody, r)
}

func (h *Handler) applyModelReasoning(model string, body map[string]any) *APIError {
	metadata, ok := h.currentConfig().ModelMetadata[model]
	if !ok || !metadata.ReasoningDeclared {
		return nil
	}
	raw, present := body["reasoning"]
	if !metadata.ReasoningSupported {
		if present {
			return &APIError{Code: ErrorCodeInvalidRequest, Message: "reasoning is not supported for model " + model, Feature: "reasoning", Model: model}
		}
		return nil
	}
	if !present && metadata.ReasoningDefaultEffort != "" {
		body["reasoning"] = map[string]any{"effort": metadata.ReasoningDefaultEffort}
		return nil
	}
	if !present {
		return nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return &APIError{Code: ErrorCodeInvalidRequest, Message: "reasoning must be an object", Feature: "reasoning", Model: model}
	}
	effort, _ := obj["effort"].(string)
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return nil
	}
	for _, allowed := range metadata.ReasoningEfforts {
		if effort == strings.ToLower(allowed) {
			return nil
		}
	}
	return &APIError{Code: ErrorCodeInvalidRequest, Message: "unsupported reasoning effort " + effort + " for model " + model, Feature: "reasoning.effort", Model: model}
}

func (h *Handler) handleBufferedResponse(w http.ResponseWriter, resp *http.Response, round *archive.Round, start time.Time, providerName, model string, stream bool, requestBody map[string]any, r *http.Request) {
	if !stream && isEventStreamContentType(resp.Header.Get("Content-Type")) {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, "upstream_protocol_error: stream=false request returned event-stream")
		return
	}
	responseBody, readErr := h.readLimitedUpstreamContext(r.Context(), resp.Body, h.currentConfig().UpstreamBodyIdleTimeout)
	if readErr != nil {
		if errors.Is(readErr, context.Canceled) && errors.Is(r.Context().Err(), context.Canceled) {
			h.recordAndPrint(round, r, providerName, model, stream, 0, time.Since(start), tokenUsage{}, "client_canceled")
			return
		}
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, readErr.Error())
		return
	}
	var responseHeader http.Header
	responseBody, responseHeader, readErr = h.decodedResponseBodyAndHeader(responseBody, resp.Header)
	if readErr != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, readErr.Error())
		return
	}
	if resp.StatusCode < http.StatusBadRequest && (r.URL.Path == "/v1/images/generations" || r.URL.Path == "/v1/images/edits") {
		if archiver, ok := h.chatGPTImage.(chatgptimage.ResponseArchiver); ok {
			identity := clientauth.ClientIdentityFromContext(r.Context())
			if err := archiver.ArchiveResponseImages(r.Context(), identity.KeyID, responseBody, imageBaseURL(r)); err != nil {
				h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, "image_archive_error: "+err.Error())
				return
			}
		}
	}
	copyResponseHeader(w.Header(), responseHeader)
	w.WriteHeader(resp.StatusCode)
	if len(responseBody) > 0 {
		_, _ = w.Write(responseBody)
	}

	usage := tokenUsage{}
	if resp.StatusCode < 400 {
		var payload struct {
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(responseBody, &payload); err == nil {
			usage, _ = usageFromRaw(payload.Usage)
		}
		if !usage.Known {
			usage = tokenUsage{
				PromptTokens:     estimatePromptTokens(requestBody),
				CompletionTokens: estimateCompletionTokensFromResponse(responseBody),
				Estimated:        true,
				Known:            true,
			}
		}
	}
	responsePath := responseFileName(resp.Header.Get("Content-Type"), false)
	if err := h.writeArchiveResponse(round, responsePath, responseBody); err != nil {
		log.Printf("archive response: %v", err)
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, providerName, model, stream, resp.StatusCode, duration, usage, "")
	h.writeArchiveMetadata(round, providerName, model, stream, resp.StatusCode, duration, usage, responsePath, "", "", "")
}

func (h *Handler) handleStreamResponse(w http.ResponseWriter, resp *http.Response, round *archive.Round, start time.Time, providerName, model string, requestBody map[string]any, requestContext context.Context, cancel context.CancelFunc, r *http.Request) {
	copyResponseHeader(w.Header(), resp.Header)
	prepareSSEHeaders(w.Header())
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	archiveWriter, err := h.createArchiveResponseWriter(round, "response.sse")
	if err != nil {
		log.Printf("archive stream response: %v", err)
	}
	if archiveWriter != nil {
		defer archiveWriter.Close()
	}

	reader := bufio.NewReader(resp.Body)
	accumulator := newOpenAIStreamAccumulator(model)
	maxStream, maxLine := h.streamLimits()
	accumulator.SetMaxContent(maxStream)
	idleTimer, stopIdleTimer := h.startStreamIdleTimer(cancel)
	defer stopIdleTimer()
	var streamErr *streamFail
	var totalBytes int64
	sawTerminal := false
	proto := streamProtocolForPath(r.URL.Path)
	for {
		line, err := readSSELine(reader, maxLine)
		if len(line) > 0 {
			resetStreamIdleTimer(idleTimer, h.currentConfig().StreamIdleTimeout)
			totalBytes += int64(len(line))
			if totalBytes > maxStream {
				streamErr = h.logStreamIssue(round, providerName, model, "read upstream stream limit", fmt.Errorf("stream exceeds limit of %d bytes", maxStream), requestContext, nil)
				break
			}
			term := parseTerminalSSELine(proto, line)
			if term.Terminal {
				sawTerminal = true
				if fail := streamFailFromTerminal(term); fail != nil {
					streamErr = fail
				}
			}
			accumulator.TrackSSELine(line)
			if _, writeErr := w.Write(line); writeErr != nil {
				streamErr = h.logStreamIssue(round, providerName, model, "write client stream", writeErr, requestContext, nil)
				break
			}
			if archiveWriter != nil {
				if _, writeErr := archiveWriter.Write(line); writeErr != nil {
					h.logStreamIssue(round, providerName, model, "write archive stream", writeErr, nil, nil)
				}
			}
			if flusher != nil {
				flusher.Flush()
			}
			if sawTerminal {
				break
			}
		}
		if err != nil {
			if err != io.EOF {
				streamErr = h.logStreamIssue(round, providerName, model, "read upstream stream", err, requestContext, idleTimer)
			} else if requiresTerminalEvent(proto) && !sawTerminal {
				// 干净 EOF 但未收到协议终止事件:视为上游截断。
				streamErr = h.logStreamIssue(round, providerName, model, "read upstream stream", fmt.Errorf("upstream stream ended without terminal event"), requestContext, idleTimer)
			}
			break
		}
	}

	usage := accumulator.FinalizeUsage(requestBody)
	if accumulator.Truncated() && streamErr == nil {
		streamErr = newStreamFail(streamKindLimitExceeded, fmt.Sprintf("stream full response truncated at %d bytes", maxStream), fmt.Errorf("truncated at %d bytes", maxStream), false)
	}
	fullPath := ""
	if streamErr == nil && !accumulator.Truncated() {
		if fullResponse, err := accumulator.ResponseJSON(); err != nil {
			log.Printf("build stream full response: %v", err)
		} else if err := h.writeArchiveResponse(round, "response.json", append(fullResponse, '\n')); err != nil {
			log.Printf("archive stream full response: %v", err)
		} else {
			fullPath = "response.json"
		}
	}
	duration := time.Since(start)
	errMsg := ""
	if streamErr != nil {
		errMsg = streamErr.Error()
	}
	h.recordAndPrintFail(round, r, providerName, model, true, resp.StatusCode, duration, usage, streamErr)
	h.writeArchiveMetadata(round, providerName, model, true, resp.StatusCode, duration, usage, "response.sse", errMsg, fullPath, outcomeFromStreamFail(streamErr, resp.StatusCode))
}

func trackSSELine(line []byte, usage *tokenUsage, content *strings.Builder) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return
	}
	if parsed, ok := usageFromMap(event["usage"]); ok {
		*usage = parsed
	}
	choices, _ := event["choices"].([]any)
	for _, item := range choices {
		choice, _ := item.(map[string]any)
		if delta, ok := choice["delta"].(map[string]any); ok {
			content.WriteString(flattenValue(delta["content"]))
		}
		if text, ok := choice["text"].(string); ok {
			content.WriteString(text)
		}
	}
}

func parseRawRequestBody(body []byte) (map[string]any, string, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", false
	}
	model, _ := payload["model"].(string)
	stream, _ := payload["stream"].(bool)
	return payload, model, stream
}

func (h *Handler) copyAndArchiveRawStream(w http.ResponseWriter, resp *http.Response, round *archive.Round, providerName string, provider config.Provider, model string, requestBody map[string]any, requestContext context.Context, cancel context.CancelFunc, requestPath string) (tokenUsage, string, *streamFail) {
	archiveWriter, err := h.createArchiveResponseWriter(round, "response.sse")
	if err != nil {
		log.Printf("archive raw stream response: %v", err)
	}
	if archiveWriter != nil {
		defer archiveWriter.Close()
	}
	var openAIAccumulator *openAIStreamAccumulator
	var responsesAccumulator *responsesStreamAccumulator
	var anthropicAccumulator *anthropicRawStreamAccumulator
	maxStream, maxLine := h.streamLimits()

	// 按入站 path 选择终止语义;Anthropic provider 强制 anthropic 语义。
	proto := streamProtocolForPath(requestPath)
	if provider.Protocol == "anthropic" {
		proto = streamProtoAnthropic
	}

	switch {
	case proto == streamProtoAnthropic || provider.Protocol == "anthropic":
		anthropicAccumulator = newAnthropicRawStreamAccumulator(model)
		anthropicAccumulator.SetMaxContent(maxStream)
	case proto == streamProtoResponses:
		responsesAccumulator = newResponsesStreamAccumulator(model)
		responsesAccumulator.SetMaxContent(maxStream)
	default:
		openAIAccumulator = newOpenAIStreamAccumulator(model)
		openAIAccumulator.SetMaxContent(maxStream)
	}

	reader := bufio.NewReader(resp.Body)
	idleTimer, stopIdleTimer := h.startStreamIdleTimer(cancel)
	defer stopIdleTimer()
	var streamErr *streamFail
	var totalBytes int64
	sawTerminal := false
	for {
		line, err := readSSELine(reader, maxLine)
		if len(line) > 0 {
			resetStreamIdleTimer(idleTimer, h.currentConfig().StreamIdleTimeout)
			totalBytes += int64(len(line))
			if totalBytes > maxStream {
				streamErr = h.logStreamIssue(round, providerName, model, "read raw stream limit", fmt.Errorf("stream exceeds limit of %d bytes", maxStream), requestContext, nil)
				break
			}
			term := parseTerminalSSELine(proto, line)
			if term.Terminal {
				sawTerminal = true
				if fail := streamFailFromTerminal(term); fail != nil {
					streamErr = fail
				}
			}
			if openAIAccumulator != nil {
				openAIAccumulator.TrackSSELine(line)
			}
			if responsesAccumulator != nil {
				responsesAccumulator.TrackSSELine(line)
			}
			if anthropicAccumulator != nil {
				anthropicAccumulator.TrackSSELine(line)
			}
			if _, writeErr := w.Write(line); writeErr != nil {
				streamErr = h.logStreamIssue(round, providerName, model, "write raw stream client", writeErr, requestContext, nil)
				break
			}
			if archiveWriter != nil {
				if _, writeErr := archiveWriter.Write(line); writeErr != nil {
					h.logStreamIssue(round, providerName, model, "write raw stream archive", writeErr, nil, nil)
				}
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			// 终止事件已写出后立即结束,避免 idle timeout 或后续脏数据覆盖结果。
			if sawTerminal {
				break
			}
		}
		if err != nil {
			if err != io.EOF {
				streamErr = h.logStreamIssue(round, providerName, model, "read raw stream", err, requestContext, idleTimer)
			} else if requiresTerminalEvent(proto) && !sawTerminal {
				streamErr = h.logStreamIssue(round, providerName, model, "read raw stream", fmt.Errorf("upstream stream ended without terminal event"), requestContext, idleTimer)
			}
			break
		}
	}

	if openAIAccumulator != nil {
		usage := openAIAccumulator.FinalizeUsage(requestBody)
		if openAIAccumulator.Truncated() && streamErr == nil {
			streamErr = newStreamFail(streamKindLimitExceeded, fmt.Sprintf("stream full response truncated at %d bytes", maxStream), fmt.Errorf("truncated at %d bytes", maxStream), false)
		}
		fullPath := ""
		if streamErr == nil && !openAIAccumulator.Truncated() && proto == streamProtoChatCompletions {
			if fullResponse, err := openAIAccumulator.ResponseJSON(); err != nil {
				log.Printf("build raw stream full response: %v", err)
			} else if err := h.writeArchiveResponse(round, "response.json", append(fullResponse, '\n')); err != nil {
				log.Printf("archive raw stream full response: %v", err)
			} else {
				fullPath = "response.json"
			}
		}
		return usage, fullPath, streamErr
	}
	if responsesAccumulator != nil {
		usage := responsesAccumulator.FinalizeUsage(requestBody)
		if responsesAccumulator.Truncated() && streamErr == nil {
			streamErr = newStreamFail(streamKindLimitExceeded, fmt.Sprintf("stream full response truncated at %d bytes", maxStream), fmt.Errorf("truncated at %d bytes", maxStream), false)
		}
		return usage, "", streamErr
	}
	usage := anthropicAccumulator.FinalizeUsage(requestBody)
	if anthropicAccumulator.Truncated() && streamErr == nil {
		streamErr = newStreamFail(streamKindLimitExceeded, fmt.Sprintf("stream full response truncated at %d bytes", maxStream), fmt.Errorf("truncated at %d bytes", maxStream), false)
	}
	fullPath := ""
	if streamErr == nil && !anthropicAccumulator.Truncated() {
		if fullResponse, err := anthropicAccumulator.ResponseJSON(usage); err != nil {
			log.Printf("build anthropic raw stream full response: %v", err)
		} else if err := h.writeArchiveResponse(round, "response.json", append(fullResponse, '\n')); err != nil {
			log.Printf("archive anthropic raw stream full response: %v", err)
		} else {
			fullPath = "response.json"
		}
	}
	return usage, fullPath, streamErr
}

type upstreamResult struct {
	ProviderName string
	Provider     config.Provider
	Response     *http.Response
	Duration     time.Duration
	Cancel       context.CancelFunc
}

// preparedHTTPCandidate is an already preflighted request attempt. Payload
// construction happens before any upstream call, so an unsupported conversion
// may be skipped in favour of a lower native candidate without losing request
// semantics or committing a response.
type preparedHTTPCandidate struct {
	Plan                 TransportPlan
	Provider             config.Provider
	Body                 []byte
	Stream               bool
	RawQuery             string
	Method               string
	ConversionCapability config.ConversionCapability
	DegradedFeatures     []string
}

func (h *Handler) prepareOpenAIChatCandidates(plans []TransportPlan, raw []byte, body map[string]any, stream bool, rawQuery, method string) ([]preparedHTTPCandidate, *APIError) {
	result := make([]preparedHTTPCandidate, 0, len(plans))
	var firstErr *APIError
	for _, plan := range plans {
		provider, ok := h.currentConfig().Providers[plan.RouteOwner]
		if !ok || provider.Disabled {
			continue
		}
		switch plan.Mode {
		case TransportModeNative:
			if plan.UpstreamProtocol != "openai" {
				continue
			}
			result = append(result, preparedHTTPCandidate{Plan: plan, Provider: provider, Body: raw, Stream: stream, RawQuery: rawQuery, Method: method})
		case TransportModeOpenAIToAnthropic:
			if apiErr := ValidateConversionRequest(plan, body); apiErr != nil {
				if firstErr == nil {
					copyErr := *apiErr
					firstErr = &copyErr
				}
				continue
			}
			payload, err := buildAnthropicRequest(body, plan.ModelID, stream)
			if err != nil {
				apiErr := conversionAPIError(plan, err)
				if firstErr == nil {
					firstErr = &apiErr
				}
				continue
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				apiErr := APIError{Code: ErrorCodeProxyInternalError, Message: err.Error(), Model: plan.ModelID, ClientEndpoint: plan.ClientEndpoint, ClientProtocol: plan.ClientProtocol, UpstreamProtocol: plan.UpstreamProtocol}
				if firstErr == nil {
					firstErr = &apiErr
				}
				continue
			}
			result = append(result, preparedHTTPCandidate{Plan: plan, Provider: provider, Body: encoded, Stream: stream, Method: http.MethodPost})
		}
	}
	return result, firstErr
}

func (h *Handler) prepareAnthropicMessageCandidates(plans []TransportPlan, raw []byte, body map[string]any, stream bool, rawQuery, method string) ([]preparedHTTPCandidate, *APIError) {
	result := make([]preparedHTTPCandidate, 0, len(plans))
	var firstErr *APIError
	for _, plan := range plans {
		provider, ok := h.currentConfig().Providers[plan.RouteOwner]
		if !ok || provider.Disabled {
			continue
		}
		switch plan.Mode {
		case TransportModeNative:
			if plan.UpstreamProtocol != "anthropic" {
				continue
			}
			result = append(result, preparedHTTPCandidate{Plan: plan, Provider: provider, Body: raw, Stream: stream, RawQuery: rawQuery, Method: method})
		case TransportModeAnthropicToOpenAI:
			if apiErr := ValidateConversionRequest(plan, body); apiErr != nil {
				if firstErr == nil {
					copyErr := *apiErr
					firstErr = &copyErr
				}
				continue
			}
			encoded, err := buildOpenAIChatFromAnthropic(body, plan.ModelID, stream)
			if err != nil {
				apiErr := conversionAPIError(plan, err)
				if firstErr == nil {
					firstErr = &apiErr
				}
				continue
			}
			result = append(result, preparedHTTPCandidate{Plan: plan, Provider: provider, Body: encoded, Stream: stream, Method: http.MethodPost})
		case TransportModeAnthropicToResponses:
			if err := h.validateDeclaredConversionCapability(plan, body, stream); err != nil {
				apiErr := conversionAPIError(plan, err)
				if firstErr == nil {
					firstErr = &apiErr
				}
				continue
			}
			capability, _ := h.declaredConversionCapability(plan)
			conversionBody := h.withBoundedDefaultOutputLimit(plan, body)
			encoded, degraded, err := buildResponsesFromAnthropicWithCapability(conversionBody, plan.ModelID, stream, capability)
			if err != nil {
				apiErr := conversionAPIError(plan, err)
				if firstErr == nil {
					firstErr = &apiErr
				}
				continue
			}
			if metadata, ok := h.currentConfig().ModelMetadata[plan.ModelID]; ok {
				encoded, err = disableResponsesReasoningForOmittedAnthropicThinking(conversionBody, encoded, metadata)
				if err != nil {
					apiErr := conversionAPIError(plan, err)
					if firstErr == nil {
						firstErr = &apiErr
					}
					continue
				}
			}
			result = append(result, preparedHTTPCandidate{Plan: plan, Provider: provider, Body: encoded, Stream: stream, Method: http.MethodPost, ConversionCapability: capability, DegradedFeatures: degraded})
		}
	}
	return result, firstErr
}

func (h *Handler) prepareOpenAIResponsesCandidates(plans []TransportPlan, raw []byte, body map[string]any, stream bool, rawQuery, method string) ([]preparedHTTPCandidate, *APIError) {
	result := make([]preparedHTTPCandidate, 0, len(plans))
	var firstErr *APIError
	for _, plan := range plans {
		provider, ok := h.currentConfig().Providers[plan.RouteOwner]
		if !ok || provider.Disabled {
			continue
		}
		switch plan.Mode {
		case TransportModeNative:
			if plan.UpstreamProtocol == "openai" {
				result = append(result, preparedHTTPCandidate{Plan: plan, Provider: provider, Body: raw, Stream: stream, RawQuery: rawQuery, Method: method})
			}
		case TransportModeResponsesToAnthropic:
			if err := h.validateDeclaredConversionCapability(plan, body, stream); err != nil {
				apiErr := conversionAPIError(plan, err)
				if firstErr == nil {
					firstErr = &apiErr
				}
				continue
			}
			capability, _ := h.declaredConversionCapability(plan)
			conversionBody := h.withBoundedDefaultOutputLimit(plan, body)
			encoded, degraded, err := buildAnthropicFromResponsesWithCapability(conversionBody, plan.ModelID, stream, capability)
			if err != nil {
				apiErr := conversionAPIError(plan, err)
				if firstErr == nil {
					firstErr = &apiErr
				}
				continue
			}
			result = append(result, preparedHTTPCandidate{Plan: plan, Provider: provider, Body: encoded, Stream: stream, Method: http.MethodPost, ConversionCapability: capability, DegradedFeatures: degraded})
		}
	}
	return result, firstErr
}

func (h *Handler) withBoundedDefaultOutputLimit(plan TransportPlan, body map[string]any) map[string]any {
	field := "max_output_tokens"
	if plan.Mode == TransportModeAnthropicToResponses {
		field = "max_tokens"
	}
	if _, exists := body[field]; exists {
		return body
	}
	metadata, ok := h.currentConfig().ModelMetadata[plan.ModelID]
	if !ok || metadata.MaxOutputTokens <= 0 || metadata.MaxOutputTokens >= 4096 {
		return body
	}
	bounded := make(map[string]any, len(body)+1)
	for key, value := range body {
		bounded[key] = value
	}
	bounded[field] = metadata.MaxOutputTokens
	return bounded
}

func (h *Handler) validateDeclaredConversionCapability(plan TransportPlan, body map[string]any, stream bool) error {
	capability, err := h.declaredConversionCapability(plan)
	if err != nil {
		return err
	}
	if stream && !capability.Streaming {
		return fmt.Errorf("stream")
	}
	needsTools := conversionRequestNeedsTools(plan.Mode, body)
	if stream && needsTools {
		return fmt.Errorf("streaming tools")
	}
	if needsTools && !capability.Tools {
		return fmt.Errorf("tools")
	}
	metadata := h.currentConfig().ModelMetadata[plan.ModelID]
	if metadata.MaxOutputTokens > 0 {
		field := "max_output_tokens"
		if plan.Mode == TransportModeAnthropicToResponses {
			field = "max_tokens"
		}
		if raw, exists := body[field]; exists {
			value, ok := numberAsInt(raw)
			if !ok || value <= 0 || value > metadata.MaxOutputTokens {
				return fmt.Errorf("%s exceeds model limit %d", field, metadata.MaxOutputTokens)
			}
		}
	}
	return nil
}

func (h *Handler) declaredConversionCapability(plan TransportPlan) (config.ConversionCapability, error) {
	metadata, ok := h.currentConfig().ModelMetadata[plan.ModelID]
	if !ok {
		return config.ConversionCapability{}, fmt.Errorf("conversion capability is not declared")
	}
	capability, ok := config.ModelConversionCapability(metadata, plan.UpstreamEndpoint, plan.Mode)
	if !ok {
		return config.ConversionCapability{}, fmt.Errorf("conversion capability is not available")
	}
	return capability, nil
}

func markConversionDegraded(round *archive.Round, features []string) {
	if round == nil || len(features) == 0 {
		return
	}
	round.SetConversionDegraded(true)
	round.SetIgnoredFeatures(uniqueSortedFeatures(append(round.IgnoredFeatures, features...)))
}

func conversionRequestNeedsTools(direction string, body map[string]any) bool {
	if hasNonEmptyConversionFeature(body["tools"]) {
		return true
	}
	if choice, ok := body["tool_choice"].(string); ok {
		if choice != "" && choice != "none" {
			return true
		}
	} else if choice, ok := body["tool_choice"].(map[string]any); ok && len(choice) > 0 {
		return true
	}
	if direction == TransportModeResponsesToAnthropic {
		items, _ := body["input"].([]any)
		for _, raw := range items {
			if item, ok := raw.(map[string]any); ok {
				typ, _ := item["type"].(string)
				if typ == "function_call" || typ == "function_call_output" {
					return true
				}
			}
		}
		return false
	}
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if hasNonEmptyConversionFeature(message["tool_calls"]) {
			return true
		}
		if blocks, ok := message["content"].([]any); ok {
			for _, rawBlock := range blocks {
				if block, ok := rawBlock.(map[string]any); ok {
					typ, _ := block["type"].(string)
					if typ == "tool_use" || typ == "tool_result" {
						return true
					}
				}
			}
		}
	}
	return false
}

func (h *Handler) doPreparedHTTPCandidates(r *http.Request, round *archive.Round, candidates []preparedHTTPCandidate) (upstreamResult, preparedHTTPCandidate, error) {
	var lastErr error
	var last preparedHTTPCandidate
	for index, candidate := range candidates {
		result, err := h.doUpstreamPath(r, round, candidate.Plan.RouteOwner, candidate.Provider, candidate.Body, len(candidate.Body), candidate.Stream, candidate.Plan.UpstreamEndpoint, candidate.RawQuery, candidate.Method)
		if err != nil {
			last, lastErr = candidate, err
			if errors.Is(err, context.Canceled) && errors.Is(r.Context().Err(), context.Canceled) {
				return result, candidate, err
			}
			if index < len(candidates)-1 {
				h.recordCandidateFailure(r, candidate.Plan, http.StatusBadGateway, result.Duration)
			}
			continue
		}
		if retryableUpstreamStatus(result.Response.StatusCode) {
			if index == len(candidates)-1 {
				return result, candidate, nil
			}
			h.recordCandidateFailure(r, candidate.Plan, result.Response.StatusCode, result.Duration)
			_ = result.Response.Body.Close()
			if result.Cancel != nil {
				result.Cancel()
			}
			last = candidate
			lastErr = fmt.Errorf("upstream %s returned %d", candidate.Plan.RouteOwner, result.Response.StatusCode)
			continue
		}
		return result, candidate, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no prepared HTTP provider is eligible")
	}
	return upstreamResult{}, last, lastErr
}

// doNativeUpstreamCandidates executes direct HTTP candidates that preserve the
// incoming protocol body. It advances only before a client response is
// committed; conversion and builtin executors have request-specific semantic
// gates and must opt in separately.
func (h *Handler) doNativeUpstreamCandidates(r *http.Request, round *archive.Round, plans []TransportPlan, body []byte, bodyBytes int, stream bool, rawQuery, method string) (upstreamResult, TransportPlan, error) {
	candidates := make([]TransportPlan, 0, len(plans))
	for _, plan := range plans {
		if plan.Mode == TransportModeNative && plan.UpstreamProtocol != effectivecatalog.BuiltinProviderID && plan.UpstreamProtocol != effectivecatalog.CodexOAuthProviderID {
			candidates = append(candidates, plan)
		}
	}
	var lastErr error
	var lastPlan TransportPlan
	for index, plan := range candidates {
		provider, ok := h.currentConfig().Providers[plan.RouteOwner]
		if !ok || provider.Disabled {
			lastErr = fmt.Errorf("provider %q is unavailable", plan.RouteOwner)
			lastPlan = plan
			continue
		}
		result, err := h.doUpstreamPath(r, round, plan.RouteOwner, provider, body, bodyBytes, stream, plan.UpstreamEndpoint, rawQuery, method)
		if err != nil {
			lastErr = err
			lastPlan = plan
			if errors.Is(err, context.Canceled) && errors.Is(r.Context().Err(), context.Canceled) {
				return result, plan, err
			}
			if index < len(candidates)-1 && h.metricsRegistry != nil {
				recordRequestPlanMetric(h.metricsRegistry, plan, RouteLabel(r), http.StatusBadGateway, result.Duration, "upstream_failed")
			}
			continue
		}
		if retryableUpstreamStatus(result.Response.StatusCode) {
			lastErr = fmt.Errorf("upstream %s returned %d", plan.RouteOwner, result.Response.StatusCode)
			lastPlan = plan
			if index == len(candidates)-1 {
				// There is no eligible fallback left. Preserve the upstream status
				// and response rather than collapsing it into a synthetic 502.
				return result, plan, nil
			}
			if h.metricsRegistry != nil {
				recordRequestPlanMetric(h.metricsRegistry, plan, RouteLabel(r), result.Response.StatusCode, result.Duration, "upstream_failed")
			}
			_ = result.Response.Body.Close()
			if result.Cancel != nil {
				result.Cancel()
			}
			continue
		}
		return result, plan, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no direct native provider is eligible for model %q", firstPlanModel(plans))
	}
	return upstreamResult{}, lastPlan, lastErr
}

func retryableUpstreamStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func firstPlanModel(plans []TransportPlan) string {
	if len(plans) == 0 {
		return ""
	}
	return plans[0].ModelID
}

// codexFallbackPlan returns the next enabled native Codex executor candidate
// after a failed direct HTTP plan. It is only used by /v1/responses, where the
// Codex port preserves the native Responses object and no conversion occurs.
func codexFallbackPlan(plans []TransportPlan, after TransportPlan) (TransportPlan, bool) {
	foundAfter := false
	for _, plan := range plans {
		if !foundAfter {
			if plan.RouteOwner == after.RouteOwner && plan.UpstreamEndpoint == after.UpstreamEndpoint && plan.Mode == after.Mode {
				foundAfter = true
			}
			continue
		}
		if plan.Mode == TransportModeCodexOAuthResponses && plan.UpstreamProtocol == effectivecatalog.CodexOAuthProviderID && plan.Fallback {
			return plan, true
		}
	}
	return TransportPlan{}, false
}

func (h *Handler) recordCandidateFailure(r *http.Request, plan TransportPlan, status int, duration time.Duration) {
	if h == nil || h.metricsRegistry == nil || strings.TrimSpace(plan.RouteOwner) == "" {
		return
	}
	recordRequestPlanMetric(h.metricsRegistry, plan, RouteLabel(r), status, duration, "upstream_failed")
}

func recordRequestPlanMetric(reg metricsport.Port, plan TransportPlan, route string, status int, duration time.Duration, outcome string) {
	if reg == nil {
		return
	}
	if levelReporter, ok := reg.(metricsport.PlanLevelReporter); ok {
		levelReporter.RecordRequestPlanWithLevel(plan.RouteOwner, plan.ModelID, route, status, duration, outcome,
			plan.ClientEndpoint, plan.UpstreamProtocol, plan.UpstreamEndpoint, plan.Mode, plan.ConversionLevel)
		return
	}
	reg.RecordRequestPlan(plan.RouteOwner, plan.ModelID, route, status, duration, outcome,
		plan.ClientEndpoint, plan.UpstreamProtocol, plan.UpstreamEndpoint, plan.Mode)
}

func (h *Handler) doUpstream(r *http.Request, round *archive.Round, providerName string, provider config.Provider, body []byte, bodyBytes int, stream bool) (upstreamResult, error) {
	return h.doUpstreamPath(r, round, providerName, provider, body, bodyBytes, stream, r.URL.Path, r.URL.RawQuery, r.Method)
}

func (h *Handler) doUpstreamPath(r *http.Request, round *archive.Round, providerName string, provider config.Provider, body []byte, bodyBytes int, stream bool, path, rawQuery, method string) (upstreamResult, error) {
	// A caller may execute one candidate or the bounded safe native candidate
	// chain. This helper itself owns only one upstream HTTP attempt.
	ctx, cancel := h.upstreamContext(r.Context(), stream)
	req, err := h.newUpstreamRequestForPath(ctx, r, provider, body, path, rawQuery, method, stream)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return upstreamResult{}, err
	}
	h.archiveAndLogUpstreamRequest(round, r, providerName, provider, req, bodyBytes)
	h.debugfRound(round, r, "upstream request provider=%s protocol=%s method=%s url=%s body_bytes=%d",
		providerName,
		provider.Protocol,
		req.Method,
		req.URL.String(),
		bodyBytes,
	)

	upstreamStart := time.Now()
	client := h.currentClient()
	if client == nil {
		return upstreamResult{}, fmt.Errorf("upstream client is unavailable")
	}
	resp, err := client.Do(req)
	duration := time.Since(upstreamStart)
	if resp != nil {
		contentType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
		transferEncoding := strings.Join(resp.TransferEncoding, ",")
		h.debugfRound(round, r, "upstream response headers provider=%s status=%d content_type=%q content_length=%d transfer_encoding=%q header_duration=%s", providerName, resp.StatusCode, contentType, resp.ContentLength, transferEncoding, duration.Truncate(time.Millisecond))
		if round != nil {
			round.SetUpstreamHeaders(resp.StatusCode, contentType, resp.ContentLength, transferEncoding, duration)
		}
	}
	if round != nil {
		round.SetUpstreamDuration(duration)
	}
	h.archiveAndLogUpstreamResponse(round, r, providerName, provider, resp, duration, err)
	if h.metricsRegistry != nil && resp != nil && resp.StatusCode >= 400 {
		h.metricsRegistry.RecordUpstreamError(providerName, resp.StatusCode)
	}
	if err != nil {
		if cancel != nil {
			cancel()
		}
		if h.metricsRegistry != nil {
			h.metricsRegistry.RecordUpstreamAttempt(providerName, duration, metrics.AttemptHeader)
			if !errors.Is(err, context.Canceled) || !errors.Is(r.Context().Err(), context.Canceled) {
				h.metricsRegistry.RecordUpstreamError(providerName, -1)
			}
		}
		return upstreamResult{}, err
	}
	if resp.StatusCode < http.StatusBadRequest {
		isSSE := isEventStreamContentType(resp.Header.Get("Content-Type"))
		if stream != isSSE {
			_ = resp.Body.Close()
			if cancel != nil {
				cancel()
			}
			if h.metricsRegistry != nil {
				h.metricsRegistry.RecordUpstreamAttempt(providerName, duration, metrics.AttemptHeader)
				h.metricsRegistry.RecordUpstreamError(providerName, -1)
			}
			if stream {
				return upstreamResult{}, fmt.Errorf("upstream_protocol_error: stream=true request returned non-event-stream response")
			}
			return upstreamResult{}, fmt.Errorf("upstream_protocol_error: stream=false request returned event-stream")
		}
	}

	// 流式请求在写出首包前探测完整首行；失败返回调用方，由候选执行器
	// 在尚未提交客户端响应时决定是否安全切换 RouteOwner。
	if stream && resp.StatusCode < 400 {
		_, maxLine := h.streamLimits()
		primed, peekErr := primeStreamBody(resp, h.currentConfig().StreamFirstEventTimeout, maxLine)
		duration = time.Since(upstreamStart)
		if peekErr != nil {
			_ = resp.Body.Close()
			if cancel != nil {
				cancel()
			}
			if h.metricsRegistry != nil {
				h.metricsRegistry.RecordUpstreamAttempt(providerName, duration, metrics.AttemptFirstEvent)
				if !errors.Is(peekErr, context.Canceled) || !errors.Is(r.Context().Err(), context.Canceled) {
					h.metricsRegistry.RecordUpstreamError(providerName, -1)
				}
			}
			return upstreamResult{}, peekErr
		}
		resp = primed
	}

	// 流式成功: first_event;非流式: header。
	kind := metrics.AttemptHeader
	if stream {
		kind = metrics.AttemptFirstEvent
	}
	if h.metricsRegistry != nil {
		h.metricsRegistry.RecordUpstreamAttempt(providerName, duration, kind)
	}
	return upstreamResult{ProviderName: providerName, Provider: provider, Response: resp, Duration: duration, Cancel: cancel}, nil
}

// primeStreamBody 在 timeout 内读取上游首个有效 SSE data 事件，成功后把探测字节回填到 Body。
// 空行、event 行和 comment 不构成首事件，不能用来无限续期首事件超时。
// 非 SSE 响应仍只探测首个完整行，由后续协议处理器给出 Content-Type 错误。
// timeout<=0 时使用 30s 兜底,避免永久阻塞。
func primeStreamBody(resp *http.Response, timeout time.Duration, maxLine int64) (*http.Response, error) {
	if resp == nil || resp.Body == nil {
		return resp, fmt.Errorf("empty upstream stream body")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if maxLine <= 0 {
		maxLine = config.DefaultMaxSSELineBytes
	}

	type peekResult struct {
		prefix []byte
		err    error
	}
	ch := make(chan peekResult, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		var prefix []byte
		requireDataEvent := isEventStreamContentType(resp.Header.Get("Content-Type"))
		for {
			line, err := readSSELine(reader, maxLine)
			// 必须读到完整换行; partial EOF 不视为成功。
			if err != nil {
				ch <- peekResult{prefix: prefix, err: err}
				return
			}
			if len(line) == 0 || line[len(line)-1] != '\n' {
				ch <- peekResult{err: fmt.Errorf("upstream stream closed before complete first SSE line")}
				return
			}
			prefix = append(prefix, line...)
			if int64(len(prefix)) > maxLine*4 {
				ch <- peekResult{err: fmt.Errorf("upstream stream preamble exceeds limit of %d bytes", maxLine*4)}
				return
			}
			if !requireDataEvent {
				break
			}
			trimmed := strings.TrimSpace(string(line))
			if strings.HasPrefix(trimmed, "data:") && strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")) != "" {
				break
			}
		}
		// 把 reader 缓冲中已预读的内容拼进 prefix,再接回原始 Body。
		var extra []byte
		if buffered := reader.Buffered(); buffered > 0 {
			extra = make([]byte, buffered)
			_, _ = io.ReadFull(reader, extra)
		}
		prefix = append(prefix, extra...)
		ch <- peekResult{prefix: prefix}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.err != nil {
			// 超限/EOF/其它读错误:不接受 partial 数据。
			if res.err == io.EOF {
				return nil, fmt.Errorf("upstream stream closed before complete first SSE line")
			}
			return nil, res.err
		}
		if len(res.prefix) == 0 {
			return nil, fmt.Errorf("upstream stream closed before first SSE line")
		}
		resp.Body = &prefixReadCloser{prefix: res.prefix, rest: resp.Body}
		return resp, nil
	case <-timer.C:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("upstream stream first event timeout after %s", timeout.Truncate(time.Millisecond))
	}
}

type prefixReadCloser struct {
	prefix []byte
	rest   io.ReadCloser
}

func (p *prefixReadCloser) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.rest.Read(b)
}

func (p *prefixReadCloser) Close() error {
	if p.rest == nil {
		return nil
	}
	return p.rest.Close()
}

func providerSupportsInboundPath(provider config.Provider, path string) bool {
	return config.ProviderSupportsInboundPath(provider, path)
}

func (h *Handler) upstreamContext(parent context.Context, stream bool) (context.Context, context.CancelFunc) {
	if stream {
		return context.WithCancel(parent)
	}
	requestTimeout := h.currentConfig().RequestTimeout
	if requestTimeout > 0 {
		return context.WithTimeout(parent, requestTimeout)
	}
	return parent, nil
}

func (h *Handler) newUpstreamRequest(ctx context.Context, r *http.Request, provider config.Provider, body []byte) (*http.Request, error) {
	return h.newUpstreamRequestForPath(ctx, r, provider, body, r.URL.Path, r.URL.RawQuery, r.Method, false)
}

// newUpstreamRequestForPath 按指定上游 path 构建请求,用于协议转换时改写目标路径。
// 请求头使用 upstream protocol allowlist 构造,不得先复制全部入站 header 再 blocklist 删除。
func (h *Handler) newUpstreamRequestForPath(ctx context.Context, r *http.Request, provider config.Provider, body []byte, path, rawQuery, method string, stream bool) (*http.Request, error) {
	incoming := *r.URL
	incoming.Path = path
	incoming.RawQuery = rawQuery
	target, err := buildUpstreamURL(provider.BaseURL, &incoming)
	if err != nil {
		return nil, err
	}
	if method == "" {
		method = r.Method
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyUpstreamHeaders(req, r, provider, body, stream)
	req.ContentLength = int64(len(body))
	return req, nil
}

// applyUpstreamHeaders 按 upstream protocol 重建认证与版本头,仅透传白名单语义 header。
// 允许透传: Content-Type、Accept、X-Request-ID(已校验)。其它 header 不从客户端复制。
func applyUpstreamHeaders(req *http.Request, client *http.Request, provider config.Provider, body []byte, stream bool) {
	if req == nil {
		return
	}
	// 从干净 header 开始。
	req.Header = make(http.Header)

	contentType := ""
	accept := ""
	requestID := ""
	if client != nil {
		contentType = strings.TrimSpace(client.Header.Get("Content-Type"))
		accept = strings.TrimSpace(client.Header.Get("Accept"))
		requestID = strings.TrimSpace(client.Header.Get("X-Request-ID"))
	}
	if contentType == "" && len(body) > 0 {
		contentType = "application/json"
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if requestID != "" && isSafeRequestID(requestID) {
		req.Header.Set("X-Request-ID", requestID)
	}

	switch strings.ToLower(strings.TrimSpace(provider.Protocol)) {
	case "anthropic":
		// Anthropic-Version 由 AetherRelay 固定生成,不信任客户端。
		req.Header.Set("Anthropic-Version", "2023-06-01")
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		} else if accept != "" && isSafeAccept(accept) {
			req.Header.Set("Accept", accept)
		} else {
			req.Header.Set("Accept", "application/json")
		}
		if strings.TrimSpace(provider.APIKey) != "" {
			req.Header.Set("X-API-Key", provider.APIKey)
		}
	default: // openai
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		} else if accept != "" && isSafeAccept(accept) {
			req.Header.Set("Accept", accept)
		} else {
			req.Header.Set("Accept", "application/json")
		}
		if strings.TrimSpace(provider.APIKey) != "" {
			req.Header.Set("Authorization", "Bearer "+provider.APIKey)
		}
	}
}

func isSafeRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

func isSafeAccept(accept string) bool {
	// 非流式请求只允许常见 JSON accept，避免 Accept:text/event-stream
	// 绕过请求体 stream=true 的统一 SSE 开关。流式请求由调用方直接设置 SSE。
	lower := strings.ToLower(accept)
	return strings.Contains(lower, "application/json") ||
		strings.Contains(lower, "*/*")
}

func buildUpstreamURL(base string, incoming *url.URL) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	parsed.Path = joinUpstreamPath(parsed.Path, incoming.Path)
	query := incoming.Query()
	query.Del("provider")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func joinUpstreamPath(basePath, incomingPath string) string {
	basePath = strings.TrimRight(basePath, "/")
	if basePath == "" {
		if incomingPath == "" {
			return "/"
		}
		return incomingPath
	}
	if incomingPath == "" || incomingPath == "/" {
		return basePath
	}
	if strings.HasSuffix(basePath, "/v1") && (incomingPath == "/v1" || strings.HasPrefix(incomingPath, "/v1/")) {
		return basePath + strings.TrimPrefix(incomingPath, "/v1")
	}
	return basePath + incomingPath
}

// resolveTransportPlan 是执行端点的唯一路由入口：从 EffectiveCatalog 解析 TransportPlan。
// 已禁用 X-AI-Provider / ?provider= / provider/model 显式选择;不允许修改 RouteOwner。
func (h *Handler) resolveTransportPlan(r *http.Request, model string) (TransportPlan, *APIError) {
	plans, apiErr := h.resolveTransportPlans(r, model)
	if apiErr != nil {
		return TransportPlan{}, apiErr
	}
	return plans[0], nil
}

func (h *Handler) resolveTransportPlans(r *http.Request, model string) ([]TransportPlan, *APIError) {
	method := ""
	path := ""
	if r != nil {
		method = r.Method
		if r.URL != nil {
			path = r.URL.Path
		}
	}
	identity, internal := internalFeatureIdentity(r.Context())
	if !internal {
		identity = clientauth.ClientIdentityFromContext(r.Context())
	}
	plans, apiErr := ResolveTransportPlansForAccess(h.currentConfig(), h.EffectiveCatalog(), identity.ProviderAccess, method, path, model)
	if apiErr != nil {
		return nil, apiErr
	}
	if r != nil {
		if requireFiles, _ := r.Context().Value(featureFileAttachmentsKey{}).(bool); requireFiles {
			filtered := plans[:0]
			for _, plan := range plans {
				if transportSupportsFileAttachments(plan) {
					filtered = append(filtered, plan)
				}
			}
			plans = filtered
			if len(plans) == 0 {
				return nil, &APIError{Code: ErrorCodeEndpointUnsupported, Message: fmt.Sprintf("model %q has no provider supporting file attachments", model), Model: model, ClientEndpoint: path}
			}
		}
	}
	if h.metricsRegistry == nil {
		return plans, nil
	}
	health := h.metricsRegistry.ProviderHealthSnapshot()
	eligible := make([]TransportPlan, 0, len(plans))
	modelHealth := make(map[string]metrics.StatsProviderHealth, len(plans))
	for _, plan := range plans {
		providerValue, providerOK := health[plan.RouteOwner]
		specific, specificOK := h.metricsRegistry.ProviderModelHealth(plan.RouteOwner, plan.ModelID)
		// Health status is a rolling quality score, not a routing quarantine. An
		// unhealthy score (for example, one truncated stream in a small window)
		// must remain routable so the provider can receive a recovery probe. Only
		// an active circuit or an explicit model-scoped credential failure may
		// block this exact model.
		if specificOK && (specific.Status == "credential_error" || specific.CircuitState == "open") {
			continue
		}
		// Credential failures are model-scoped: one Provider key may be
		// authorized for some exact models but not others. Transport failures
		// and open provider circuits remain shared across all models.
		// Provider-level rolling scores are used for ordering/observability. Do
		// not turn them into a permanent fail-fast gate; an active circuit is the
		// authoritative availability decision.
		if providerOK && providerValue.CircuitState == "open" {
			continue
		}
		eligible = append(eligible, plan)
		key := plan.RouteOwner + "\x00" + plan.ModelID
		if specificOK {
			modelHealth[key] = specific
		} else if providerOK {
			modelHealth[key] = providerValue
		}
	}
	if len(eligible) == 0 {
		first := plans[0]
		return nil, &APIError{Code: ErrorCodeProviderUnavailable, Message: fmt.Sprintf("all providers for model %q are unhealthy", model), Model: model, ClientEndpoint: first.ClientEndpoint, ClientProtocol: first.ClientProtocol}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if left, right := transportPlanSemanticRank(eligible[i]), transportPlanSemanticRank(eligible[j]); left != right {
			return left < right
		}
		if eligible[i].Priority != eligible[j].Priority {
			return eligible[i].Priority > eligible[j].Priority
		}
		leftKey := eligible[i].RouteOwner + "\x00" + eligible[i].ModelID
		rightKey := eligible[j].RouteOwner + "\x00" + eligible[j].ModelID
		left, right := modelHealth[leftKey], modelHealth[rightKey]
		if left.Score != right.Score {
			return left.Score > right.Score
		}
		return eligible[i].RouteOwner < eligible[j].RouteOwner
	})
	return eligible, nil
}

func providerMatchesModel(name string, provider config.Provider, model string) bool {
	return config.ProviderMatchesModel(name, provider, model)
}

func matchModelPattern(model, pattern string) bool {
	return config.MatchModelPattern(model, pattern)
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// decodedResponseBodyAndHeader 在需要时解压 gzip,并使用配置的上游响应上限。
// 解压失败或超限返回 error,调用方应在写响应头前处理。
func (h *Handler) decodedResponseBodyAndHeader(body []byte, header http.Header) ([]byte, http.Header, error) {
	decodedHeader := header.Clone()
	if !strings.EqualFold(strings.TrimSpace(decodedHeader.Get("Content-Encoding")), "gzip") {
		return body, decodedHeader, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, decodedHeader, fmt.Errorf("gzip decode failed: %w", err)
	}
	defer reader.Close()
	limit := h.currentConfig().MaxUpstreamResponseBytes
	if limit <= 0 {
		limit = config.DefaultMaxUpstreamResponseBytes
	}
	limited := io.LimitReader(reader, limit+1)
	decodedBody, err := io.ReadAll(limited)
	if err != nil {
		return nil, decodedHeader, fmt.Errorf("gzip decode read failed: %w", err)
	}
	if int64(len(decodedBody)) > limit {
		return nil, decodedHeader, fmt.Errorf("decompressed upstream response exceeds limit of %d bytes", limit)
	}
	decodedHeader.Del("Content-Encoding")
	decodedHeader.Del("Content-Length")
	return decodedBody, decodedHeader, nil
}

// hopByHopHeaders 是 RFC 9110 定义的标准 hop-by-hop 头。
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection", // 非标准但旧式代理常见
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// removeHopByHop 删除标准 hop-by-hop 头,以及 Connection 中动态列出的扩展头。
// 请求与响应方向均应调用,避免双重编码/错误连接复用等问题。
func removeHopByHop(header http.Header) {
	if header == nil {
		return
	}
	// 先解析 Connection 中列出的动态 header 名并删除。
	for _, connVal := range header.Values("Connection") {
		for _, token := range strings.Split(connVal, ",") {
			name := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(token))
			if name == "" || strings.EqualFold(name, "close") || strings.EqualFold(name, "keep-alive") {
				// close/keep-alive 是连接指令,不是要额外删除的扩展头名(标准列表已含 Keep-Alive)。
				continue
			}
			header.Del(name)
		}
	}
	for _, key := range hopByHopHeaders {
		header.Del(key)
	}
}

// copyResponseHeader 复制上游响应头并剥离 hop-by-hop,供回写客户端。
func copyResponseHeader(dst, src http.Header) {
	copyHeader(dst, src)
	removeHopByHop(dst)
}

// prepareSSEHeaders 统一所有模型流式响应的下游 HTTP 合同。
// SSE 由 HTTP server 自行选择传输编码，不能继承上游 Content-Length 或连接级头；
// no-transform 与 X-Accel-Buffering 避免常见反向代理缓存、压缩或聚合事件。
func prepareSSEHeaders(header http.Header) {
	if header == nil {
		return
	}
	removeHopByHop(header)
	header.Del("Content-Length")
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("X-Accel-Buffering", "no")
}

func (h *Handler) recordAndPrint(round *archive.Round, r *http.Request, provider, model string, stream bool, status int, duration time.Duration, tok tokenUsage, errMessage string) {
	h.recordAndPrintFail(round, r, provider, model, stream, status, duration, tok, streamFailFromMessage(errMessage))
}

func (h *Handler) recordAndPrintFail(round *archive.Round, r *http.Request, provider, model string, stream bool, status int, duration time.Duration, tok tokenUsage, fail *streamFail) {
	outcome := outcomeFromStreamFail(fail, status)
	if status == 0 && outcome == string(streamKindClientCanceled) {
		status = 499
	}
	errMessage := ""
	errorCode := ""
	if fail != nil {
		errMessage = fail.Error()
		if fail.ErrorCode != "" {
			errorCode = fail.ErrorCode
		} else if fail.Kind != "" {
			errorCode = string(fail.Kind)
		}
	}
	clientEndpoint, upstreamProtocol, upstreamEndpoint, conversionMode := "", "", "", ""
	conversionLevel := 0
	if round != nil {
		clientEndpoint = round.ClientEndpoint
		upstreamProtocol = round.UpstreamProtocol
		upstreamEndpoint = round.UpstreamEndpoint
		conversionMode = round.ConversionMode
		conversionLevel = round.ConversionLevel
	}
	// 结算 DuckDB usage event(ServeHTTP 已 Start)。
	eventID := ""
	if r != nil {
		eventID = usageEventIDFromContext(r.Context())
	}
	usageCompleted := h.completeUsage(r, eventID, provider, model, stream, status, duration, tok, outcome, errorCode, round)
	if usageCompleted && h.metricsRegistry != nil && r != nil {
		h.metricsRegistry.RecordClientUsage(clientauth.ClientIdentityFromContext(r.Context()).KeyID, tok.PromptTokens, tok.CompletionTokens)
	}

	if h.metricsRegistry != nil {
		route := RouteLabel(r)
		if levelReporter, ok := h.metricsRegistry.(metricsport.PlanLevelReporter); ok {
			levelReporter.RecordRequestPlanWithLevel(provider, model, route, status, duration, outcome,
				clientEndpoint, upstreamProtocol, upstreamEndpoint, conversionMode, conversionLevel)
		} else {
			h.metricsRegistry.RecordRequestPlan(provider, model, route, status, duration, outcome,
				clientEndpoint, upstreamProtocol, upstreamEndpoint, conversionMode)
		}
		// 普通 HTTP upstream 已在转发器中计数；ChatGPT Web 的 executor
		// 路径没有该入口，因此也要记录其尚未提交响应的上游失败。
		if shouldCountUpstreamError(fail, stream) {
			h.metricsRegistry.RecordUpstreamError(provider, -2) // -2 = stream_midflight
		} else if provider == effectivecatalog.BuiltinProviderID && fail != nil && fail.CountUpstream {
			h.metricsRegistry.RecordUpstreamError(provider, -1) // -1 = ChatGPT Web upstream failure
		}
		h.metricsRegistry.RecordTokens(provider, model, tok.PromptTokens, tok.CompletionTokens, tok.CachedInputTokens, tok.CacheCreationInputTokens)
		if usageCompleted && round != nil && round.ConversionMode != "" && round.ConversionMode != TransportModeNative {
			if reporter, ok := h.metricsRegistry.(metricsport.ConversionReporter); ok {
				reporter.RecordConversion(provider, model, round.ClientProtocol, round.UpstreamProtocol, round.ConversionMode, round.ConversionLevel, round.UpstreamStatus, round.ConversionDuration, round.ConversionDegraded, tok.Estimated, round.IgnoredFeatures, round.UnsupportedFeatures)
			}
		}
	}
	h.printSummary(round, provider, model, stream, status, duration, tok, errMessage)
}

func (h *Handler) settleConversionClientCanceled(round *archive.Round, r *http.Request, provider, model string, stream bool, start time.Time) {
	duration := time.Since(start)
	fail := newStreamFail(streamKindClientCanceled, "client canceled", context.Canceled, false)
	h.recordAndPrintFail(round, r, provider, model, stream, 499, duration, tokenUsage{}, fail)
	h.writeArchiveMetadata(round, provider, model, stream, 499, duration, tokenUsage{}, "", fail.Error(), "", string(streamKindClientCanceled))
}

func (h *Handler) writeArchiveMetadata(round *archive.Round, provider, model string, stream bool, status int, duration time.Duration, usage tokenUsage, responsePath, message, fullResponsePath, outcome string) {
	stableHash, fingerprint, drift, driftCount := h.driftInfo(round)
	fullContent := round == nil || round.FullContent()
	if outcome == "" {
		outcome = outcomeFromStreamFail(streamFailFromMessage(message), status)
	}
	// 转换路径中途失败时 outcome 必须显式为 conversion,避免伪造成功终止。
	if outcome == "" || outcome == "error" {
		if round != nil && round.ConversionMode != "" && round.ConversionMode != TransportModeNative && message != "" {
			if strings.Contains(message, "conversion") || strings.Contains(message, ErrorCodeConversionUnsupported) {
				outcome = "conversion"
			}
		}
	}
	meta := archive.Metadata{
		FinishedAt:               time.Now(),
		Provider:                 provider,
		Model:                    model,
		StablePrefixHash:         stableHash,
		RequestFingerprint:       fingerprint,
		StablePrefixDrift:        drift,
		StablePrefixDriftCount:   driftCount,
		Stream:                   stream,
		HTTPStatus:               status,
		Outcome:                  outcome,
		DurationMS:               duration.Milliseconds(),
		InputTokens:              usage.PromptTokens,
		OutputTokens:             usage.CompletionTokens,
		TotalTokens:              usage.PromptTokens + usage.CompletionTokens,
		CachedInputTokens:        usage.CachedInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheHitRate:             usage.CacheHitRate(),
		Estimated:                usage.Estimated,
		FullContentEnabled:       fullContent,
		Error:                    message,
	}
	if round != nil {
		meta.RequestID = round.RequestID
		meta.EventID = round.RequestID
		meta.APIKeyID = round.APIKeyID
		meta.Operation = round.Operation
		meta.ClientEndpoint = round.ClientEndpoint
		meta.ClientProtocol = round.ClientProtocol
		meta.UpstreamProtocol = round.UpstreamProtocol
		meta.UpstreamEndpoint = round.UpstreamEndpoint
		meta.ConversionMode = round.ConversionMode
		meta.ConversionLevel = round.ConversionLevel
		meta.IgnoredFeatures = append([]string(nil), round.IgnoredFeatures...)
		meta.UnsupportedFeatures = append([]string(nil), round.UnsupportedFeatures...)
		meta.ConversionDurationMS = round.ConversionDuration.Milliseconds()
		meta.ConversionDegraded = round.ConversionDegraded
	}
	if round != nil {
		if round.HasFile("request.meta.json") {
			meta.RequestMetaPath = "request.meta.json"
		}
		if round.HasFile("request.json") {
			meta.RequestPath = "request.json"
		}
		if round.HasFile("upstream_request.json") {
			meta.UpstreamRequestPath = "upstream_request.json"
		}
		if round.HasFile("upstream_response.json") {
			meta.UpstreamResponsePath = "upstream_response.json"
		}
		if responsePath != "" && round.HasFile(responsePath) {
			meta.ResponsePath = responsePath
		}
		if fullResponsePath != "" && round.HasFile(fullResponsePath) {
			meta.FullResponsePath = fullResponsePath
		}
		if err := round.WriteMetadata(meta); err != nil {
			log.Printf("archive metadata: %v", err)
		}
	}
}

func (h *Handler) driftInfo(round *archive.Round) (stableHash, fingerprint string, drift bool, driftCount int) {
	if round == nil || h.driftTracker == nil {
		return "", "", false, 0
	}
	stableHash = round.StablePrefixHash
	fingerprint = round.RequestFingerprint
	if stableHash == "" {
		return
	}
	drift, driftCount = h.driftTracker.Observe(stableHash)
	return
}

func responseFileName(contentType string, stream bool) string {
	if stream {
		return "response.sse"
	}
	contentType = strings.ToLower(contentType)
	switch {
	case strings.Contains(contentType, "json"):
		return "response.json"
	case strings.Contains(contentType, "text"):
		return "response.txt"
	default:
		return "response.bin"
	}
}

func (h *Handler) printSummary(round *archive.Round, provider, model string, stream bool, status int, duration time.Duration, usage tokenUsage, errMessage string) {
	level := slog.LevelInfo
	label := "ok"
	clientCanceled := isClientCanceledStreamIssue(errMessage)
	if status >= 500 {
		level = slog.LevelError
		label = "error"
	} else if status >= 400 || (errMessage != "" && !clientCanceled) || usage.Estimated {
		level = slog.LevelWarn
		if status >= 400 || (errMessage != "" && !clientCanceled) {
			label = "warn"
		} else {
			label = "estimated"
		}
	} else if clientCanceled {
		label = "canceled"
	}
	roundID := roundIDValue(round)
	attrs := []any{
		slog.String("label", label),
		slog.String("provider", provider),
		slog.String("model", model),
		slog.Int("round", roundID),
		slog.Int("status", status),
		slog.Bool("stream", stream),
		slog.Duration("duration", duration.Truncate(time.Millisecond)),
		slog.Int("input_tokens", usage.PromptTokens),
		slog.Int("output_tokens", usage.CompletionTokens),
		slog.Int("total_tokens", usage.PromptTokens+usage.CompletionTokens),
		slog.Int("cached_input_tokens", usage.CachedInputTokens),
		slog.Int("cache_creation_input_tokens", usage.CacheCreationInputTokens),
		slog.Float64("cache_hit_rate", usage.CacheHitRate()),
		slog.Bool("estimated", usage.Estimated),
	}
	if errMessage != "" {
		key := "error"
		if clientCanceled {
			key = "reason"
		}
		attrs = append(attrs, slog.String(key, errMessage))
	}
	slog.LogAttrs(context.Background(), level, "AetherRelay", toAttrs(attrs)...)
}

func roundIDValue(round *archive.Round) int {
	if round == nil {
		return 0
	}
	return round.ID
}

type streamIdleTimer struct {
	timer   *time.Timer
	timeout time.Duration
	expired atomic.Bool
}

func (h *Handler) startStreamIdleTimer(cancel context.CancelFunc) (*streamIdleTimer, func()) {
	streamIdleTimeout := h.currentConfig().StreamIdleTimeout
	if cancel == nil || streamIdleTimeout <= 0 {
		return nil, func() {}
	}
	idle := &streamIdleTimer{timeout: streamIdleTimeout}
	idle.timer = time.AfterFunc(streamIdleTimeout, func() {
		idle.expired.Store(true)
		cancel()
	})
	return idle, func() {
		idle.timer.Stop()
	}
}

func resetStreamIdleTimer(idle *streamIdleTimer, timeout time.Duration) {
	if idle == nil || idle.timer == nil || timeout <= 0 {
		return
	}
	idle.expired.Store(false)
	idle.timer.Reset(idle.timeout)
}

// logStreamFail 直接记录已构造的 streamFail,不再二次推断 kind。
// 用于在错误产生点已明确 protocol/conversion 等类型的场景。
func (h *Handler) logStreamFail(round *archive.Round, provider, model string, fail *streamFail) {
	if fail == nil {
		return
	}
	level := slog.LevelWarn
	if fail.Kind == streamKindClientCanceled {
		level = slog.LevelInfo
	}
	slog.LogAttrs(context.Background(), level, "stream issue",
		slog.String("event", "STREAM"),
		slog.Int("round", roundID(round)),
		slog.String("provider", provider),
		slog.String("model", model),
		slog.String("outcome", string(fail.Kind)),
		slog.String("message", fail.Error()),
	)
}

// logStreamIssue 记录流式问题并返回 typed streamFail。
// kind 由 operation + 错误上下文决定,不再依赖最终字符串匹配。
func (h *Handler) logStreamIssue(round *archive.Round, provider, model, operation string, err error, requestContext context.Context, idleTimer *streamIdleTimer) *streamFail {
	if err == nil {
		return nil
	}
	kind := streamKindError
	countUpstream := false
	level := slog.LevelWarn
	message := fmt.Sprintf("%s: %v", operation, err)
	errText := strings.ToLower(err.Error())
	op := strings.ToLower(operation)

	clientCanceled := errors.Is(err, context.Canceled) ||
		(requestContext != nil && errors.Is(requestContext.Err(), context.Canceled))
	deadlineExceeded := errors.Is(err, context.DeadlineExceeded) ||
		(requestContext != nil && errors.Is(requestContext.Err(), context.DeadlineExceeded))

	switch {
	case idleTimer != nil && idleTimer.expired.Load():
		kind = streamKindIdleTimeout
		countUpstream = true
		message = fmt.Sprintf("%s: stream idle timeout exceeded after %s", operation, idleTimer.timeout.Truncate(time.Millisecond))
	case clientCanceled:
		kind = streamKindClientCanceled
		level = slog.LevelInfo
		// 保持与历史测试/日志一致的消息格式。
		message = fmt.Sprintf("%s: client canceled downstream request", operation)
	case deadlineExceeded:
		kind = streamKindError
		message = fmt.Sprintf("%s: downstream request deadline exceeded", operation)
	case strings.Contains(op, "write") && (strings.Contains(op, "client") || strings.Contains(op, "downstream")):
		kind = streamKindClientWrite
	case strings.Contains(op, "convert") || strings.Contains(op, "conversion"):
		kind = streamKindConversion
	case strings.Contains(op, "protocol") || strings.Contains(errText, "invalid json") || strings.Contains(errText, "invalid sse") || strings.Contains(errText, "unmarshal"):
		kind = streamKindProtocol
		countUpstream = true
	case strings.Contains(op, "limit") || strings.Contains(errText, "exceeds limit") || strings.Contains(errText, "truncated"):
		kind = streamKindLimitExceeded
		countUpstream = false
	case strings.Contains(op, "read") && (strings.Contains(op, "upstream") || strings.Contains(op, "stream") || strings.Contains(op, "raw")):
		kind = streamKindUpstreamTrunc
		countUpstream = true
	default:
		if strings.Contains(op, "upstream") || strings.Contains(op, "read") {
			kind = streamKindUpstreamTrunc
			countUpstream = true
		}
	}

	slog.LogAttrs(context.Background(), level, "stream issue",
		slog.String("event", "STREAM"),
		slog.Int("round", roundID(round)),
		slog.String("provider", provider),
		slog.String("model", model),
		slog.String("outcome", string(kind)),
		slog.String("message", message),
	)
	return newStreamFail(kind, message, err, countUpstream)
}

func isClientCanceledStreamIssue(message string) bool {
	return strings.Contains(message, "client canceled downstream request")
}

func roundID(round *archive.Round) int {
	if round == nil {
		return 0
	}
	return round.ID
}
