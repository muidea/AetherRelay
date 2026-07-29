package admin

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	tempevents "ai-proxy/internal/modules/application/chatgpttemporarychat/pkg/events"
	"ai-proxy/internal/pkg/chatgptimageinput"
)

type temporaryConversationBody struct {
	Model          string `json:"model"`
	ThinkingEffort string `json:"thinking_effort"`
	SystemPrompt   string `json:"system_prompt"`
}

type temporaryTurnBody struct {
	Content string `json:"content"`
}

const temporaryChatMultipartLimit = imageinput.MaxChatImageBytes + (1 << 20)

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
	body, images, err := decodeTemporaryTurnBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Content) == "" && len(images) == 0 {
		writeError(w, http.StatusBadRequest, "content or image is required")
		return
	}
	out, err := h.chatGPT.StartTemporaryTurn(r.Context(), tempevents.StartTurnCommand{
		OwnerID:        ownerID,
		ConversationID: id,
		Content:        body.Content,
		Images:         images,
	})
	if err != nil {
		writeTemporaryChatError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

func decodeTemporaryTurnBody(w http.ResponseWriter, r *http.Request) (temporaryTurnBody, []tempevents.ImageInput, error) {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		var body temporaryTurnBody
		if !decodeAdminBody(w, r, &body) {
			return temporaryTurnBody{}, nil, fmt.Errorf("invalid temporary chat request")
		}
		return body, nil, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, temporaryChatMultipartLimit)
	if err := r.ParseMultipartForm(temporaryChatMultipartLimit); err != nil || r.MultipartForm == nil {
		return temporaryTurnBody{}, nil, fmt.Errorf("invalid multipart temporary chat request")
	}
	body := temporaryTurnBody{Content: firstTemporaryFormValue(r.MultipartForm, "content")}
	images, err := temporaryTurnImages(r.MultipartForm)
	if err != nil {
		return temporaryTurnBody{}, nil, err
	}
	return body, images, nil
}

func firstTemporaryFormValue(form *multipart.Form, key string) string {
	if form == nil || len(form.Value[key]) == 0 {
		return ""
	}
	return form.Value[key][0]
}

func temporaryTurnImages(form *multipart.Form) ([]tempevents.ImageInput, error) {
	if form == nil {
		return nil, nil
	}
	files := make([]*multipart.FileHeader, 0)
	for _, key := range []string{"images", "images[]", "image"} {
		files = append(files, form.File[key]...)
	}
	if len(files) > imageinput.MaxChatImageCount {
		return nil, fmt.Errorf("at most %d images are supported per turn", imageinput.MaxChatImageCount)
	}
	images := make([]tempevents.ImageInput, 0, len(files))
	totalBytes := 0
	for index, header := range files {
		file, err := header.Open()
		if err != nil {
			return nil, fmt.Errorf("cannot read image %d", index+1)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, imageinput.MaxChatImageBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			return nil, fmt.Errorf("cannot read image %d", index+1)
		}
		image, err := imageinput.ValidateImage(data)
		if err != nil {
			return nil, fmt.Errorf("image %d: %w", index+1, err)
		}
		totalBytes += len(image.Bytes)
		if totalBytes > imageinput.MaxChatImageBytes {
			return nil, fmt.Errorf("images exceed %d MiB per turn", imageinput.MaxChatImageBytes>>20)
		}
		images = append(images, tempevents.ImageInput{Bytes: image.Bytes, ContentType: image.MIMEType})
	}
	return images, nil
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

func (h *Handler) getTemporaryMessageImage(w http.ResponseWriter, r *http.Request, rel string) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "temporary chat unavailable")
		return
	}
	ownerID := h.adminOwnerID(r)
	if ownerID == "" {
		writeError(w, http.StatusUnauthorized, "admin login is required")
		return
	}
	conversationID, messageID, imageID := temporaryMessageImageIDs(rel)
	if conversationID == "" || messageID == "" || imageID == "" {
		writeError(w, http.StatusBadRequest, "invalid temporary message image")
		return
	}
	image, err := h.chatGPT.GetTemporaryMessageImage(r.Context(), tempevents.GetMessageImageCommand{
		OwnerID: ownerID, ConversationID: conversationID, MessageID: messageID, ImageID: imageID,
	})
	if err != nil || len(image.Bytes) == 0 {
		writeError(w, http.StatusNotFound, "temporary message image not found")
		return
	}
	w.Header().Set("Content-Type", image.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline")
	_, _ = w.Write(image.Bytes)
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

func temporaryMessageImageIDs(rel string) (conversationID, messageID, imageID string) {
	// /api/chatgpt/temporary-conversations/{id}/messages/{message_id}/images/{image_id}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 8 || parts[0] != "api" || parts[1] != "chatgpt" || parts[2] != "temporary-conversations" || parts[4] != "messages" || parts[6] != "images" {
		return "", "", ""
	}
	return strings.TrimSpace(parts[3]), strings.TrimSpace(parts[5]), strings.TrimSpace(parts[7])
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
	case strings.Contains(lower, "required") || strings.Contains(lower, "invalid") || strings.Contains(lower, "limit") || strings.Contains(lower, "exceeds") || strings.Contains(lower, "image"):
		writeError(w, http.StatusBadRequest, msg)
	default:
		writeError(w, http.StatusBadGateway, "temporary chat failed")
	}
}
