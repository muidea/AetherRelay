package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"aetherrelay/internal/pkg/aetherrelayarchive"
	"aetherrelay/internal/pkg/aetherrelayconfig"
)

// handleAnthropicMessages 处理客户端 POST /v1/messages。
// 通过 TransportPlan 决定 native 或已发布的 Responses 转换。
func (h *Handler) handleAnthropicMessages(w http.ResponseWriter, r *http.Request, requestID string) {
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

	if err := h.writeArchiveRequest(round, bodyBytes); err != nil {
		log.Printf("archive request: %v", err)
	}
	h.archiveAndLogClientRequest(round, r, len(bodyBytes))

	// Route from a small envelope before decoding tool arguments. Codex needs
	// UseNumber from ingress; float64 decoding would irreversibly round them.
	// Keep exact field-name lookup, as in the original map-based parser.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &envelope); err != nil {
		h.writeArchivedError(w, round, r, start, "", "", false, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	var model string
	var stream bool
	_ = json.Unmarshal(envelope["model"], &model)
	_ = json.Unmarshal(envelope["stream"], &stream)
	plans, apiErr := h.resolveTransportPlans(r, model)
	if apiErr != nil {
		h.writeArchivedAPIError(w, round, r, start, "", model, stream, statusForAPIError(apiErr), *apiErr)
		return
	}
	plan := plans[0]
	var body map[string]any
	if plan.Mode == TransportModeAnthropicToCodex && plan.UpstreamProtocol == effectivecatalog.CodexOAuthProviderID {
		err = decodeCodexJSON(bodyBytes, &body)
	} else {
		err = json.Unmarshal(bodyBytes, &body)
	}
	if err != nil {
		h.writeArchivedError(w, round, r, start, "", "", false, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	if round != nil && plan.IsConversion() {
		round.SetTransportPlan(RouteLabel(r), plan.ClientEndpoint, plan.ClientProtocol, plan.UpstreamProtocol, plan.UpstreamEndpoint, plan.Mode)
		round.SetConversionLevel(plan.ConversionLevel)
	}
	if plan.Mode == TransportModeAnthropicToCodex && plan.UpstreamProtocol == effectivecatalog.CodexOAuthProviderID {
		h.handleAnthropicToCodex(w, r, start, plan, model, stream, body)
		return
	}
	candidates, preflightErr := h.prepareAnthropicMessageCandidates(plans, bodyBytes, body, stream, r.URL.RawQuery, r.Method)
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
			h.settleConversionClientCanceled(round, r, selected.Plan.RouteOwner, model, stream, start)
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

	if selectedPlan.Mode == TransportModeAnthropicToResponses {
		r = withAnthropicStops(r, body)
		markConversionDegraded(round, selected.DegradedFeatures)
		if resp.StatusCode >= http.StatusBadRequest {
			h.writeConversionUpstreamError(w, r, resp, round, start, selectedPlan, providerName, model, stream)
			return
		}
		if stream {
			if err := h.handleAnthropicToResponsesStream(w, r, resp, round, start, providerName, model, selected.ConversionCapability); err != nil {
				log.Printf("anthropic→responses stream: %v", err)
			}
			return
		}
		h.handleAnthropicToResponses(w, r, resp, round, start, providerName, model, selected.ConversionCapability)
		return
	}
	h.writePreparedAnthropicNativeResponse(w, r, round, start, selectedPlan, providerName, provider, resp, model, stream, body, result.Cancel)
}

func (h *Handler) writePreparedAnthropicNativeResponse(w http.ResponseWriter, r *http.Request, round *archive.Round, start time.Time, plan TransportPlan, providerName string, provider config.Provider, resp *http.Response, model string, stream bool, body map[string]any, cancel context.CancelFunc) {
	responsePath := responseFileName(resp.Header.Get("Content-Type"), strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "event-stream"))
	if strings.HasSuffix(responsePath, ".sse") {
		copyResponseHeader(w.Header(), resp.Header)
		prepareSSEHeaders(w.Header())
		w.WriteHeader(resp.StatusCode)
		usage, fullPath, streamErr := h.copyAndArchiveRawStream(w, resp, round, providerName, provider, model, body, r.Context(), cancel, plan.UpstreamEndpoint)
		duration := time.Since(start)
		errMsg := ""
		if streamErr != nil {
			errMsg = streamErr.Error()
		}
		h.recordAndPrintFail(round, r, providerName, model, true, resp.StatusCode, duration, usage, streamErr)
		h.writeArchiveMetadata(round, providerName, model, true, resp.StatusCode, duration, usage, responsePath, errMsg, fullPath, outcomeFromStreamFail(streamErr, resp.StatusCode))
		return
	}
	responseBody, err := h.readLimitedUpstream(resp.Body)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, err.Error())
		return
	}
	responseBody, responseHeader, err := h.decodedResponseBodyAndHeader(responseBody, resp.Header)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, err.Error())
		return
	}
	copyResponseHeader(w.Header(), responseHeader)
	w.WriteHeader(resp.StatusCode)
	if len(responseBody) > 0 {
		_, _ = w.Write(responseBody)
	}
	if err := h.writeArchiveResponse(round, responsePath, responseBody); err != nil {
		log.Printf("archive anthropic response: %v", err)
	}
	usage := tokenUsage{}
	if resp.StatusCode < http.StatusBadRequest {
		usage = usageFromRawResponse(provider, responseBody, body)
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, providerName, model, stream, resp.StatusCode, duration, usage, "")
	h.writeArchiveMetadata(round, providerName, model, stream, resp.StatusCode, duration, usage, responsePath, "", "", "")
}

