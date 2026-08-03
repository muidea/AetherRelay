package proxy

import (
	"fmt"
	"net/http"
	"strings"

	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"ai-proxy/internal/pkg/aiproxyconfig"
)

// 客户端协议与 TransportPlan 模式常量。
const (
	ClientProtocolOpenAI    = "openai"
	ClientProtocolAnthropic = "anthropic"

	TransportModeNative            = "native"
	TransportModeOpenAIToAnthropic = "openai_to_anthropic"
	TransportModeAnthropicToOpenAI = "anthropic_to_openai"
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
// 只在 ResolvedModelRoute 之上解析,不允许修改 RouteOwner。
type TransportPlan struct {
	ModelID          string
	Operation        string
	ClientProtocol   string
	ClientEndpoint   string
	RouteOwner       string
	UpstreamProtocol string
	UpstreamEndpoint string
	Mode             string // native | openai_to_anthropic | anthropic_to_openai
	Priority         int
	Fallback         bool
}

// IsConversion 表示需要协议转换(非 native 直通)。
func (p TransportPlan) IsConversion() bool {
	return p.Mode == TransportModeOpenAIToAnthropic || p.Mode == TransportModeAnthropicToOpenAI
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

// OperationForPath 将入站 path 映射为 model_catalog.operations 合同枚举。
// /v1/models 返回空字符串(不适用)。
func OperationForPath(path string) string {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	switch path {
	case "/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/completions", "/v1/search":
		return config.ModelOperationChatCompletions
	case "/v1/embeddings":
		return config.ModelOperationEmbeddings
	case "/v1/images/generations", "/v1/images/edits":
		return config.ModelOperationImageGenerations
	default:
		return ""
	}
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

// ProviderHasDirectEndpoint 只检查配置中的上游直连 endpoint capability。
// 不得与转换后可服务 path 混用。
func ProviderHasDirectEndpoint(provider config.Provider, capability string) bool {
	return config.ProviderHasDirectEndpoint(provider, capability)
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
func ResolveTransportPlans(cfg config.Config, snap effectivecatalog.Snapshot, method, path, modelID string) ([]TransportPlan, *APIError) {
	clientEndpoint := NormalizeClientEndpoint(path)
	operation := OperationForPath(clientEndpoint)
	clientProtocol := ClientProtocolForPath(clientEndpoint)
	modelID = strings.TrimSpace(modelID)

	// 入站白名单(执行端点)由 isSupportedInbound 保证;此处仍防御未知 path。
	if clientEndpoint == "" || operation == "" || clientProtocol == "" {
		return nil, &APIError{
			Code:           ErrorCodeEndpointUnsupported,
			Message:        fmt.Sprintf("inbound endpoint %q is not supported", path),
			Model:          modelID,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
			Operation:      operation,
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
			Operation:      operation,
		}
	}

	if modelID == "" {
		return nil, &APIError{
			Code:           ErrorCodeModelRequired,
			Message:        "model is required",
			Operation:      operation,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
		}
	}

	if _, ok := snap.Lookup(modelID); !ok {
		return nil, &APIError{
			Code:           ErrorCodeModelNotFound,
			Message:        fmt.Sprintf("model %q was not found in the effective model catalog", modelID),
			Model:          modelID,
			Operation:      operation,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
		}
	}
	candidates := snap.CandidatesFor(modelID, operation)
	if len(candidates) == 0 {
		return nil, &APIError{
			Code:           ErrorCodeOperationUnsupported,
			Message:        fmt.Sprintf("model %q does not support operation %q", modelID, operation),
			Model:          modelID,
			Operation:      operation,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
		}
	}

	plans := make([]TransportPlan, 0, len(candidates))
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
		plan, ok := applyTransportMatrix(clientEndpoint, clientProtocol, operation, modelID, owner, provider)
		if !ok {
			continue
		}
		if len(plans) > 0 && !candidate.Fallback {
			continue
		}
		plan.Priority = candidate.Priority
		plan.Fallback = candidate.Fallback
		plans = append(plans, plan)
	}
	if len(plans) == 0 {
		return nil, &APIError{
			Code:           ErrorCodeEndpointUnsupported,
			Message:        fmt.Sprintf("no compatible provider can serve endpoint %q for model %q", clientEndpoint, modelID),
			Model:          modelID,
			Operation:      operation,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
		}
	}
	return plans, nil
}

// applyTransportMatrix 仅应用固定转发矩阵(设计文档 §9)。
// 矩阵以外组合返回 false → endpoint_unsupported。
func applyTransportMatrix(clientEndpoint, clientProtocol, operation, modelID, owner string, provider config.Provider) (TransportPlan, bool) {
	upstreamProtocol := strings.TrimSpace(provider.Protocol)
	base := TransportPlan{
		ModelID:          modelID,
		Operation:        operation,
		ClientProtocol:   clientProtocol,
		ClientEndpoint:   clientEndpoint,
		RouteOwner:       owner,
		UpstreamProtocol: upstreamProtocol,
	}

	switch clientEndpoint {
	case "/v1/chat/completions":
		if upstreamProtocol == "chatgptweb" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityChatCompletions) {
			base.UpstreamEndpoint = "chatgptweb"
			base.Mode = TransportModeNative
			return base, true
		}
		// OpenAI client → OpenAI native / Anthropic conversion
		if upstreamProtocol == "openai" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityChatCompletions) {
			base.UpstreamEndpoint = "/v1/chat/completions"
			base.Mode = TransportModeNative
			return base, true
		}
		if upstreamProtocol == "anthropic" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityMessages) {
			base.UpstreamEndpoint = "/v1/messages"
			base.Mode = TransportModeOpenAIToAnthropic
			return base, true
		}
	case "/v1/messages":
		// Anthropic client -> Anthropic native / OpenAI conversion
		if upstreamProtocol == "anthropic" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityMessages) {
			base.UpstreamEndpoint = "/v1/messages"
			base.Mode = TransportModeNative
			return base, true
		}
		if upstreamProtocol == "openai" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityChatCompletions) {
			base.UpstreamEndpoint = "/v1/chat/completions"
			base.Mode = TransportModeAnthropicToOpenAI
			return base, true
		}
	case "/v1/responses":
		if upstreamProtocol == "chatgptweb" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityResponses) {
			base.UpstreamEndpoint = "chatgptweb_responses"
			base.Mode = TransportModeChatGPTWebResponses
			return base, true
		}
		if upstreamProtocol == "openai" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityResponses) {
			base.UpstreamEndpoint = "/v1/responses"
			base.Mode = TransportModeNative
			return base, true
		}
		if upstreamProtocol == effectivecatalog.CodexOAuthProviderID && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityResponses) {
			base.UpstreamEndpoint = "codex_oauth_responses"
			base.Mode = TransportModeCodexOAuthResponses
			return base, true
		}
	case "/v1/search":
		// /v1/search is an ai-proxy extension for the one Web-search operation
		// that can be represented faithfully. Never route it to a same-model
		// static Provider: that would silently become a normal completion or an
		// unrelated upstream tool call.
		if upstreamProtocol == "chatgptweb" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityChatCompletions) {
			base.UpstreamEndpoint = "chatgptweb_search"
			base.Mode = TransportModeNative
			return base, true
		}
	case "/v1/completions":
		if upstreamProtocol == "openai" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityCompletions) {
			base.UpstreamEndpoint = "/v1/completions"
			base.Mode = TransportModeNative
			return base, true
		}
	case "/v1/embeddings":
		if upstreamProtocol == "openai" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityEmbeddings) {
			base.UpstreamEndpoint = "/v1/embeddings"
			base.Mode = TransportModeNative
			return base, true
		}
	case "/v1/images/generations", "/v1/images/edits":
		if upstreamProtocol == "chatgptweb" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityImages) {
			base.UpstreamEndpoint = "chatgptweb_images"
			base.Mode = TransportModeNative
			return base, true
		}
		if upstreamProtocol == "openai" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityImages) {
			base.UpstreamEndpoint = clientEndpoint
			base.Mode = TransportModeNative
			return base, true
		}
	}
	return TransportPlan{}, false
}

// TransportPlanForCanonical 在启动校验时判断 RouteOwner 是否能为 operation 的 canonical path 生成 plan。
// 与请求期 ResolveTransportPlan 使用同一矩阵,但不做 model catalog 查找。
func TransportPlanForCanonical(providerName string, provider config.Provider, modelID, operation string) (TransportPlan, bool) {
	path := config.OperationToPrimaryInboundPath(operation)
	if path == "" {
		return TransportPlan{}, false
	}
	clientProtocol := ClientProtocolForPath(path)
	return applyTransportMatrix(path, clientProtocol, operation, modelID, providerName, provider)
}
