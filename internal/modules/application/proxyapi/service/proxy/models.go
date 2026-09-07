package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	"aetherrelay/internal/pkg/aetherrelayclientauth"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelaymetricsport"
)

// ModelsListResponse 是普通 GET /v1/models 的具体外部协议 DTO。
// 禁止使用 map[string]any / []any 动态组装。
type ModelsListResponse struct {
	Object string        `json:"object"`
	Data   []ModelRecord `json:"data"`
}

// ModelRecord 是 catalog 中单个模型的稳定输出。
type ModelRecord struct {
	ID                     string `json:"id"`
	Object                 string `json:"object"`
	ContextWindowTokens    int    `json:"contextWindowTokens,omitempty"`
	MaxContextWindowTokens int    `json:"maxContextWindowTokens,omitempty"`
	MaxOutputTokens        int    `json:"maxOutputTokens,omitempty"`
	// SupportedEndpoints is derived at runtime from the model's eligible
	// providers and the shared transport matrix. It contains client-facing
	// paths, never provider configuration endpoint names.
	SupportedEndpoints []string           `json:"supported_endpoints,omitempty"`
	Capabilities       *ModelCapabilities `json:"capabilities,omitempty"`
}

type ModelCapabilities struct {
	Reasoning   *ReasoningCapability    `json:"reasoning,omitempty"`
	Native      *NativeCapabilities     `json:"native,omitempty"`
	Codex       *CodexCapabilities      `json:"codex,omitempty"`
	Conversions *ConversionCapabilities `json:"conversions,omitempty"`
}