type conversionLocationError struct {
	Path string
	Err  error
}

func (e *conversionLocationError) Error() string { return e.Path + ": " + e.Err.Error() }
func (e *conversionLocationError) Unwrap() error { return e.Err }

// Keep a bounded capability name independent of nested field paths.
type conversionUnsupportedFeatureError struct{ Feature string }

func (e *conversionUnsupportedFeatureError) Error() string { return e.Feature }

func conversionAPIError(plan TransportPlan, err error) APIError {
	var limit *conversionLimitError
	if errors.As(err, &limit) {
		retryable := false
		return APIError{
			Code: ErrorCodeConversionLimitExceeded, Message: limit.Error(),
			Param: limit.Path, LimitKind: limit.Kind, Limit: limit.Limit, Actual: limit.Actual,
			Retryable: &retryable, Model: plan.ModelID, ClientEndpoint: plan.ClientEndpoint,
			ClientProtocol: plan.ClientProtocol, UpstreamProtocol: plan.UpstreamProtocol,
		}
	}
	feature := conversionFeatureFromError(err)
	path := ""
	var located *conversionLocationError
	if errors.As(err, &located) {
		path = located.Path
		if feature == "unsupported_feature" {
			feature = path
		}
	}
	if path == "" && feature != "unsupported_feature" && err.Error() == feature {
		// Top-level rejected fields have no nested location wrapper.
		path = feature
	}
	msg := err.Error()
	if feature != "" {
		msg = fmt.Sprintf("conversion does not support %q; simplify request or use a native endpoint contract", feature)
	}
	if path != "" {
		msg += " (at " + path + ")"
	}
	return APIError{
		Param:            path,
		Code:             ErrorCodeConversionUnsupported,
		Message:          msg,
		Model:            plan.ModelID,
		ClientEndpoint:   plan.ClientEndpoint,
		ClientProtocol:   plan.ClientProtocol,
		UpstreamProtocol: plan.UpstreamProtocol,
		Feature:          feature,
		UnsupportedFeatures: func() []string {
			if feature == "" {
				return nil
			}
			return []string{feature}
		}(),
	}
}

