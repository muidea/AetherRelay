package admin

import (
	"net/http"
	"strings"

	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
)

type featureSearchBody struct {
	Model string `json:"model"`
	Query string `json:"query"`
}

// executeFeatureSearch is the Admin feature-page adapter for Proxy's bounded
// forced-search command. It intentionally does not proxy a client API key or
// expose a generic upstream tool surface.
func (h *Handler) executeFeatureSearch(w http.ResponseWriter, r *http.Request) {
	runtime, ok := h.chatGPT.(featureSearchRuntime)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "web search is unavailable")
		return
	}
	ownerID := h.adminOwnerID(r)
	if ownerID == "" {
		writeError(w, http.StatusUnauthorized, "admin login is required")
		return
	}
	var body featureSearchBody
	if !decodeAdminBody(w, r, &body) {
		return
	}
	body.Model = strings.TrimSpace(body.Model)
	body.Query = strings.TrimSpace(body.Query)
	if body.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if body.Query == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}
	out, err := runtime.ExecuteFeatureSearch(r.Context(), proxyevents.ExecuteFeatureSearchCommand{OwnerID: ownerID, Model: body.Model, Query: body.Query})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
