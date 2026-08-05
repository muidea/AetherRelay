package proxy

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
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
}

// handleModels returns the effective catalog (static model_catalog plus
// auto-discovered ChatGPT Web models) as an OpenAI-compatible model list.
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
		// Auto-discovered models omit capacity when unknown; static catalog keeps
		// its required capacity fields when present.
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

// ReserveMetricsModels 为 metrics 预占 model label 槽位。
// 1) 各 provider 的精确 models(不含通配);
// 2) model_catalog 中已确定 RouteOwner 的 ID。
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
	catalogIDs := make([]string, 0, len(cfg.ModelCatalog))
	for _, info := range cfg.ModelCatalog {
		if id := strings.TrimSpace(info.ID); id != "" {
			catalogIDs = append(catalogIDs, id)
		}
	}
	sort.Strings(catalogIDs)
	for _, id := range catalogIDs {
		info, ok := cfg.ModelCatalog[id]
		if !ok || strings.TrimSpace(info.RouteOwner) == "" {
			continue
		}
		reg.ReserveModels(info.RouteOwner, []string{id})
	}
}
