package admin

import (
	"net/http"
	"strconv"
	"strings"

	tempevents "ai-proxy/internal/modules/application/chatgpttemporarychat/pkg/events"
)

type temporaryConversationBody struct {
	Model          string `json:"model"`
	ThinkingEffort string `json:"thinking_effort"`
	SystemPrompt   string `json:"system_prompt"`
}

type temporaryTurnBody struct {
	Content string `json:"content"`
}

func (h *Handler) adminOwnerID(r *http.Request) string {
	if sess := h.sessionFromRequest(r); sess != nil && strings.TrimSpace(sess.Username) != "" {
		return strings.TrimSpace(sess.Username)
	}
	if h.authEnabled() {
		return ""
	}
	// Auth disabled single-admin deployments still need a stable owner scope.
	cfg := h.auth.snapshot()
	if name := strings.TrimSpace(cfg.Username); name != "" {
		return name
	}
	return "admin"
}

func (h *Handler) createTemporaryConversation(w http.ResponseWriter, r *http.Request) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "temporary chat unavailable")
		return
	}
	ownerID := h.adminOwnerID(r)
	if ownerID == "" {
		writeError(w, http.StatusUnauthorized, "admin login is required")
		return
	}
	var body temporaryConversationBody
	if !decodeAdminBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	out, err := h.chatGPT.CreateTemporaryConversation(r.Context(), tempevents.CreateConversationCommand{
		OwnerID:        ownerID,
		Model:          strings.TrimSpace(body.Model),
		ThinkingEffort: strings.TrimSpace(body.ThinkingEffort),
		SystemPrompt:   strings.TrimSpace(body.SystemPrompt),
	})
	if err != nil {
		writeTemporaryChatError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out.Conversation)
}

func (h *Handler) listTemporaryConversations(w http.ResponseWriter, r *http.Request) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "temporary chat unavailable")
		return
	}
	ownerID := h.adminOwnerID(r)
	if ownerID == "" {
		writeError(w, http.StatusUnauthorized, "admin login is required")
		return
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	out, err := h.chatGPT.ListTemporaryConversations(r.Context(), tempevents.ListConversationsCommand{
		OwnerID: ownerID,
		Cursor:  strings.TrimSpace(r.URL.Query().Get("cursor")),
		Limit:   limit,
	})
	if err != nil {
		writeTemporaryChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getTemporaryConversation(w http.ResponseWriter, r *http.Request, rel string) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "temporary chat unavailable")
		return
	}
	ownerID := h.adminOwnerID(r)
	if ownerID == "" {
		writeError(w, http.StatusUnauthorized, "admin login is required")
		return
	}
	id := temporaryConversationID(rel)
	if id == "" {
		writeError(w, http.StatusBadRequest, "conversation id is required")
		return
	}
	var before *int64
	if raw := strings.TrimSpace(r.URL.Query().Get("before_sequence")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			writeError(w, http.StatusBadRequest, "invalid before_sequence")
			return
		}
		before = &value
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	out, err := h.chatGPT.GetTemporaryConversation(r.Context(), tempevents.GetConversationCommand{
		OwnerID:        ownerID,
		ConversationID: id,
		BeforeSequence: before,
		Limit:          limit,
	})
	if err != nil {
		writeTemporaryChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) startTemporaryTurn(w http.ResponseWriter, r *http.Request, rel string) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "temporary chat unavailable")
		return
	}
	ownerID := h.adminOwnerID(r)
	if ownerID == "" {
		writeError(w, http.StatusUnauthorized, "admin login is required")
		return
	}
	id := temporaryConversationID(rel)
	if id == "" {
		writeError(w, http.StatusBadRequest, "conversation id is required")
		return
	}
	var body temporaryTurnBody
	if !decodeAdminBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	out, err := h.chatGPT.StartTemporaryTurn(r.Context(), tempevents.StartTurnCommand{
		OwnerID:        ownerID,
		ConversationID: id,
		Content:        body.Content,
	})
	if err != nil {
		writeTemporaryChatError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

func (h *Handler) pullTemporaryTurn(w http.ResponseWriter, r *http.Request, rel string) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "temporary chat unavailable")
		return
	}
	ownerID := h.adminOwnerID(r)
	if ownerID == "" {
		writeError(w, http.StatusUnauthorized, "admin login is required")
		return
	}
	conversationID, turnID := temporaryTurnIDs(rel)
	if conversationID == "" || turnID == "" {
		writeError(w, http.StatusBadRequest, "conversation id and turn id are required")
		return
	}
	timeoutMS, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("timeout_ms")))
	out, err := h.chatGPT.PullTemporaryTurn(r.Context(), tempevents.PullTurnCommand{
		OwnerID:        ownerID,
		ConversationID: conversationID,
		TurnID:         turnID,
		TimeoutMillis:  timeoutMS,
	})
	if err != nil {
		writeTemporaryChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) cancelTemporaryTurn(w http.ResponseWriter, r *http.Request, rel string) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "temporary chat unavailable")
		return
	}
	ownerID := h.adminOwnerID(r)
	if ownerID == "" {
		writeError(w, http.StatusUnauthorized, "admin login is required")
		return
	}
	conversationID, turnID := temporaryTurnIDs(rel)
	if conversationID == "" || turnID == "" {
		writeError(w, http.StatusBadRequest, "conversation id and turn id are required")
		return
	}
	out, err := h.chatGPT.CancelTemporaryTurn(r.Context(), tempevents.CancelTurnCommand{
		OwnerID:        ownerID,
		ConversationID: conversationID,
		TurnID:         turnID,
	})
	if err != nil {
		writeTemporaryChatError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

func (h *Handler) deleteTemporaryConversation(w http.ResponseWriter, r *http.Request, rel string) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "temporary chat unavailable")
		return
	}
	ownerID := h.adminOwnerID(r)
	if ownerID == "" {
		writeError(w, http.StatusUnauthorized, "admin login is required")
		return
	}
	id := temporaryConversationID(rel)
	if id == "" {
		writeError(w, http.StatusBadRequest, "conversation id is required")
		return
	}
	if _, err := h.chatGPT.DeleteTemporaryConversation(r.Context(), tempevents.DeleteConversationCommand{
		OwnerID:        ownerID,
		ConversationID: id,
	}); err != nil {
		writeTemporaryChatError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func temporaryConversationID(rel string) string {
	// /api/chatgpt/temporary-conversations/{id}[/*]
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) < 3 || parts[0] != "api" || parts[1] != "chatgpt" || parts[2] != "temporary-conversations" {
		return ""
	}
	if len(parts) < 4 {
		return ""
	}
	return strings.TrimSpace(parts[3])
}

