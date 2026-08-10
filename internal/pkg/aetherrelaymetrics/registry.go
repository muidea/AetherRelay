// Package metrics 提供 AetherRelay 进程内的轻量级指标聚合。
// 不引入 prometheus client_golang,直接手写 minimal exposition format 与
// /stats JSON 序列化。所有方法并发安全(单 mutex 保护 map)。
package metrics

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 延迟样本每个 (provider, model) 组合的容量上限;超过会触发降采样。
const latencySamplesCap = 2048

// providerHealthSamplesCap bounds the in-memory rolling health window per
// provider. Health is a process-local live signal and is intentionally not
// persisted across restart.
const providerHealthSamplesCap = 512

// maxModelsPerProvider 限制每个 provider 下独立 model label 数量。
// 超出后新 model 归一为 otherModelLabel,防止客户端通过通配路由刷爆 map。
const maxModelsPerProvider = 64

// otherModelLabel 是超出基数上限后的聚合标签。
const otherModelLabel = "_other"

// requestKey 是请求计数/直方图的复合 label。
// Outcome 描述业务结果(完整枚举):
//
//	success | client_canceled | idle_timeout | limit_exceeded |
//	upstream_truncated | upstream_failed | incomplete | endpoint_drift |
//	client_write | conversion | protocol | error
//
// 流式首包 200 后中途失败时 Status 仍可能是 2xx,Outcome 用于区分真实成败。
// 计入 upstream error rate 的: upstream_truncated, upstream_failed, idle_timeout, protocol(上游损坏)。
type requestKey struct {
	Provider, Model, Route, Status, Outcome string
	// TransportPlan 有界枚举 label(空值归一为 unknown,避免基数爆炸)。
	ClientEndpoint, UpstreamProtocol, UpstreamEndpoint, ConversionMode string
	// ConversionLevel is the validated model/direction capability level. Keep
	// it bounded to 0..3 so it cannot create unbounded Prometheus cardinality.
	ConversionLevel int
}

// tokenKey 是 token 计数器的复合 label。
type tokenKey struct {
	Provider, Model string
}

// errorKey 是 upstream 错误计数的复合 label。
type errorKey struct {
	Provider, StatusCode string
}

type conversionKey struct {
	Provider, Model, ClientProtocol, UpstreamProtocol, Mode string
	Level, UpstreamStatus                                   int
	Degraded, Estimated                                     bool
}

type conversionFeatureKey struct {
	Provider, Model, Mode, Kind, Feature string
}

// clientUsageKey 只有配置内受控的 api_key_id 与 default，label 基数有界。
type clientUsageKey string

// latencyKey 是延迟样本的复合 label。
type latencyKey struct {
	Provider, Model string
}

// Registry 是 metrics 聚合中心。所有记录方法都接收 nil-safe,r == nil 时静默返回。
type Registry struct {
	mu        sync.Mutex
	startedAt time.Time

	requestCount         map[requestKey]uint64
	requestDurationSum   map[requestKey]float64
	requestDurationCount map[requestKey]uint64
	requestDurationMinMS map[requestKey]float64
	requestDurationMaxMS map[requestKey]float64

	latencySamples map[latencyKey][]float64

	// upstreamHeaderLatency 非流式/仅响应头时间的 attempt 延迟。
	upstreamHeaderLatency map[string][]float64
	// upstreamFirstEventLatency 流式含首行探测的 attempt 延迟;SLO p99 优先使用。
	upstreamFirstEventLatency map[string][]float64

	inputTokens         map[tokenKey]uint64
	outputTokens        map[tokenKey]uint64
	cachedInputTokens   map[tokenKey]uint64
	cacheCreationTokens map[tokenKey]uint64
	cacheHits           map[tokenKey]uint64
	cacheMisses         map[tokenKey]uint64
	cachedTokenSumHits  map[tokenKey]uint64

	upstreamErrors   map[errorKey]uint64
	upstreamAttempts map[string]uint64 // provider -> total attempts
	providerHealth   map[string]providerHealth
	healthSamples    map[string][]healthSample

	conversionCount         map[conversionKey]uint64
	conversionDurationSum   map[conversionKey]float64
	conversionDurationCount map[conversionKey]uint64
	conversionFeatures      map[conversionFeatureKey]uint64

	clientRequests          map[clientUsageKey]uint64
	clientInput             map[clientUsageKey]uint64
	clientOutput            map[clientUsageKey]uint64
	clientTokens            map[clientUsageKey]uint64
	usageWriteErr           map[string]uint64
	usageQueryErr           uint64
	usageQueryDurationSum   float64
	usageQueryDurationCount uint64
	usageRecovered          uint64
	usageCheckpointErr      uint64
	usageHealthy            bool

	// knownModels 记录每个 provider 已见过的 model label(不含 _other),用于基数限制。
	knownModels map[string]map[string]struct{} // provider -> set(model)

	// slo 可选挂接:用于把 webhook 队列/投递指标暴露到 /metrics。
	// 用 atomic.Pointer 避免与 metrics 记录路径争用同一把锁。
	slo atomic.Pointer[SLOEvaluator]
}

