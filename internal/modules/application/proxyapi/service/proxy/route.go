package proxy

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	"aetherrelay/internal/pkg/aetherrelayconfig"
)

// 客户端协议与 TransportPlan 模式常量。
const (
	ClientProtocolOpenAI    = "openai"
	ClientProtocolAnthropic = "anthropic"

	TransportModeNative               = "native"
	TransportModeOpenAIToAnthropic    = "openai_to_anthropic"
	TransportModeAnthropicToOpenAI    = "anthropic_to_openai"
	TransportModeResponsesToAnthropic = "responses_to_anthropic"
	TransportModeAnthropicToResponses = "anthropic_to_responses"
	// TransportModeChatGPTWebResponses is a bounded, stateless Responses
	// projection backed by the ChatGPT Web text executor. It intentionally is
	// not a native upstream Responses implementation.
	TransportModeChatGPTWebResponses = "chatgptweb_responses"
	// TransportModeCodexOAuthResponses is a native Responses relay backed by
	// the Codex OAuth account pool. No ChatGPT Web message-tree projection is
	// involved.
	TransportModeCodexOAuthResponses = "codex_oauth_responses"
)

// TransportPlan 是请求期唯一转发计划:固定入站协议/path、上游协议/path 与转换方式。
// 只在 EffectiveCatalog 候选之上解析，不允许请求侧修改 RouteOwner。
type TransportPlan struct {
	ModelID          string
	ClientProtocol   string
	ClientEndpoint   string
	RouteOwner       string
	UpstreamProtocol string
	UpstreamEndpoint string
	Mode             string // native | openai_to_anthropic | anthropic_to_openai
	// ConversionLevel is copied from the validated model/direction metadata.
	// Native plans always use level 0.
	ConversionLevel int
	Priority        int
	Fallback        bool
}

// IsConversion 表示需要协议转换(非 native 直通)。
func (p TransportPlan) IsConversion() bool {
	return p.Mode == TransportModeOpenAIToAnthropic || p.Mode == TransportModeAnthropicToOpenAI || p.Mode == TransportModeResponsesToAnthropic || p.Mode == TransportModeAnthropicToResponses
}

// RouteLabel 把入站 HTTP 路径归一化为 Prometheus 标签使用的稳定 route 名。
// 已知路径直接映射,未知路径收敛到 "other",避免基数爆炸。
func RouteLabel(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	path := strings.TrimRight(r.URL.Path, "/")
	switch path {
	case "/v1/chat/completions":
		return "chat_completions"
	case "/v1/messages":
		return "messages"
	case "/v1/responses":
		return "responses"
	case "/v1/search":
		return "search"
	case "/v1/completions":
		return "completions"
	case "/v1/embeddings":
		return "embeddings"
	case "/v1/images/generations", "/v1/images/edits":
		return "images"
	case "/v1/models":
		return "models"
	case "/healthz":
		return "healthz"
	}
	if strings.HasPrefix(path, "/v1/") {
		return "v1_other"
	}
	return "other"
}

// ClientProtocolForPath 由 method+path 决定客户端协议,不从 User-Agent/SDK/body 推断。
func ClientProtocolForPath(path string) string {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	switch path {
	case "/v1/messages":
		return ClientProtocolAnthropic
	case "/v1/chat/completions", "/v1/responses", "/v1/completions", "/v1/embeddings", "/v1/images/generations", "/v1/images/edits", "/v1/models", "/v1/search":
		return ClientProtocolOpenAI
	default:
		return ""
	}
}

// NormalizeClientEndpoint 归一化入站 endpoint path。
func NormalizeClientEndpoint(path string) string {
	return strings.TrimRight(strings.TrimSpace(path), "/")
}

// ProviderHasDirectEndpoint 只检查配置中的上游原生 endpoint。
// 不得与转换后可服务 path 混用。
func ProviderHasDirectEndpoint(provider config.Provider, endpoint string) bool {
	return config.ProviderHasDirectEndpoint(provider, endpoint)
}

