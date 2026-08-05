package admin

import (
	"encoding/json"
	"net/http"
	"strings"

	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
)

func (h *Handler) refreshChatGPTAccounts(w http.ResponseWriter, r *http.Request) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "chatgpt account pool is unavailable")
		return
	}
	var body struct {
		AccountIDs []string `json:"account_ids"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(&body) != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	result, err := h.chatGPT.RefreshChatGPTAccountsByID(r.Context(), body.AccountIDs)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (h *Handler) addChatGPTAccounts(w http.ResponseWriter, r *http.Request) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "chatgpt account pool is unavailable")
		return
	}
	var body struct {
		Tokens     []string               `json:"tokens"`
		Accounts   []accevents.ExportItem `json:"accounts"`
		SourceType string                 `json:"source_type"`
	}
	if !decodeAdminBody(w, r, &body) {
		return
	}
	if len(body.Tokens) == 0 && len(body.Accounts) == 0 {
		writeError(w, http.StatusBadRequest, "tokens or accounts is required")
		return
	}
	if len(body.Tokens)+len(body.Accounts) > maxAccountImportItems {
		writeError(w, http.StatusBadRequest, "at most 1000 accounts may be imported at once")
		return
	}
	result, err := h.chatGPT.AddChatGPTAccounts(r.Context(), body.Tokens, body.Accounts, body.SourceType)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (h *Handler) deleteChatGPTAccounts(w http.ResponseWriter, r *http.Request) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "chatgpt account pool is unavailable")
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
	result, err := h.chatGPT.DeleteChatGPTAccounts(r.Context(), body.IDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) updateChatGPTAccount(w http.ResponseWriter, r *http.Request, rel string) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "chatgpt account pool is unavailable")
		return
	}
	id := strings.Trim(strings.TrimPrefix(rel, "/api/chatgpt/accounts/"), "/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusBadRequest, "account id is required")
		return
	}
	var body struct {
		Type   *string `json:"type"`
		Status *string `json:"status"`
		Quota  *int    `json:"quota"`
		Proxy  *string `json:"proxy"`
	}
	if !decodeAdminBody(w, r, &body) {
		return
	}
	if body.Type == nil && body.Status == nil && body.Quota == nil && body.Proxy == nil {
		writeError(w, http.StatusBadRequest, "at least one account field is required")
		return
	}
	result, err := h.chatGPT.UpdateChatGPTAccount(r.Context(), accevents.UpdateByIDCommand{ID: id, Type: body.Type, Status: body.Status, Quota: body.Quota, Proxy: body.Proxy})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result.Item.AccessToken = redactAccountToken(result.Item.AccessToken)
	result.Item.Proxy = ""
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) exportChatGPTAccounts(w http.ResponseWriter, r *http.Request) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "chatgpt account pool is unavailable")
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
	result, err := h.chatGPT.ExportChatGPTAccounts(r.Context(), body.IDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(result.Items) == 0 {
		writeError(w, http.StatusBadRequest, "no complete accounts to export; access_token and refresh_token are required")
		return
	}
	// Export is an intentional secret-bearing response: prevent intermediary
	// storage, and never log response bodies in this handler. Keep the
	// Both account pools always export an array that can be submitted back to
	// the corresponding import endpoint. Do not wrap it in an Admin result.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="chatgpt-web-accounts.json"`)
	writeJSON(w, http.StatusOK, result.Items)
}
func (h *Handler) chatGPTAccountRefreshProgress(w http.ResponseWriter, r *http.Request, rel string) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "chatgpt account pool is unavailable")
		return
	}
	id := strings.TrimSpace(strings.TrimPrefix(rel, "/api/chatgpt/accounts/refresh/progress/"))
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusBadRequest, "progress id is required")
		return
	}
	progress, err := h.chatGPT.ChatGPTAccountRefreshProgress(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "refresh progress not found")
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

func (h *Handler) startChatGPTOAuth(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EmailHint string `json:"email_hint"`
	}
	if !decodeAdminBody(w, r, &body) {
		return
	}
	result, err := h.chatGPT.StartChatGPTOAuth(r.Context(), body.EmailHint)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (h *Handler) finishChatGPTOAuth(w http.ResponseWriter, r *http.Request) {
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
	result, err := h.chatGPT.FinishChatGPTOAuth(r.Context(), body.SessionID, body.Callback)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