// NewRegistry 构造初始化的 Registry,启动时间设为当前时刻。
func NewRegistry() *Registry {
	return &Registry{
		startedAt:                 time.Now(),
		requestCount:              map[requestKey]uint64{},
		requestDurationSum:        map[requestKey]float64{},
		requestDurationCount:      map[requestKey]uint64{},
		requestDurationMinMS:      map[requestKey]float64{},
		requestDurationMaxMS:      map[requestKey]float64{},
		latencySamples:            map[latencyKey][]float64{},
		upstreamHeaderLatency:     map[string][]float64{},
		upstreamFirstEventLatency: map[string][]float64{},
		inputTokens:               map[tokenKey]uint64{},
		outputTokens:              map[tokenKey]uint64{},
		cachedInputTokens:         map[tokenKey]uint64{},
		cacheCreationTokens:       map[tokenKey]uint64{},
		cacheHits:                 map[tokenKey]uint64{},
		cacheMisses:               map[tokenKey]uint64{},
		cachedTokenSumHits:        map[tokenKey]uint64{},
		upstreamErrors:            map[errorKey]uint64{},
		upstreamAttempts:          map[string]uint64{},
		providerHealth:            map[string]providerHealth{},
		healthSamples:             map[string][]healthSample{},
		conversionCount:           map[conversionKey]uint64{},
		conversionDurationSum:     map[conversionKey]float64{},
		conversionDurationCount:   map[conversionKey]uint64{},
		conversionFeatures:        map[conversionFeatureKey]uint64{},
		clientRequests:            map[clientUsageKey]uint64{},
		clientInput:               map[clientUsageKey]uint64{},
		clientOutput:              map[clientUsageKey]uint64{},
		clientTokens:              map[clientUsageKey]uint64{},
		usageWriteErr:             map[string]uint64{},
		usageHealthy:              true,
		knownModels:               map[string]map[string]struct{}{},
	}
}

func (r *Registry) RecordConversion(provider, model, clientProtocol, upstreamProtocol, conversionMode string, conversionLevel, upstreamStatus int, duration time.Duration, degraded, estimated bool, ignoredFeatures, unsupportedFeatures []string) {
	if r == nil || conversionMode == "" || conversionMode == "native" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	model = r.normalizeModelLabel(provider, model)
	key := conversionKey{
		Provider: provider, Model: model,
		ClientProtocol: boundProtocolLabel(clientProtocol), UpstreamProtocol: boundProtocolLabel(upstreamProtocol),
		Mode: boundModeLabel(conversionMode), Level: boundConversionLevel(conversionLevel),
		UpstreamStatus: boundConversionStatus(upstreamStatus), Degraded: degraded, Estimated: estimated,
	}
	r.conversionCount[key]++
	r.conversionDurationSum[key] += duration.Seconds()
	r.conversionDurationCount[key]++
	r.recordConversionFeaturesLocked(provider, model, key.Mode, "ignored", ignoredFeatures)
	r.recordConversionFeaturesLocked(provider, model, key.Mode, "unsupported", unsupportedFeatures)
}