// ResolveTransportPlan returns the first compatible candidate for compatibility
// callers. Request executors that can safely retry must use
// ResolveTransportPlans to obtain the complete ordered chain.
func ResolveTransportPlan(cfg config.Config, snap effectivecatalog.Snapshot, method, path, modelID string) (TransportPlan, *APIError) {
	plans, apiErr := ResolveTransportPlans(cfg, snap, method, path, modelID)
	if apiErr != nil {
		return TransportPlan{}, apiErr
	}
	return plans[0], nil
}

// ResolveTransportPlans validates the inbound request and returns every
// compatible candidate in deterministic priority order. It never creates an
// upstream request and is shared by routing, fallback, and Admin projections.
// HTTP candidates can use the complete chain before committing a response;
// stateful builtin executors apply stricter no-duplicate fallback boundaries.
func ResolveTransportPlans(cfg config.Config, snap effectivecatalog.Snapshot, method, path, modelID string) ([]TransportPlan, *APIError) {
	return ResolveTransportPlansForAccess(cfg, snap, clientaccess.All(), method, path, modelID)
}

// ResolveTransportPlansForAccess applies the authenticated client's provider
// policy before endpoint compatibility and request-time health are evaluated.
func ResolveTransportPlansForAccess(cfg config.Config, snap effectivecatalog.Snapshot, policy clientaccess.Policy, method, path, modelID string) ([]TransportPlan, *APIError) {
	clientEndpoint := NormalizeClientEndpoint(path)
	clientProtocol := ClientProtocolForPath(clientEndpoint)
	modelID = strings.TrimSpace(modelID)

	// 入站白名单(执行端点)由 isSupportedInbound 保证;此处仍防御未知 path。
	if clientEndpoint == "" || clientProtocol == "" {
		return nil, &APIError{
			Code:           ErrorCodeEndpointUnsupported,
			Message:        fmt.Sprintf("inbound endpoint %q is not supported", path),
			Model:          modelID,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
		}
	}
	if method != "" && method != http.MethodPost {
		// 执行端点仅 POST;/v1/models 不走本函数。
		return nil, &APIError{
			Code:           ErrorCodeEndpointUnsupported,
			Message:        fmt.Sprintf("method %s is not supported for endpoint %q", method, clientEndpoint),
			Model:          modelID,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
		}
	}

	if modelID == "" {
		return nil, &APIError{
			Code:           ErrorCodeModelRequired,
			Message:        "model is required",
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
		}
	}

	if _, ok := snap.LookupForAccess(modelID, policy); !ok {
		return nil, &APIError{
			Code:           ErrorCodeModelNotFound,
			Message:        fmt.Sprintf("model %q was not found in the effective model catalog", modelID),
			Model:          modelID,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
		}
	}
	candidates := snap.CandidatesForAccess(modelID, policy)

	compatible := make([]TransportPlan, 0, len(candidates))
	for _, candidate := range candidates {
		owner := strings.TrimSpace(candidate.RouteOwner)
		if owner == "" {
			continue
		}
		var provider config.Provider
		if candidate.Builtin || owner == effectivecatalog.BuiltinProviderID || owner == effectivecatalog.CodexOAuthProviderID {
			provider = effectivecatalog.BuiltinProviderViewFor(owner)
		} else {
			var found bool
			provider, found = cfg.Providers[owner]
			if !found || provider.Disabled {
				continue
			}
		}
		for _, plan := range applyTransportMatrix(clientEndpoint, clientProtocol, modelID, owner, provider) {
			if (plan.Mode == TransportModeResponsesToAnthropic || plan.Mode == TransportModeAnthropicToResponses) && !conversionDeclared(cfg, modelID, plan.UpstreamEndpoint, plan.Mode) {
				continue
			}
			plan.Priority = candidate.Priority
			plan.Fallback = candidate.Fallback
			if plan.IsConversion() {
				if metadata, ok := cfg.ModelMetadata[modelID]; ok {
					if capability, ok := config.ModelConversionCapability(metadata, plan.UpstreamEndpoint, plan.Mode); ok {
						plan.ConversionLevel = capability.Level
					}
				}
			}
			compatible = append(compatible, plan)
		}
	}
	// Protocol-preserving candidates always lead cross-protocol conversion,
	// regardless of provider priority. Provider priority and health still order
	// candidates within the same semantic class.
	sort.SliceStable(compatible, func(i, j int) bool {
		if left, right := transportPlanSemanticRank(compatible[i]), transportPlanSemanticRank(compatible[j]); left != right {
			return left < right
		}
		if compatible[i].Priority != compatible[j].Priority {
			return compatible[i].Priority > compatible[j].Priority
		}
		return compatible[i].RouteOwner < compatible[j].RouteOwner
	})
	plans := make([]TransportPlan, 0, len(compatible))
	for _, plan := range compatible {
		if len(plans) > 0 && !plan.Fallback {
			continue
		}
		plans = append(plans, plan)
	}
	if len(plans) == 0 {
		if level, direction := configuredUnavailableConversion(cfg, modelID, clientEndpoint, clientProtocol); level > 0 {
			return nil, &APIError{
				Code:                ErrorCodeConversionUnsupported,
				Message:             fmt.Sprintf("conversion level %d for %s has no compatible model endpoint template", level, direction),
				Feature:             direction,
				UnsupportedFeatures: []string{direction},
				Model:               modelID, ClientEndpoint: clientEndpoint, ClientProtocol: clientProtocol,
			}
		}
		return nil, &APIError{
			Code:           ErrorCodeEndpointUnsupported,
			Message:        fmt.Sprintf("no compatible provider can serve endpoint %q for model %q", clientEndpoint, modelID),
			Model:          modelID,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
		}
	}
	return plans, nil
}