type CodexCapabilities struct {
	Compact       string `json:"compact"`
	Websocket     string `json:"websocket"`
	FunctionTools string `json:"function_tools"`
	ParallelTools string `json:"parallel_tools"`
	ImageInput    string `json:"image_input"`
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

// CodexModelsManifest is selected by the presence of the client_version query
// parameter used by Codex custom providers.
type CodexModelsManifest struct {
	Models []CodexModelManifestRecord `json:"models"`
}

type CodexModelManifestRecord struct {
	Slug                        string                      `json:"slug"`
	DisplayName                 string                      `json:"display_name"`
	Description                 string                      `json:"description"`
	DefaultReasoningLevel       string                      `json:"default_reasoning_level,omitempty"`
	SupportedReasoningLevels    []CodexReasoningLevelRecord `json:"supported_reasoning_levels"`
	InputModalities             []string                    `json:"input_modalities"`
	UseResponsesLite            bool                        `json:"use_responses_lite"`
	PreferWebsockets            bool                        `json:"prefer_websockets"`
	SupportsImageDetailOriginal bool                        `json:"supports_image_detail_original,omitempty"`
	SupportsSearchTool          bool                        `json:"supports_search_tool,omitempty"`
	MultiAgentVersion           string                      `json:"multi_agent_version,omitempty"`
	MultiAgentReasoningEffort   string                      `json:"multi_agent_reasoning_effort,omitempty"`
	CompHash                    string                      `json:"comp_hash,omitempty"`
	ContextWindow               int                         `json:"context_window,omitempty"`
	MaxContextWindow            int                         `json:"max_context_window,omitempty"`
	BaseInstructions            string                      `json:"base_instructions"`
	MinimalClientVersion        string                      `json:"minimal_client_version"`
	Visibility                  string                      `json:"visibility"`
	Priority                    int                         `json:"priority"`
	ServiceTiers                []any                       `json:"service_tiers"`
	SupportedInAPI              bool                        `json:"supported_in_api"`
}

type CodexReasoningLevelRecord struct {
	Effort      string `json:"effort"`
	Description string `json:"description,omitempty"`
}

type codexTrustedModelProfile struct {
	DisplayName                 string
	Description                 string
	MinimalClientVersion        string
	BaseInstructions            string
	UseResponsesLite            bool
	InputModalities             []string
	SupportsImageDetailOriginal bool
	SupportsSearchTool          bool
	MultiAgentVersion           string
	MultiAgentReasoningEffort   string
	CompHash                    string
	ContextWindow               int
	MaxContextWindow            int
	DefaultReasoningLevel       string
	SupportedReasoningLevels    []string
	ServiceTiers                []any
}

func trustedCodexModelProfile(model string) (codexTrustedModelProfile, bool) {
	if model != "gpt-6-astra" {
		return codexTrustedModelProfile{}, false
	}
	return codexTrustedModelProfile{
		DisplayName:                 "GPT-6-Astra",
		Description:                 "Our most capable model for complex, demanding work.",
		MinimalClientVersion:        "0.153.0",
		BaseInstructions:            "You are Codex, an agent based on GPT-6.",
		UseResponsesLite:            true,
		InputModalities:             []string{"text", "image"},
		SupportsImageDetailOriginal: true,
		SupportsSearchTool:          true,
		MultiAgentVersion:           "v2",
		MultiAgentReasoningEffort:   "xhigh",
		CompHash:                    "3000",
		ContextWindow:               272000,
		MaxContextWindow:            872000,
		DefaultReasoningLevel:       "medium",
		SupportedReasoningLevels:    []string{"low", "medium", "high", "xhigh", "max", "ultra"},
		ServiceTiers: []any{map[string]any{
			"id":          "priority",
			"name":        "Fast",
			"description": "2x speed, increased usage",
		}},
	}, true
}

// handleModels returns the effective catalog as either an OpenAI-compatible
// list or, when client_version is present, a Codex client manifest.
// 不转发上游;字段 contextWindowTokens / maxOutputTokens 为扩展元数据。
// RouteOwner 仅用于内部路由、归档与观测，不作为客户端发现接口的一部分。
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request, requestID string) {
	start := time.Now()
	round := archiveRoundFromContext(r.Context())
	bodyBytes := []byte(nil)
	if r.Body != nil {
		_ = r.Body.Close()
	}
	if len(bodyBytes) > 0 {
		if err := h.writeArchiveRequest(round, bodyBytes); err != nil {
			// best-effort
		}
	}
	h.archiveAndLogClientRequest(round, r, len(bodyBytes))

	identity := clientauth.ClientIdentityFromContext(r.Context())
	snapshot := h.EffectiveCatalog()
	var payload any
	if _, codexClient := r.URL.Query()["client_version"]; codexClient {
		payload = buildCodexModelsManifest(snapshot, identity.ProviderAccess, r.URL.Query().Get("client_version"))
	} else {
		payload = buildModelsListResponse(snapshot, identity.ProviderAccess)
	}
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

func buildCodexModelsManifest(snap effectivecatalog.Snapshot, policy clientaccess.Policy, clientVersion string) CodexModelsManifest {
	models := make([]CodexModelManifestRecord, 0)
	for priority, record := range buildModelsListResponse(snap, policy).Data {
		if !containsString(record.SupportedEndpoints, "/v1/responses") {
			continue
		}
		profile, trusted := trustedCodexModelProfile(record.ID)
		efforts := []string{"medium"}
		defaultEffort := "medium"
		if trusted {
			efforts = append([]string(nil), profile.SupportedReasoningLevels...)
			defaultEffort = profile.DefaultReasoningLevel
		}
		if record.Capabilities != nil && record.Capabilities.Reasoning != nil {
			if !record.Capabilities.Reasoning.Supported {
				efforts = nil
				defaultEffort = ""
			} else {
				efforts = append([]string(nil), record.Capabilities.Reasoning.Efforts...)
				defaultEffort = ""
				if containsString(efforts, record.Capabilities.Reasoning.DefaultEffort) {
					defaultEffort = record.Capabilities.Reasoning.DefaultEffort
				} else if len(efforts) > 0 {
					defaultEffort = efforts[0]
				}
			}
		}
		if !codexClientSupportsExtendedReasoning(clientVersion) {
			filtered := efforts[:0]
			for _, effort := range efforts {
				if effort != "max" && effort != "ultra" {
					filtered = append(filtered, effort)
				}
			}
			efforts = filtered
			if len(efforts) == 0 {
				defaultEffort = ""
			} else if !containsString(efforts, defaultEffort) {
				defaultEffort = efforts[0]
			}
		}
		levels := make([]CodexReasoningLevelRecord, 0, len(efforts))
		for _, effort := range efforts {
			level := CodexReasoningLevelRecord{Effort: effort}
			if trusted {
				level.Description = codexReasoningLevelDescription(effort)
			}
			levels = append(levels, level)
		}
		modalities := []string{"text"}
		if trusted {
			modalities = append([]string(nil), profile.InputModalities...)
		} else if record.Capabilities != nil && record.Capabilities.Native != nil && record.Capabilities.Native.Responses != nil && record.Capabilities.Native.Responses.Images {
			modalities = append(modalities, "image")
		}
		manifest := CodexModelManifestRecord{
			Slug: record.ID, DisplayName: record.ID, Description: record.ID,
			DefaultReasoningLevel: defaultEffort, SupportedReasoningLevels: levels,
			InputModalities: modalities, UseResponsesLite: false,
			PreferWebsockets: modelHasRouteOwner(snap, record.ID, effectivecatalog.CodexOAuthProviderID, policy),
			ContextWindow:    manifestContextWindow(record.ContextWindowTokens),
			MaxContextWindow: manifestMaxContextWindow(record.ContextWindowTokens, record.MaxContextWindowTokens),
			BaseInstructions: "You are Codex, an AI coding assistant.", MinimalClientVersion: "0.147.0",
			Visibility: "list", Priority: priority + 1, ServiceTiers: []any{}, SupportedInAPI: true,
		}
		if trusted {
			manifest.DisplayName = profile.DisplayName
			manifest.Description = profile.Description
			manifest.BaseInstructions = profile.BaseInstructions
			manifest.MinimalClientVersion = profile.MinimalClientVersion
			manifest.UseResponsesLite = profile.UseResponsesLite
			manifest.SupportsImageDetailOriginal = profile.SupportsImageDetailOriginal
			manifest.SupportsSearchTool = profile.SupportsSearchTool
			manifest.MultiAgentVersion = profile.MultiAgentVersion
			manifest.MultiAgentReasoningEffort = profile.MultiAgentReasoningEffort
			manifest.CompHash = profile.CompHash
			manifest.ContextWindow = profile.ContextWindow
			manifest.MaxContextWindow = profile.MaxContextWindow
			manifest.ServiceTiers = append([]any(nil), profile.ServiceTiers...)
			if !containsString(efforts, manifest.MultiAgentReasoningEffort) {
				manifest.MultiAgentReasoningEffort = defaultEffort
			}
		}
		if record.Capabilities != nil && record.Capabilities.Native != nil && record.Capabilities.Native.Responses != nil {
			manifest.InputModalities = []string{"text"}
			if record.Capabilities.Native.Responses.Images {
				manifest.InputModalities = append(manifest.InputModalities, "image")
			} else {
				manifest.SupportsImageDetailOriginal = false
			}
		}
		if record.ContextWindowTokens > 0 {
			manifest.ContextWindow = manifestContextWindow(record.ContextWindowTokens)
			manifest.MaxContextWindow = manifestMaxContextWindow(record.ContextWindowTokens, record.MaxContextWindowTokens)
		}
		models = append(models, manifest)
	}
	return CodexModelsManifest{Models: models}
}

func codexReasoningLevelDescription(effort string) string {
	switch effort {
	case "low":
		return "Fast responses with lighter reasoning"
	case "medium":
		return "Balances speed and reasoning depth for everyday tasks"
	case "high":
		return "Greater reasoning depth for complex problems"
	case "xhigh":
		return "Extra high reasoning depth for complex problems"
	case "max":
		return "Maximum reasoning depth for the hardest problems"
	case "ultra":
		return "Maximum reasoning with automatic task delegation"
	default:
		return ""
	}
}

func codexClientSupportsExtendedReasoning(clientVersion string) bool {
	clientVersion = strings.TrimSpace(clientVersion)
	if clientVersion == "" {
		return true
	}
	comparison, valid := compareCodexClientVersions(clientVersion, "0.144.0")
	return !valid || comparison >= 0
}

func compareCodexClientVersions(left, right string) (int, bool) {
	parse := func(value string) ([]int64, bool) {
		value = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(value, "v"), "V"))
		if index := strings.IndexAny(value, "-+"); index >= 0 {
			value = value[:index]
		}
		parts := strings.Split(value, ".")
		values := make([]int64, 0, len(parts))
		for _, part := range parts {
			if strings.TrimSpace(part) == "" {
				return nil, false
			}
			number, err := strconv.ParseInt(part, 10, 64)
			if err != nil || number < 0 {
				return nil, false
			}
			values = append(values, number)
		}
		return values, len(values) > 0
	}
	leftParts, leftOK := parse(left)
	rightParts, rightOK := parse(right)
	if !leftOK || !rightOK {
		return 0, false
	}
	length := len(leftParts)
	if len(rightParts) > length {
		length = len(rightParts)
	}
	for index := 0; index < length; index++ {
		var leftValue, rightValue int64
		if index < len(leftParts) {
			leftValue = leftParts[index]
		}
		if index < len(rightParts) {
			rightValue = rightParts[index]
		}
		if leftValue < rightValue {
			return -1, true
		}
		if leftValue > rightValue {
			return 1, true
		}
	}
	return 0, true
}