func (r *Registry) recordConversionFeaturesLocked(provider, model, mode, kind string, features []string) {
	seen := map[string]struct{}{}
	for _, feature := range features {
		feature = boundConversionFeature(feature)
		if _, exists := seen[feature]; exists {
			continue
		}
		seen[feature] = struct{}{}
		r.conversionFeatures[conversionFeatureKey{Provider: provider, Model: model, Mode: mode, Kind: kind, Feature: feature}]++
	}
}

func boundConversionStatus(status int) int {
	if status < 100 || status > 599 {
		return 0
	}
	return status
}

func boundConversionFeature(feature string) string {
	switch strings.TrimSpace(feature) {
	case "reasoning", "thinking", "reasoning_output", "thinking_output", "internal_chat_message_metadata_passthrough", "output_metadata", "tools", "tool_choice", "function_call", "function_call_output", "tool_use", "tool_result", "stream", "max_output_tokens", "max_tokens", "text.format", "previous_response_id", "image", "document", "structured_output", "continuation", "unsupported_feature", "unsupported_content", "unsupported_role":
		return strings.TrimSpace(feature)
	default:
		return "_other"
	}
}

// InitializeClientUsage 使用 DuckDB all-time 聚合初始化进程内 Prometheus 镜像。
// Store 仍是唯一 authority；该镜像只服务低开销 exposition。
func (r *Registry) InitializeClientUsage(values map[string]ClientUsage) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clientRequests = map[clientUsageKey]uint64{}
	r.clientInput = map[clientUsageKey]uint64{}
	r.clientOutput = map[clientUsageKey]uint64{}
	r.clientTokens = map[clientUsageKey]uint64{}
	for id, value := range values {
		key := clientUsageKey(id)
		if value.Requests > 0 {
			r.clientRequests[key] = uint64(value.Requests)
		}
		if value.InputTokens > 0 {
			r.clientInput[key] = uint64(value.InputTokens)
		}
		if value.OutputTokens > 0 {
			r.clientOutput[key] = uint64(value.OutputTokens)
		}
		if value.TotalTokens > 0 {
			r.clientTokens[key] = uint64(value.TotalTokens)
		}
	}
}

// ClientUsage 是 DuckDB all-time key 汇总投影，避免 metrics 包依赖 usage 包。
type ClientUsage struct{ Requests, InputTokens, OutputTokens, TotalTokens int64 }

func (r *Registry) RecordClientUsage(apiKeyID string, input, output int) {
	if r == nil || apiKeyID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := clientUsageKey(apiKeyID)
	r.clientRequests[key]++
	if input > 0 {
		r.clientInput[key] += uint64(input)
	}
	if output > 0 {
		r.clientOutput[key] += uint64(output)
	}
	if input+output > 0 {
		r.clientTokens[key] += uint64(input + output)
	}
}

func (r *Registry) RecordUsageStoreWriteError(phase string) {
	if r == nil {
		return
	}
	if phase != "start" && phase != "complete" {
		phase = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usageWriteErr[phase]++
	r.usageHealthy = false
}

func (r *Registry) RecordUsageStoreQuery(duration time.Duration, err error, healthy bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usageQueryDurationSum += duration.Seconds()
	r.usageQueryDurationCount++
	if err != nil {
		r.usageQueryErr++
	}
	r.usageHealthy = healthy
}

func (r *Registry) RecordUsageStoreRecovered(count int64) {
	if r == nil || count <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usageRecovered += uint64(count)
}

func (r *Registry) RecordUsageStoreCheckpointError() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usageCheckpointErr++
	r.usageHealthy = false
}

func (r *Registry) SetUsageStoreHealthy(healthy bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usageHealthy = healthy
}

// normalizeModelLabel 在已持锁前提下限制 model label 基数。
// 空 model 保持为空;已登记 model 原样返回;新 model 在未超限时登记,否则归为 _other。
func (r *Registry) normalizeModelLabel(provider, model string) string {
	if model == "" || model == otherModelLabel {
		return model
	}
	set := r.knownModels[provider]
	if set == nil {
		set = map[string]struct{}{}
		r.knownModels[provider] = set
	}
	if _, ok := set[model]; ok {
		return model
	}
	if len(set) >= maxModelsPerProvider {
		return otherModelLabel
	}
	set[model] = struct{}{}
	return model
}