func transportPlanSemanticRank(plan TransportPlan) int {
	switch plan.Mode {
	case TransportModeNative, TransportModeCodexOAuthResponses:
		return 0
	case TransportModeChatGPTWebResponses:
		return 1
	default:
		if plan.IsConversion() {
			return 2
		}
		return 1
	}
}

func configuredUnavailableConversion(cfg config.Config, modelID, endpoint, protocol string) (int, string) {
	metadata, ok := cfg.ModelMetadata[modelID]
	if !ok {
		return 0, ""
	}
	direction := ""
	switch {
	case endpoint == "/v1/responses" && protocol == ClientProtocolOpenAI:
		direction = TransportModeResponsesToAnthropic
	case endpoint == "/v1/messages" && protocol == ClientProtocolAnthropic:
		direction = TransportModeAnthropicToResponses
	}
	if direction == "" {
		return 0, ""
	}
	upstreamEndpoint, ok := config.ConversionUpstreamEndpointForDirection(direction)
	if !ok {
		return 0, ""
	}
	capability, ok := config.ModelConversionCapability(metadata, upstreamEndpoint, direction)
	if !ok {
		return 0, ""
	}
	return capability.Level, direction
}

func conversionDeclared(cfg config.Config, modelID, upstreamEndpoint, mode string) bool {
	metadata, ok := cfg.ModelMetadata[modelID]
	if !ok {
		return false
	}
	_, ok = config.ModelConversionCapability(metadata, upstreamEndpoint, mode)
	return ok
}

// conversionCapabilityUsable keeps route and discovery policy aligned with
// config validation. Reasoning is available only through an explicit,
// direction-specific degraded adapter.
func conversionCapabilityUsable(direction string, capability config.ConversionCapability) bool {
	return config.ConversionCapabilityUsable(direction, capability)
}

// applyTransportMatrix projects the shared routing contract into the
// request-time plan. The matrix itself lives in config.ResolveProviderTransports
// so startup validation and request routing cannot drift.
func applyTransportMatrix(clientEndpoint, clientProtocol, modelID, owner string, provider config.Provider) []TransportPlan {
	upstreamProtocol := strings.TrimSpace(provider.Protocol)
	transports := config.ResolveProviderTransports(provider, clientEndpoint)
	result := make([]TransportPlan, 0, len(transports))
	for _, transport := range transports {
		result = append(result, TransportPlan{
			ModelID: modelID, ClientProtocol: clientProtocol, ClientEndpoint: clientEndpoint,
			RouteOwner: owner, UpstreamProtocol: upstreamProtocol,
			UpstreamEndpoint: transport.UpstreamEndpoint, Mode: transport.Mode,
		})
	}
	return result
}
