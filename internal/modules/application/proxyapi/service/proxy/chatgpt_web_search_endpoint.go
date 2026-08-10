package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptsearch"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
)

// webSearchRequest is AetherRelay's intentionally narrow /v1/search contract.
// It is not an OpenAI endpoint alias: one plain-text query becomes one
// isolated, forced ChatGPT Web search conversation.
type webSearchRequest struct {
	Model string `json:"model"`
	Query string `json:"query"`
}

type webSearchSource struct {
	Title   string `json:"title,omitempty"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

type webSearchResponse struct {
	ID         string            `json:"id"`
	Object     string            `json:"object"`
	Created    int64             `json:"created"`
	Model      string            `json:"model"`
	OutputText string            `json:"output_text"`
	Sources    []webSearchSource `json:"sources"`
	Usage      tokenUsage        `json:"usage"`
}

func decodeWebSearchRequest(body []byte) (webSearchRequest, *APIError) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return webSearchRequest{}, &APIError{Code: ErrorCodeInvalidRequest, Message: "invalid JSON search request"}
	}
	for field := range fields {
		if field != "model" && field != "query" {
			return webSearchRequest{}, &APIError{Code: ErrorCodeConversionUnsupported, Message: fmt.Sprintf("/v1/search does not support %s", field), Feature: field}
		}
	}
	var request webSearchRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return webSearchRequest{}, &APIError{Code: ErrorCodeInvalidRequest, Message: "model and query must be strings"}
	}
	request.Model = strings.TrimSpace(request.Model)
	request.Query = strings.TrimSpace(request.Query)
	if request.Model == "" {
		return webSearchRequest{}, &APIError{Code: ErrorCodeModelRequired, Message: "model is required"}
	}
	if request.Query == "" {
		return webSearchRequest{}, &APIError{Code: ErrorCodeInvalidRequest, Message: "query is required", Feature: "query"}
	}
	return request, nil
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request, _ string) {
	started := time.Now()
	round := archiveRoundFromContext(r.Context())
	body, err := h.readLimitedBody(w, r)
	if err != nil {
		status, code := http.StatusBadRequest, ErrorCodeInvalidRequest
		if isRequestTooLarge(err) {
			status, code = http.StatusRequestEntityTooLarge, ErrorCodeRequestTooLarge
		}
		h.writeArchivedAPIError(w, round, r, started, "", "", false, status, APIError{Code: code, Message: err.Error()})
		return
	}
	defer r.Body.Close()

	if len(body) > 0 {
		stableHash, fingerprint := ComputeRequestFingerprint(body)
		round.SetFingerprint(stableHash, fingerprint)
	}
	// Archive write failures are non-fatal, matching the existing proxy
	// endpoint behaviour.
	_ = h.writeArchiveRequest(round, body)
	h.archiveAndLogClientRequest(round, r, len(body))

	request, apiErr := decodeWebSearchRequest(body)
	if apiErr != nil {
		h.writeArchivedAPIError(w, round, r, started, "", "", false, statusForAPIError(apiErr), *apiErr)
		return
	}
	plans, apiErr := h.resolveTransportPlans(r, request.Model)
	if apiErr != nil {
		snapshot := h.EffectiveCatalog()
		if snapshot.BuiltinProvider.Status != effectivecatalog.StatusReady {
			reason := strings.TrimSpace(snapshot.BuiltinProvider.UnavailableReason)
			if reason == "" {
				reason = "ChatGPT Web account-pool search capability is unavailable"
			}
			unavailable := APIError{Code: ErrorCodeProviderUnavailable, Message: reason, Model: request.Model}
			h.writeArchivedAPIError(w, round, r, started, effectivecatalog.BuiltinProviderID, request.Model, false, http.StatusServiceUnavailable, unavailable)
			return
		}
		h.writeArchivedAPIError(w, round, r, started, "", request.Model, false, statusForAPIError(apiErr), *apiErr)
		return
	}
	plan := plans[0]
	if plan.UpstreamProtocol != effectivecatalog.BuiltinProviderID || plan.UpstreamEndpoint != "chatgptweb_search" {
		// applyTransportMatrix guarantees this branch is unreachable. Keep the
		// defensive error explicit so future route changes cannot weaken the
		// forced-search provider boundary.
		apiErr := APIError{Code: ErrorCodeProviderUnavailable, Message: "no ChatGPT Web provider is available for search", Model: request.Model}
		h.writeArchivedAPIError(w, round, r, started, plan.RouteOwner, request.Model, false, http.StatusServiceUnavailable, apiErr)
		return
	}
	h.archiveAndLogTransportPlan(round, r, plan, effectivecatalog.BuiltinProviderView(), false)
	h.handleChatGPTWebSearchEndpoint(w, r, started, plan.RouteOwner, request)
}

func (h *Handler) handleChatGPTWebSearchEndpoint(w http.ResponseWriter, r *http.Request, started time.Time, provider string, request webSearchRequest) {
	round := archiveRoundFromContext(r.Context())
	h.cfgMu.RLock()
	executor := h.chatGPTSearch
	h.cfgMu.RUnlock()
	if executor == nil {
		apiErr := APIError{Code: ErrorCodeProviderUnavailable, Message: "chatgpt web search executor is unavailable", Model: request.Model}
		h.writeChatGPTWebAPIError(w, round, r, started, provider, request.Model, false, http.StatusServiceUnavailable, apiErr, streamFailFromKind(chatgptfail.KindProviderUnavailable, apiErr.Code+": "+apiErr.Message, nil), tokenUsage{})
		return
	}

	result, err := executor.Search(r.Context(), chatgptsearch.Request{Model: request.Model, Query: request.Query})
	billingModel := firstNonEmpty(result.ActualModel, request.Model)
	usageRequest := chatgpttext.Request{Model: request.Model, Messages: []chatgpttext.Message{{Role: "user", Content: request.Query}}}
	tok := estimateChatGPTTextUsage(usageRequest, result.Text)
	if err != nil {
		fail := streamFailFromChatGPTSearchErr(err)
		h.writeChatGPTWebAPIError(w, round, r, started, provider, billingModel, false, statusForChatGPTFailure(fail), APIError{Code: ErrorCodeUpstreamUnavailable, Message: "chatgpt web search failed: " + chatGPTFailureCode(fail), Model: billingModel}, fail, tok)
		return
	}

	text := cleanChatGPTWebSearchText(result.Text)
	tok = estimateChatGPTTextUsage(usageRequest, text)
	payload := webSearchResponse{
		ID:         "search-" + firstNonEmpty(result.ConversationID, "chatgptweb"),
		Object:     "search.result",
		Created:    time.Now().Unix(),
		Model:      billingModel,
		OutputText: text,
		Sources:    webSearchSources(result.Sources),
		Usage:      tok,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		h.settleChatGPTWeb(round, r, provider, billingModel, false, http.StatusOK, time.Since(started), tok, newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", err, false))
		return
	}
	if archived, err := json.Marshal(payload); err == nil {
		_ = h.writeArchiveResponse(round, "response.json", append(archived, '\n'))
	}
	h.settleChatGPTWeb(round, r, provider, billingModel, false, http.StatusOK, time.Since(started), tok, nil)
}

func webSearchSources(sources []chatgptsearch.Source) []webSearchSource {
	result := make([]webSearchSource, 0, len(sources))
	for _, source := range sources {
		url := strings.TrimSpace(source.URL)
		if url == "" {
			continue
		}
		result = append(result, webSearchSource{
			Title:   strings.TrimSpace(source.Title),
			URL:     url,
			Snippet: strings.TrimSpace(source.Snippet),
		})
	}
	return result
}