// ReserveModels 预登记应优先占用 model label 槽位的模型(catalog / 精确 models)。
// 在接受动态通配流量前调用,避免随机 model 先占满 64 槽。
func (r *Registry) ReserveModels(provider string, models []string) {
	if r == nil || provider == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.knownModels[provider]
	if set == nil {
		set = map[string]struct{}{}
		r.knownModels[provider] = set
	}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || model == otherModelLabel {
			continue
		}
		// 通配模式不预占槽位。
		if strings.Contains(model, "*") {
			continue
		}
		if _, ok := set[model]; ok {
			continue
		}
		if len(set) >= maxModelsPerProvider {
			return
		}
		set[model] = struct{}{}
	}
}

// RecordRequest 记录一次完成的请求(包含 duration)。
// status 归一为 2xx/3xx/4xx/5xx;outcome 描述业务结果(空则 success)。
func (r *Registry) RecordRequest(provider, model, route string, status int, duration time.Duration, outcome string) {
	r.RecordRequestPlanWithLevel(provider, model, route, status, duration, outcome, "", "", "", "", 0)
}

// RecordRequestPlan 记录请求,并附带 TransportPlan 有界 label。
func (r *Registry) RecordRequestPlan(provider, model, route string, status int, duration time.Duration, outcome, clientEndpoint, upstreamProtocol, upstreamEndpoint, conversionMode string) {
	r.RecordRequestPlanWithLevel(provider, model, route, status, duration, outcome, clientEndpoint, upstreamProtocol, upstreamEndpoint, conversionMode, 0)
}

// RecordRequestPlanWithLevel records a request with the validated conversion
// capability level. The legacy RecordRequestPlan API remains available and
// records level 0 for callers that do not have a conversion declaration.
func (r *Registry) RecordRequestPlanWithLevel(provider, model, route string, status int, duration time.Duration, outcome, clientEndpoint, upstreamProtocol, upstreamEndpoint, conversionMode string, conversionLevel int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	model = r.normalizeModelLabel(provider, model)
	if outcome == "" {
		outcome = "success"
	}
	key := requestKey{
		Provider: provider, Model: model, Route: route, Status: statusBucket(status), Outcome: outcome,
		ClientEndpoint: boundEndpointLabel(clientEndpoint), UpstreamProtocol: boundProtocolLabel(upstreamProtocol),
		UpstreamEndpoint: boundEndpointLabel(upstreamEndpoint), ConversionMode: boundModeLabel(conversionMode),
		ConversionLevel: boundConversionLevel(conversionLevel),
	}
	r.requestCount[key]++
	if provider != "" && shouldTrackProviderHealth(status, outcome) {
		now := time.Now()
		sample := healthSample{At: now, Model: model, Operation: route, Status: status, Outcome: outcome, Duration: duration}
		r.providerHealth[provider] = applyProviderHealthSample(r.providerHealth[provider], sample)
		samples := r.healthSamples[provider]
		if len(samples) >= providerHealthSamplesCap {
			samples = samples[providerHealthSamplesCap/2:]
		}
		r.healthSamples[provider] = append(samples, sample)
	}
	seconds := duration.Seconds()
	r.requestDurationSum[key] += seconds
	r.requestDurationCount[key]++
	durMS := float64(duration.Milliseconds())
	if existing, ok := r.requestDurationMinMS[key]; !ok || durMS < existing {
		r.requestDurationMinMS[key] = durMS
	}
	if existing, ok := r.requestDurationMaxMS[key]; !ok || durMS > existing {
		r.requestDurationMaxMS[key] = durMS
	}

	// 完成请求延迟按 (provider, model) 记录,与 attempt 延迟分离。
	latKey := latencyKey{Provider: provider, Model: model}
	samples := r.latencySamples[latKey]
	if len(samples) >= latencySamplesCap {
		samples = samples[latencySamplesCap/2:]
	}
	r.latencySamples[latKey] = append(samples, seconds)
}

