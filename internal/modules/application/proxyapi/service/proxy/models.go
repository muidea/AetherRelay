package proxy

import (
	"encoding/json"
	"net/http"
	"time"

	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetricsport"
)

// ModelsListResponse 是 GET/POST /v1/models 的具体外部协议 DTO。
// 禁止使用 map[string]any / []any 动态组装。
type ModelsListResponse struct {
	Object string        `json:"object"`
	Data   []ModelRecord `json:"data"`
}

// ModelRecord 是 catalog 中单个模型的稳定输出。
type ModelRecord struct {
	ID                  string `json:"id"`
	Object              string `json:"object"`
	ContextWindowTokens int    `json:"contextWindowTokens,omitempty"`
	MaxOutputTokens     int    `json:"maxOutputTokens,omitempty"`
	// SupportedEndpoints is derived at runtime from the model's eligible
	// providers and the shared transport matrix. It contains client-facing
	// paths, never provider configuration endpoint names.
	SupportedEndpoints []string           `json:"supported_endpoints,omitempty"`
	Capabilities       *ModelCapabilities `json:"capabilities,omitempty"`
}

type ModelCapabilities struct {
	Reasoning   *ReasoningCapability    `json:"reasoning,omitempty"`
	Native      *NativeCapabilities     `json:"native,omitempty"`
	Conversions *ConversionCapabilities `json:"conversions,omitempty"`
}
type ConversionCapabilities struct {
	ResponsesToAnthropic *ConversionCapability `json:"responses_to_anthropic,omitempty"`
	AnthropicToResponses *ConversionCapability `json:"anthropic_to_responses,omitempty"`
}
type ConversionCapability struct {
	Level     int  `json:"level"`
	Text      bool `json:"text"`
	Images    bool `json:"images"`
	Documents bool `json:"documents"`
	Reasoning bool `json:"reasoning"`
	// ReasoningMode is "degrade" when cross-protocol reasoning controls are
	// adapted but reasoning output is intentionally not exposed as text.
	ReasoningMode    string `json:"reasoning_mode,omitempty"`
	Tools            bool   `json:"tools"`
	StructuredOutput bool   `json:"structured_output"`
	Streaming        bool   `json:"streaming"`
	Continuation     bool   `json:"continuation"`
}
type NativeCapabilities struct {
	Responses *NativeResponsesCapabilities `json:"responses,omitempty"`
}
type NativeResponsesCapabilities struct {
	Tools  bool `json:"tools"`
	Images bool `json:"images"`
}
type ReasoningCapability struct {
	Supported     bool     `json:"supported"`
	DefaultEffort string   `json:"default_effort,omitempty"`
	Efforts       []string `json:"efforts,omitempty"`
}

// handleModels returns the effective catalog (exact provider models, optional
// static metadata, and auto-discovered account-pool models) as an
// OpenAI-compatible model list.
// 不转发上游;字段 contextWindowTokens / maxOutputTokens 为扩展元数据。
// RouteOwner 仅用于内部路由、归档与观测，不作为客户端发现接口的一部分。
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request, requestID string) {
	start := time.Now()
	round := archiveRoundFromContext(r.Context())
	bodyBytes := []byte(nil)
	if r.Body != nil && r.Method == http.MethodPost {
		var err error
		bodyBytes, err = h.readLimitedBody(w, r)
		if err != nil {
			status := http.StatusBadRequest
			code := ErrorCodeInvalidRequest
			if isRequestTooLarge(err) {
				status = http.StatusRequestEntityTooLarge
				code = ErrorCodeRequestTooLarge
			}
			h.writeArchivedAPIError(w, round, r, start, "", "", false, status, APIError{
				Code:           code,
				Message:        err.Error(),
				ClientProtocol: clientProtocolFromRequest(r),
				ClientEndpoint: NormalizeClientEndpoint(r.URL.Path),
			})
			return
		}
	}
	if r.Body != nil {
		_ = r.Body.Close()
	}
	if len(bodyBytes) > 0 {
		if err := h.writeArchiveRequest(round, bodyBytes); err != nil {
			// best-effort
		}
	}
	h.archiveAndLogClientRequest(round, r, len(bodyBytes))

	payload := buildModelsListResponse(h.EffectiveCatalog())
	body, err := json.Marshal(payload)
	if err != nil {
		h.writeArchivedError(w, round, r, start, "", "", false, http.StatusInternalServerError, err.Error())
		return
	}
	body = append(body, '\n')

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	if err := h.writeArchiveResponse(round, "response.json", body); err != nil {
		// best-effort archive
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, "", "", false, http.StatusOK, duration, tokenUsage{}, "")
	h.writeArchiveMetadata(round, "", "", false, http.StatusOK, duration, tokenUsage{}, "response.json", "", "", "success")
}