func manifestContextWindow(value int) int {
	if value > 0 {
		return value
	}
	return 128000
}

func manifestMaxContextWindow(contextWindow, maxContextWindow int) int {
	if maxContextWindow > 0 {
		return maxContextWindow
	}
	return manifestContextWindow(contextWindow)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func buildModelsListResponse(snap effectivecatalog.Snapshot, policy clientaccess.Policy) ModelsListResponse {
	ids := snap.SortedModelIDsForAccess(policy)
	data := make([]ModelRecord, 0, len(ids))
	for _, id := range ids {
		route, ok := snap.LookupForAccess(id, policy)
		if !ok {
			continue
		}
		rec := ModelRecord{
			ID:     route.ModelID,
			Object: "model",
		}
		rec.SupportedEndpoints = modelSupportedEndpoints(snap, id, policy)
		if modelHasRouteOwner(snap, id, effectivecatalog.CodexOAuthProviderID, policy) {
			rec.Capabilities = &ModelCapabilities{Codex: &CodexCapabilities{
				Compact: "supported", Websocket: "supported", FunctionTools: "unknown", ParallelTools: "unknown", ImageInput: "unknown",
			}}
		}
		if metadata, ok := snap.ModelMetadata[route.ModelID]; ok && metadata.ReasoningDeclared {
			if rec.Capabilities == nil {
				rec.Capabilities = &ModelCapabilities{}
			}
			rec.Capabilities.Reasoning = &ReasoningCapability{Supported: metadata.ReasoningSupported, DefaultEffort: metadata.ReasoningDefaultEffort, Efforts: append([]string(nil), metadata.ReasoningEfforts...)}
		}
		if metadata, ok := snap.ModelMetadata[route.ModelID]; ok && metadata.NativeResponsesDeclared {
			if rec.Capabilities == nil {
				rec.Capabilities = &ModelCapabilities{}
			}
			rec.Capabilities.Native = &NativeCapabilities{Responses: &NativeResponsesCapabilities{Tools: metadata.NativeResponsesTools, Images: metadata.NativeResponsesImages}}
			if rec.Capabilities.Codex != nil {
				rec.Capabilities.Codex.FunctionTools = capabilityState(metadata.NativeResponsesTools)
				rec.Capabilities.Codex.ImageInput = capabilityState(metadata.NativeResponsesImages)
			}
		}
		if metadata, ok := snap.ModelMetadata[route.ModelID]; ok {
			for endpoint, capability := range metadata.ConversionCapabilities {
				direction, ok := config.ConversionDirectionForUpstreamEndpoint(endpoint)
				if !ok {
					continue
				}
				if !conversionCapabilityUsable(direction, capability) || !implementedConversionDirection(direction) || !modelHasConversionDirection(snap, id, direction, policy) {
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
		if route.MaxContextWindowTokens > 0 {
			rec.MaxContextWindowTokens = route.MaxContextWindowTokens
		}
		if route.MaxOutputTokens > 0 {
			rec.MaxOutputTokens = route.MaxOutputTokens
		}
		data = append(data, rec)
	}
	return ModelsListResponse{Object: "list", Data: data}
}

func capabilityState(supported bool) string {
	if supported {
		return "supported"
	}
	return "unsupported"
}

func modelHasRouteOwner(snap effectivecatalog.Snapshot, modelID, owner string, policy clientaccess.Policy) bool {
	for _, candidate := range snap.CandidatesForAccess(modelID, policy) {
		if candidate.RouteOwner == owner {
			return true
		}
	}
	return false
}

func modelHasConversionDirection(snap effectivecatalog.Snapshot, modelID, direction string, policy clientaccess.Policy) bool {
	for _, candidate := range snap.CandidatesForAccess(modelID, policy) {
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
func modelSupportedEndpoints(snap effectivecatalog.Snapshot, modelID string, policy clientaccess.Policy) []string {
	seen := map[string]bool{}
	for _, candidate := range snap.CandidatesForAccess(modelID, policy) {
		for _, path := range candidate.SupportedEndpoints {
			seen[path] = true
		}
	}
	if modelHasRouteOwner(snap, modelID, effectivecatalog.CodexOAuthProviderID, policy) {
		seen["/v1/responses/compact"] = true
	}
	if seen["/v1/responses"] {
		seen["/v1/responses/input_tokens"] = true
	}
	paths := []string{
		"/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/responses/input_tokens", "/v1/responses/compact", "/v1/search",
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