func applyProviderHealthSample(health providerHealth, sample healthSample) providerHealth {
	health.LastOutcome = sample.Outcome
	if sample.Status >= 200 && sample.Status < 400 && sample.Outcome == "success" {
		health.Successes++
		health.ConsecutiveFailures = 0
		health.LastSuccessAt = sample.At
		health.LastStatus = sample.Status
		health.CircuitOpenUntil = time.Time{}
	} else if sample.Outcome != "success" || sample.Status == 401 || sample.Status == 403 || sample.Status == 408 || sample.Status == 429 || sample.Status >= 500 {
		health.Failures++
		health.ConsecutiveFailures++
		health.LastFailureAt = sample.At
		health.LastStatus = sample.Status
		if health.ConsecutiveFailures >= 3 && retryableHealthFailure(sample.Status, sample.Outcome) {
			health.CircuitOpenUntil = sample.At.Add(30 * time.Second)
		}
	}
	return health
}

// shouldTrackProviderHealth keeps the provider-health window about upstream
// availability. Client validation, unsupported conversion features, and other
// local 4xx outcomes remain request metrics but must not make a healthy
// upstream look degraded.
func shouldTrackProviderHealth(status int, outcome string) bool {
	if status >= 200 && status < 400 && outcome == "success" {
		return true
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError {
		return true
	}
	switch outcome {
	case "upstream_failed", "upstream_truncated", "idle_timeout", "protocol", "endpoint_drift":
		return true
	default:
		return false
	}
}

func retryableHealthFailure(status int, outcome string) bool {
	if status == 408 || status == 429 || status >= 500 || status <= 0 {
		return true
	}
	switch outcome {
	case "upstream_failed", "upstream_truncated", "idle_timeout", "protocol":
		return true
	default:
		return false
	}
}

// ResetProviderHealth discards process-local health and circuit state after
// an operator changes a Provider definition. Historical request counters stay
// intact; only observations tied to the previous transport configuration are
// no longer eligible to gate routing.
func (r *Registry) ResetProviderHealth(provider string) {
	if r == nil || strings.TrimSpace(provider) == "" {
		return
	}
	r.mu.Lock()
	delete(r.providerHealth, provider)
	delete(r.healthSamples, provider)
	r.mu.Unlock()
}

// RecordTokens 累计 token 用量,并按 cached_input_tokens>0 判定 cache hit/miss。
// input <= 0 时不计入 hit/miss(避免零值干扰 hit rate)。
func (r *Registry) RecordTokens(provider, model string, input, output, cached, cacheCreation int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	model = r.normalizeModelLabel(provider, model)
	key := tokenKey{Provider: provider, Model: model}
	if input > 0 {
		r.inputTokens[key] += uint64(input)
	}
	if output > 0 {
		r.outputTokens[key] += uint64(output)
	}
	if cached > 0 {
		r.cachedInputTokens[key] += uint64(cached)
	}
	if cacheCreation > 0 {
		r.cacheCreationTokens[key] += uint64(cacheCreation)
	}
	if input > 0 {
		if cached > 0 {
			r.cacheHits[key]++
			r.cachedTokenSumHits[key] += uint64(cached)
		} else {
			r.cacheMisses[key]++
		}
	}
}

// AttemptLatencyKind 区分 attempt 延迟语义。
type AttemptLatencyKind string

const (
	// AttemptHeader 仅到响应头(非流式 body 下载前 / 流式未含首包)。
	AttemptHeader AttemptLatencyKind = "header"
	// AttemptFirstEvent 到首个 SSE 行(含探测),流式成功路径使用;SLO p99 优先此项。
	AttemptFirstEvent AttemptLatencyKind = "first_event"
)

// RecordUpstreamAttempt 累计一次上游尝试,并按 kind 写入对应延迟样本。
// SLO p99 优先 first_event,否则 header;不与完成请求 latency 混用。
func (r *Registry) RecordUpstreamAttempt(provider string, duration time.Duration, kind AttemptLatencyKind) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upstreamAttempts[provider]++
	var bucket map[string][]float64
	switch kind {
	case AttemptFirstEvent:
		bucket = r.upstreamFirstEventLatency
	default:
		bucket = r.upstreamHeaderLatency
	}
	samples := bucket[provider]
	if len(samples) >= latencySamplesCap {
		samples = samples[latencySamplesCap/2:]
	}
	bucket[provider] = append(samples, duration.Seconds())
}