func buildModelsListResponse(snap effectivecatalog.Snapshot) ModelsListResponse {
	ids := snap.SortedModelIDs()
	data := make([]ModelRecord, 0, len(ids))
	for _, id := range ids {
		route, ok := snap.Lookup(id)
		if !ok {
			continue
		}
		rec := ModelRecord{
			ID:     route.ModelID,
			Object: "model",
		}
		rec.SupportedEndpoints = modelSupportedEndpoints(snap, id)
		if metadata, ok := snap.ModelMetadata[route.ModelID]; ok && metadata.ReasoningDeclared {
			rec.Capabilities = &ModelCapabilities{Reasoning: &ReasoningCapability{Supported: metadata.ReasoningSupported, DefaultEffort: metadata.ReasoningDefaultEffort, Efforts: append([]string(nil), metadata.ReasoningEfforts...)}}
		}
		if metadata, ok := snap.ModelMetadata[route.ModelID]; ok && metadata.NativeResponsesDeclared {
			if rec.Capabilities == nil {
				rec.Capabilities = &ModelCapabilities{}
			}
			rec.Capabilities.Native = &NativeCapabilities{Responses: &NativeResponsesCapabilities{Tools: metadata.NativeResponsesTools, Images: metadata.NativeResponsesImages}}
		}
		if metadata, ok := snap.ModelMetadata[route.ModelID]; ok {
			for direction, capability := range metadata.ConversionCapabilities {
				if !conversionCapabilityUsable(direction, capability) || !implementedConversionDirection(direction) || !modelHasConversionDirection(snap, id, direction) {
					continue
				}
				if rec.Capabilities == nil {
					rec.Capabilities = &ModelCapabilities{}
				}
				if rec.Capabilities.Conversions == nil {
					rec.Capabilities.Conversions = &ConversionCapabilities{}
				}
				projected := &ConversionCapability{Level: capability.Level, Text: capability.Text, Images: capability.Images, Documents: capability.Documents, Reasoning: capability.Reasoning, Tools: capability.Tools, StructuredOutput: capability.StructuredOutput, Streaming: capability.Streaming, Continuation: capability.Continuation}
				if capability.Reasoning {
					projected.ReasoningMode = "degrade"
				}
				switch direction {
				case "responses_to_anthropic":
					rec.Capabilities.Conversions.ResponsesToAnthropic = projected
				case "anthropic_to_responses":
					rec.Capabilities.Conversions.AnthropicToResponses = projected
				}
			}
		}
		// Every source omits optional capacity metadata when unknown or not applicable.
		if route.ContextWindowTokens > 0 {
			rec.ContextWindowTokens = route.ContextWindowTokens
		}
		if route.MaxOutputTokens > 0 {
			rec.MaxOutputTokens = route.MaxOutputTokens
		}
		data = append(data, rec)
	}
	return ModelsListResponse{Object: "list", Data: data}
}

func modelHasConversionDirection(snap effectivecatalog.Snapshot, modelID, direction string) bool {
	for _, candidate := range snap.CandidatesFor(modelID) {
		for _, mode := range candidate.ConversionModes {
			if mode == direction {
				return true
			}
		}
	}
	return false
}

// Both conversion directions are implemented for text, text SSE and
// function-tool non-streaming requests.  Capability flags that are not yet
// semantically safe are filtered by conversionCapabilityUsable.
func implementedConversionDirection(direction string) bool {
	return direction == "responses_to_anthropic" || direction == "anthropic_to_responses"
}

// modelSupportedEndpoints returns paths exposed by at least one configured or
// discovered catalog candidate. Request-time health and circuit state are
// intentionally applied later and are not part of this stable generation.
func modelSupportedEndpoints(snap effectivecatalog.Snapshot, modelID string) []string {
	seen := map[string]bool{}
	for _, candidate := range snap.CandidatesFor(modelID) {
		for _, path := range candidate.SupportedEndpoints {
			seen[path] = true
		}
	}
	paths := []string{
		"/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/search",
		"/v1/completions", "/v1/embeddings", "/v1/images/generations", "/v1/images/edits",
	}
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if seen[path] {
			result = append(result, path)
		}
	}
	return result
}

// ReserveMetricsModels 为 metrics 预占各 Provider 精确 models 的 label 槽位。
func ReserveMetricsModels(reg metricsport.Reporter, cfg config.Config) {
	if reg == nil {
		return
	}
	for name, provider := range cfg.Providers {
		if provider.Disabled {
			continue
		}
		reg.ReserveModels(name, provider.Models)
	}
}