func conversionFeatureFromError(err error) string {
	if err == nil {
		return ""
	}
	var unsupported *conversionUnsupportedFeatureError
	if errors.As(err, &unsupported) {
		return unsupported.Feature
	}
	msg := err.Error()
	for _, prefix := range []string{"metadata.", "cache_control", "output_config."} {
		if strings.HasPrefix(msg, prefix) {
			return strings.Fields(msg)[0]
		}
	}
	// 从已知错误文案提取 feature 名。
	for _, key := range []string{
		"tools", "tool_choice", "functions", "function_call", "response_format",
		"max_output_tokens", "max_tokens", "stream",
		"stop_sequences", "stop",
		"parallel_tool_calls", "disable_parallel_tool_use", "store", "truncation", "include", "conversation", "prompt", "metadata", "service_tier", "thinking", "background", "reasoning", "previous_response_id", "text.format",
		"tool_result.is_error", "tool_calls", "tool role", "tool_use", "tool_result", "tool call", "tool result",
		"image", "image_url", "input_image", "audio", "input_audio", "document", "file",
	} {
		if strings.Contains(msg, key) || strings.Contains(msg, `"`+key+`"`) {
			return key
		}
	}
	if strings.Contains(msg, "role ") {
		return "unsupported_role"
	}
	if strings.Contains(msg, "content type") {
		return "unsupported_content"
	}
	return "unsupported_feature"
}

// writeConversionUpstreamError 保留上游 HTTP status,输出客户端协议兼容的安全错误 envelope。
func (h *Handler) writeConversionUpstreamError(w http.ResponseWriter, r *http.Request, resp *http.Response, round *archive.Round, start time.Time, plan TransportPlan, providerName, model string, stream bool) {
	responseBody, err := h.readLimitedUpstream(resp.Body)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, err.Error())
		return
	}
	responseBody, responseHeader, err := h.decodedResponseBodyAndHeader(responseBody, resp.Header)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, err.Error())
		return
	}

	// 按客户端协议转换错误 envelope;无法安全解析时输出 AetherRelay typed error。
	outBody, contentType := convertUpstreamErrorForClient(plan, resp.StatusCode, responseBody, providerName)
	header := responseHeader.Clone()
	header.Del("Content-Length")
	header.Del("Content-Encoding")
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	copyResponseHeader(w.Header(), header)
	w.WriteHeader(resp.StatusCode)
	if len(outBody) > 0 {
		_, _ = w.Write(outBody)
	}
	responsePath := responseFileName(contentType, false)
	if err := h.writeArchiveResponse(round, responsePath, outBody); err != nil {
		log.Printf("archive converted upstream error: %v", err)
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, providerName, model, stream, resp.StatusCode, duration, tokenUsage{}, "")
	h.writeArchiveMetadata(round, providerName, model, stream, resp.StatusCode, duration, tokenUsage{}, responsePath, "", "", "")
}

// convertUpstreamErrorForClient 将上游错误体转换为客户端协议 envelope,并脱敏。
func convertUpstreamErrorForClient(plan TransportPlan, status int, upstreamBody []byte, routeOwner string) ([]byte, string) {
	safeMsg := summarizeUpstreamError(upstreamBody, status, routeOwner)
	switch plan.ClientProtocol {
	case ClientProtocolOpenAI:
		// Anthropic upstream → OpenAI client
		payload := map[string]any{
			"error": map[string]any{
				"message": safeMsg,
				"type":    "upstream_error",
				"code":    "upstream_error",
				"param":   nil,
			},
		}
		b, _ := json.Marshal(payload)
		return b, "application/json"
	case ClientProtocolAnthropic:
		// OpenAI upstream → Anthropic client
		payload := map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": safeMsg,
			},
		}
		b, _ := json.Marshal(payload)
		return b, "application/json"
	default:
		payload := APIErrorResponse{Error: APIError{
			Code:             "upstream_error",
			Message:          safeMsg,
			Model:            plan.ModelID,
			ClientEndpoint:   plan.ClientEndpoint,
			ClientProtocol:   plan.ClientProtocol,
			UpstreamProtocol: plan.UpstreamProtocol,
		}}
		b, _ := json.Marshal(payload)
		return b, "application/json"
	}
}

