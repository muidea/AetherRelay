package admin

import (
	"net/http"
	"strings"

	codexevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

func (h *Handler) listCodexAccounts(w http.ResponseWriter, r *http.Request) {
	if h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "Codex account pool is unavailable")
		return
	}
	items, err := h.codex.ListCodexAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) importCodexAccounts(w http.ResponseWriter, r *http.Request) {
	if h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "Codex account pool is unavailable")
		return
	}
	var body struct {
		Accounts []codexevents.CredentialInput `json:"accounts"`
	}
	if !decodeAdminBody(w, r, &body) {
		return
	}
	if len(body.Accounts) == 0 {
		writeError(w, http.StatusBadRequest, "accounts is required")
		return
	}
	if len(body.Accounts) > maxAccountImportItems {
		writeError(w, http.StatusBadRequest, "at most 1000 accounts may be imported at once")
		return
	}
	result, err := h.codex.ImportCodexAccounts(r.Context(), body.Accounts)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (h *Handler) deleteCodexAccounts(w http.ResponseWriter, r *http.Request) {
	if h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "Codex account pool is unavailable")
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decodeAdminBody(w, r, &body) {
		return
	}
	if len(body.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required")
		return
	}
	result, err := h.codex.DeleteCodexAccounts(r.Context(), body.IDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) updateCodexAccount(w http.ResponseWriter, r *http.Request, rel string) {
	if h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "Codex account pool is unavailable")
		return
	}
	id := strings.Trim(strings.TrimPrefix(rel, "/api/codex/accounts/"), "/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusBadRequest, "account id is required")
		return
	}
	var body struct {
		Status *string `json:"status"`
		Proxy  *string `json:"proxy"`
	}
	if !decodeAdminBody(w, r, &body) {
		return
	}
	if body.Status == nil && body.Proxy == nil {
		writeError(w, http.StatusBadRequest, "at least one account field is required")
		return
	}
	result, err := h.codex.UpdateCodexAccount(r.Context(), codexevents.UpdateCommand{ID: id, Status: body.Status, Proxy: body.Proxy})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) refreshCodexAccounts(w http.ResponseWriter, r *http.Request) {
	if h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "Codex account pool is unavailable")
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decodeAdminBody(w, r, &body) {
		return
	}
	if len(body.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required")
		return
	}
	result, err := h.codex.RefreshCodexAccounts(r.Context(), body.IDs)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) exportCodexAccounts(w http.ResponseWriter, r *http.Request) {
	if h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "Codex account pool is unavailable")
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decodeAdminBody(w, r, &body) {
		return
	}
	if len(body.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required")
		return
	}
	result, err := h.codex.ExportCodexAccounts(r.Context(), body.IDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(result.Items) == 0 {
		writeError(w, http.StatusBadRequest, "no complete Codex OAuth accounts to export")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="codex-oauth-accounts.json"`)
	writeJSON(w, http.StatusOK, result.Items)
}

func (h *Handler) startCodexModelDiscovery(w http.ResponseWriter, r *http.Request) {
	if h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "Codex account pool is unavailable")
		return
	}
	var body struct {
		AccountIDs []string `json:"account_ids"`
	}
	if !decodeAdminBody(w, r, &body) {
		return
	}
	progress, err := h.codex.StartCodexModelDiscovery(r.Context(), body.AccountIDs)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, progress)
}

func (h *Handler) codexModelDiscoveryProgress(w http.ResponseWriter, r *http.Request, rel string) {
	if h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "Codex account pool is unavailable")
		return
	}
	progressID := strings.Trim(strings.TrimPrefix(rel, "/api/codex/accounts/discovery/progress/"), "/")
	if progressID == "" || strings.Contains(progressID, "/") {
		writeError(w, http.StatusBadRequest, "discovery progress id is required")
		return
	}
	progress, err := h.codex.CodexModelDiscoveryProgress(r.Context(), progressID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

func (h *Handler) startCodexUsageRefresh(w http.ResponseWriter, r *http.Request) {
	if h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "Codex account pool is unavailable")
		return
	}
	var body struct {
		AccountIDs []string `json:"account_ids"`
	}
	if !decodeAdminBody(w, r, &body) {
		return
	}
	progress, err := h.codex.StartCodexUsageRefresh(r.Context(), body.AccountIDs)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, progress)
}

func (h *Handler) codexUsageRefreshProgress(w http.ResponseWriter, r *http.Request, rel string) {
	if h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "Codex account pool is unavailable")
		return
	}
	progressID := strings.Trim(strings.TrimPrefix(rel, "/api/codex/accounts/usage/progress/"), "/")
	if progressID == "" || strings.Contains(progressID, "/") {
		writeError(w, http.StatusBadRequest, "usage progress id is required")
		return
	}
	progress, err := h.codex.CodexUsageRefreshProgress(r.Context(), progressID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

func (h *Handler) startCodexOAuth(w http.ResponseWriter, r *http.Request) {
	if h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "Codex account pool is unavailable")
		return
	}
	var body struct {
		EmailHint string `json:"email_hint"`
		Proxy     string `json:"proxy"`
	}
	if !decodeAdminBody(w, r, &body) {
		return
	}
	result, err := h.codex.StartCodexOAuth(r.Context(), body.EmailHint, body.Proxy)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) finishCodexOAuth(w http.ResponseWriter, r *http.Request) {
	if h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "Codex account pool is unavailable")
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
		Callback  string `json:"callback"`
	}
	if !decodeAdminBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.SessionID) == "" || strings.TrimSpace(body.Callback) == "" {
		writeError(w, http.StatusBadRequest, "session_id and callback are required")
		return
	}
	result, err := h.codex.FinishCodexOAuth(r.Context(), body.SessionID, body.Callback)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