// RecordUpstreamError 累计一次上游错误响应,status_code 保留原始值。
func (r *Registry) RecordUpstreamError(provider string, status int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upstreamErrors[errorKey{Provider: provider, StatusCode: strconv.Itoa(status)}]++
}

// StartedAt 返回注册表创建时间;r 为 nil 时返回零值。
func (r *Registry) StartedAt() time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startedAt
}

// AttachSLO 挂接 SLOEvaluator,使其 webhook 队列/投递计数暴露到 /metrics。
// e 为 nil 时清除挂接。可重复调用。
func (r *Registry) AttachSLO(e *SLOEvaluator) {
	if r == nil {
		return
	}
	r.slo.Store(e)
}

// SLO 返回当前挂接的 evaluator(可能为 nil)。
func (r *Registry) SLO() *SLOEvaluator {
	if r == nil {
		return nil
	}
	return r.slo.Load()
}

// quantileSummary 描述一组延迟样本的关键分位数。
type quantileSummary struct {
	P50 float64 `json:"p50"`
	P75 float64 `json:"p75"`
	P90 float64 `json:"p90"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

// computeQuantiles 对每个 (provider, model) 计算 p50/p75/p90/p95/p99。
// 使用最近 latencySamplesCap 个样本,线性插值式选点(不插值,取 floor 索引)。
func (r *Registry) computeQuantiles() map[latencyKey]quantileSummary {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[latencyKey]quantileSummary, len(r.latencySamples))
	for key, samples := range r.latencySamples {
		if len(samples) == 0 {
			continue
		}
		sorted := make([]float64, len(samples))
		copy(sorted, samples)
		sort.Float64s(sorted)
		out[key] = quantileSummary{
			P50: percentile(sorted, 0.50),
			P75: percentile(sorted, 0.75),
			P90: percentile(sorted, 0.90),
			P95: percentile(sorted, 0.95),
			P99: percentile(sorted, 0.99),
		}
	}
	return out
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := int(float64(len(sorted)-1) * q)
	idx = max(idx, 0)
	idx = min(idx, len(sorted)-1)
	return sorted[idx]
}

// statusBucket 把 HTTP 状态码归一为类,避免 label 基数爆炸。
func statusBucket(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500 && status < 600:
		return "5xx"
	default:
		return "other"
	}
}

// ContextLike 是 SLO evaluator 周期巡检使用的最小 ctx 抽象。
// 避免直接依赖 context 包,以便单元测试可注入。
type ContextLike interface {
	Done() <-chan struct{}
}

// sloProviderSnapshot 描述单个 provider 的 SLO 评估用快照。
type sloProviderSnapshot struct {
	hits     int64
	misses   int64
	errors   int64
	requests int64
	p99MS    float64
	samples  int
}

// sloSnapshot 是 SLO evaluator 一次评估所需的全部输入。
type sloSnapshot struct {
	byProvider map[string]sloProviderSnapshot
}

// snapshotForSLO 在锁内构造 SLO evaluator 所需快照,完成后立即释放锁。
// 按 provider 聚合所有 model 的请求、缓存与延迟样本,避免不同 model 互相覆盖。
func (r *Registry) snapshotForSLO() sloSnapshot {
	if r == nil {
		return sloSnapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// SLO p99:每个 provider 优先 first_event,其次 header,再回退完成请求延迟。
	samplesByProvider := map[string][]float64{}
	copySamples := func(src map[string][]float64) {
		for p, v := range src {
			if len(v) == 0 || len(samplesByProvider[p]) > 0 {
				continue
			}
			cp := make([]float64, len(v))
			copy(cp, v)
			samplesByProvider[p] = cp
		}
	}
	copySamples(r.upstreamFirstEventLatency)
	copySamples(r.upstreamHeaderLatency)
	// 按 provider 回退完成请求延迟(仅该 provider 尚无 attempt 样本时)。
	completedByProv := map[string][]float64{}
	for k, v := range r.latencySamples {
		if len(v) == 0 {
			continue
		}
		cp := make([]float64, len(v))
		copy(cp, v)
		completedByProv[k.Provider] = append(completedByProv[k.Provider], cp...)
	}
	for p, v := range completedByProv {
		if len(samplesByProvider[p]) == 0 && len(v) > 0 {
			samplesByProvider[p] = v
		}
	}

	// 完成请求计数(最终返回)按 provider 汇总。
	completedByProvider := map[string]uint64{}
	for k, v := range r.requestCount {
		completedByProvider[k.Provider] += v
	}

	// 上游 attempt 总数作为错误率分母;若尚未记录 attempt 则回退到完成请求数。
	requestsByProvider := map[string]uint64{}
	for p, v := range r.upstreamAttempts {
		requestsByProvider[p] = v
	}
	for p, v := range completedByProvider {
		if requestsByProvider[p] == 0 {
			requestsByProvider[p] = v
		}
	}

	// 上游错误按 provider 汇总
	errorsByProvider := map[string]uint64{}
	for k, v := range r.upstreamErrors {
		errorsByProvider[k.Provider] += v
	}

	// 缓存命中/未命中按 provider 汇总
	hitsByProvider := map[string]uint64{}
	missesByProvider := map[string]uint64{}
	for k, v := range r.cacheHits {
		hitsByProvider[k.Provider] += v
	}
	for k, v := range r.cacheMisses {
		missesByProvider[k.Provider] += v
	}

	// 收集所有出现过的 provider 名
	providers := map[string]struct{}{}
	for p := range requestsByProvider {
		providers[p] = struct{}{}
	}
	for p := range r.upstreamAttempts {
		providers[p] = struct{}{}
	}
	for p := range errorsByProvider {
		providers[p] = struct{}{}
	}
	for p := range hitsByProvider {
		providers[p] = struct{}{}
	}
	for p := range missesByProvider {
		providers[p] = struct{}{}
	}
	for p := range samplesByProvider {
		providers[p] = struct{}{}
	}

	byProvider := map[string]sloProviderSnapshot{}
	for provider := range providers {
		latSamples := samplesByProvider[provider]
		var p99 float64
		if len(latSamples) > 0 {
			sorted := make([]float64, len(latSamples))
			copy(sorted, latSamples)
			sort.Float64s(sorted)
			p99 = percentile(sorted, 0.99) * 1000
		}
		byProvider[provider] = sloProviderSnapshot{
			hits:     int64(hitsByProvider[provider]),
			misses:   int64(missesByProvider[provider]),
			errors:   int64(errorsByProvider[provider]),
			requests: int64(requestsByProvider[provider]),
			p99MS:    p99,
			samples:  len(latSamples),
		}
	}
	return sloSnapshot{byProvider: byProvider}
}

func boundEndpointLabel(path string) string {
	path = strings.TrimSpace(path)
	switch path {
	case "", "unknown":
		return "unknown"
	case "/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/completions", "/v1/embeddings", "/v1/models":
		return path
	default:
		return "other"
	}
}

func boundProtocolLabel(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "openai", "anthropic":
		return strings.ToLower(strings.TrimSpace(p))
	case "":
		return "unknown"
	default:
		return "other"
	}
}

func boundModeLabel(m string) string {
	switch strings.TrimSpace(m) {
	case "native", "openai_to_anthropic", "anthropic_to_openai", "responses_to_anthropic", "anthropic_to_responses":
		return strings.TrimSpace(m)
	case "":
		return "unknown"
	default:
		return "other"
	}
}

func boundConversionLevel(level int) int {
	if level < 0 || level > 3 {
		return 0
	}
	return level
}