func temporaryTurnIDs(rel string) (conversationID, turnID string) {
	// /api/chatgpt/temporary-conversations/{id}/turns/{turn_id}/events|cancel
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) < 6 {
		return "", ""
	}
	if parts[0] != "api" || parts[1] != "chatgpt" || parts[2] != "temporary-conversations" || parts[4] != "turns" {
		return "", ""
	}
	return strings.TrimSpace(parts[3]), strings.TrimSpace(parts[5])
}

func writeTemporaryChatError(w http.ResponseWriter, err error) {
	if err == nil {
		writeError(w, http.StatusInternalServerError, "temporary chat failed")
		return
	}
	msg := strings.TrimSpace(err.Error())
	// Strip formatted *cd.Error prefix when present.
	if i := strings.Index(msg, "message:"); i >= 0 {
		msg = strings.TrimSpace(msg[i+len("message:"):])
		if j := strings.Index(msg, ","); j >= 0 {
			msg = strings.TrimSpace(msg[:j])
		}
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "not found"):
		writeError(w, http.StatusNotFound, "conversation not found")
	case strings.Contains(lower, "expired"):
		writeError(w, http.StatusGone, "conversation expired")
	case strings.Contains(lower, "streaming") || strings.Contains(lower, "recovery"):
		writeError(w, http.StatusConflict, msg)
	case strings.Contains(lower, "unavailable") || strings.Contains(lower, "no available"):
		writeError(w, http.StatusServiceUnavailable, msg)
	case strings.Contains(lower, "upstream") || strings.Contains(lower, "failed to start"):
		writeError(w, http.StatusBadGateway, "upstream request failed")
	case strings.Contains(lower, "required") || strings.Contains(lower, "invalid") || strings.Contains(lower, "limit") || strings.Contains(lower, "exceeds"):
		writeError(w, http.StatusBadRequest, msg)
	default:
		writeError(w, http.StatusBadGateway, "temporary chat failed")
	}
}