func summarizeUpstreamError(body []byte, status int, routeOwner string) string {
	// 不回传完整上游 body;仅提取安全 message 摘要。
	msg := ""
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err == nil {
		if errObj, ok := parsed["error"].(map[string]any); ok {
			if m, ok := errObj["message"].(string); ok {
				msg = m
			} else if m, ok := errObj["type"].(string); ok {
				msg = m
			}
		} else if m, ok := parsed["message"].(string); ok {
			msg = m
		}
	}
	msg = redactSecrets(msg)
	if len(msg) > 240 {
		msg = msg[:240] + "..."
	}
	if msg == "" {
		msg = fmt.Sprintf("upstream returned HTTP %d", status)
	}
	if routeOwner != "" {
		return fmt.Sprintf("%s (route_owner=%s)", msg, routeOwner)
	}
	return msg
}

func redactSecrets(s string) string {
	// 粗粒度脱敏:去掉看起来像 Bearer/sk-/key 的片段。
	lower := strings.ToLower(s)
	if strings.Contains(lower, "bearer ") || strings.Contains(lower, "api_key") || strings.Contains(lower, "apikey") || strings.Contains(s, "sk-") {
		return "upstream error (details redacted)"
	}
	return s
}

// hasNonEmptyConversionFeature 判断上游可选字段是否包含真实不支持内容。
// null / 缺失 / 空数组 / 空对象视为无 feature,允许基础文本转换继续。

func hasNonEmptyConversionFeature(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		// 其它 JSON 类型(数字/bool 等)视为存在 feature。
		return true
	}
}

// anthropicUsage parses an Anthropic usage object (message_start, message_delta,
// non-stream response) and normalizes it into the internal convention.
//
// CP-OBS-007: Anthropic reports input_tokens WITHOUT the prompt tokens it read
// from or wrote to a cache, while every other protocol and the internal
// accounting treat input tokens as the full prompt with cache reads as a known
// subset. Folding both cache counters into the input here keeps the cache usage
// rate (cached / input) inside [0,1], makes total_tokens the real prompt size,
// and keeps the OpenAI-shaped projection correct. The Anthropic wire shape is
// restored on output by anthropicUsagePayload / anthropicUsageFields.
func anthropicUsage(value any) (tokenUsage, bool) {
	raw, ok := value.(map[string]any)
	if !ok {
		return tokenUsage{}, false
	}
	usage := tokenUsage{Known: true}
	usage.PromptTokens, _ = numberAsInt(raw["input_tokens"])
	usage.CompletionTokens, _ = numberAsInt(raw["output_tokens"])
	applyUsageDetails(&usage, raw)
	foldCacheIntoAnthropicInput(&usage)
	return usage, true
}

// mergeAnthropicUsage replaces present cumulative counters without losing
// fields omitted from a partial stream update.
func mergeAnthropicUsage(usage *tokenUsage, value any) {
	raw, ok := value.(map[string]any)
	if !ok {
		return
	}
	// Delta counters are cumulative, not increments. Restore the uncached
	// input before replacing present fields, then normalize exactly once.
	input := usage.PromptTokens
	if usage.CachedInputTokensKnown {
		input -= usage.CachedInputTokens
	}
	if usage.CacheCreationInputTokensKnown {
		input -= usage.CacheCreationInputTokens
	}
	if n, valid := cacheTokenCount(raw["input_tokens"]); valid {
		input = n
	}
	if n, valid := cacheTokenCount(raw["output_tokens"]); valid {
		usage.CompletionTokens = n
		usage.OutputTokensKnown = true
	}
	usage.PromptTokens = input
	applyUsageDetails(usage, raw)
	foldCacheIntoAnthropicInput(usage)
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	usage.Known = true
}

// foldCacheIntoAnthropicInput adds cache counters exactly once per raw usage.
func foldCacheIntoAnthropicInput(usage *tokenUsage) {
	if usage == nil {
		return
	}
	if usage.CachedInputTokensKnown {
		usage.PromptTokens += usage.CachedInputTokens
	}
	if usage.CacheCreationInputTokensKnown {
		usage.PromptTokens += usage.CacheCreationInputTokens
	}
}

func numberAsInt(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case json.Number:
		i, err := v.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
