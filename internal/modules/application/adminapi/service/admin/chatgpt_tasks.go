package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	taskevents "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
)

type chatGPTTaskBody struct {
	APIKeyID         string   `json:"api_key_id"`
	OwnerID          string   `json:"owner_id,omitempty"` // test/client compatibility; production uses api_key_id
	ClientTaskID     string   `json:"client_task_id"`
	Prompt           string   `json:"prompt"`
	Model            string   `json:"model"`
	Size             string   `json:"size"`
	Quality          string   `json:"quality"`
	Images           []string `json:"images"`
	Image            string   `json:"image"`
	ExtraTimeoutSecs int      `json:"extra_timeout_secs"`
}

func (h *Handler) listChatGPTImageTasks(w http.ResponseWriter, r *http.Request) {
	apiKeyID, ok := h.imageAPIKeyID(w, r.Context(), r.URL.Query().Get("api_key_id"), r.URL.Query().Get("owner_id"))
	if !ok {
		return
	}
	ids := splitAdminCSV(r.URL.Query().Get("ids"))
	out, err := h.chatGPT.ListChatGPTImageTasks(r.Context(), apiKeyID, ids)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (h *Handler) submitChatGPTImageGeneration(w http.ResponseWriter, r *http.Request) {
	var body chatGPTTaskBody
	if !decodeAdminBody(w, r, &body) {
		return
	}
	apiKeyID, ok := h.imageAPIKeyID(w, r.Context(), body.APIKeyID, body.OwnerID)
	if !ok {
		return
	}
	if strings.TrimSpace(body.ClientTaskID) == "" || strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "client_task_id and prompt are required")
		return
	}
	out, err := h.chatGPT.SubmitChatGPTImageGeneration(r.Context(), taskevents.SubmitGenerationCommand{OwnerID: apiKeyID, ClientTaskID: body.ClientTaskID, Prompt: body.Prompt, Model: body.Model, Size: body.Size, Quality: body.Quality, BaseURL: adminImageBaseURL(r)})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, out.Task)
}
func (h *Handler) submitChatGPTImageEdit(w http.ResponseWriter, r *http.Request) {
	var body chatGPTTaskBody
	if !decodeAdminBody(w, r, &body) {
		return
	}
	apiKeyID, ok := h.imageAPIKeyID(w, r.Context(), body.APIKeyID, body.OwnerID)
	if !ok {
		return
	}
	if strings.TrimSpace(body.ClientTaskID) == "" || strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "client_task_id and prompt are required")
		return
	}
	images := append([]string(nil), body.Images...)
	if strings.TrimSpace(body.Image) != "" {
		images = append(images, body.Image)
	}
	if len(images) == 0 {
		writeError(w, http.StatusBadRequest, "image is required")
		return
	}
	out, err := h.chatGPT.SubmitChatGPTImageEdit(r.Context(), taskevents.SubmitEditCommand{OwnerID: apiKeyID, ClientTaskID: body.ClientTaskID, Prompt: body.Prompt, Model: body.Model, Size: body.Size, Quality: body.Quality, BaseURL: adminImageBaseURL(r), Images: images})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, out.Task)
}
func (h *Handler) resumeChatGPTImageTask(w http.ResponseWriter, r *http.Request, rel string) {
	var body chatGPTTaskBody
	if !decodeAdminBody(w, r, &body) {
		return
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 5 {
		writeError(w, http.StatusBadRequest, "task_id is required")
		return
	}
	apiKeyID, ok := h.imageAPIKeyID(w, r.Context(), body.APIKeyID, body.OwnerID)
	if !ok {
		return
	}
	if body.ExtraTimeoutSecs == 0 {
		body.ExtraTimeoutSecs = 30
	}
	out, err := h.chatGPT.ResumeChatGPTImageTask(r.Context(), apiKeyID, parts[3], body.ExtraTimeoutSecs)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, out.Task)
}

func (h *Handler) retryChatGPTImageGeneration(w http.ResponseWriter, r *http.Request, rel string) {
	var body chatGPTTaskBody
	if !decodeAdminBody(w, r, &body) {
		return
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 5 {
		writeError(w, http.StatusBadRequest, "task_id is required")
		return
	}
	apiKeyID, ok := h.imageAPIKeyID(w, r.Context(), body.APIKeyID, body.OwnerID)
	if !ok {
		return
	}
	out, err := h.chatGPT.RetryChatGPTImageGeneration(r.Context(), apiKeyID, parts[3], adminImageBaseURL(r))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, out.Task)
}

func (h *Handler) cancelChatGPTImageTask(w http.ResponseWriter, r *http.Request, rel string) {
	var body chatGPTTaskBody
	if !decodeAdminBody(w, r, &body) {
		return
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 5 {
		writeError(w, http.StatusBadRequest, "task_id is required")
		return
	}
	apiKeyID, ok := h.imageAPIKeyID(w, r.Context(), body.APIKeyID, body.OwnerID)
	if !ok {
		return
	}
	out, err := h.chatGPT.CancelChatGPTImageTask(r.Context(), apiKeyID, parts[3])
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out.Task)
}

func (h *Handler) deleteChatGPTImageTask(w http.ResponseWriter, r *http.Request, rel string) {
	var body chatGPTTaskBody
	if !decodeAdminBody(w, r, &body) {
		return
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 4 {
		writeError(w, http.StatusBadRequest, "task_id is required")
		return
	}
	apiKeyID, ok := h.imageAPIKeyID(w, r.Context(), body.APIKeyID, body.OwnerID)
	if !ok {
		return
	}
	out, err := h.chatGPT.DeleteChatGPTImageTask(r.Context(), apiKeyID, parts[3])
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) imageAPIKeyID(w http.ResponseWriter, ctx context.Context, apiKeyID, legacyOwner string) (string, bool) {
	if strings.TrimSpace(apiKeyID) == "" && h.usageStore == nil {
		apiKeyID = legacyOwner
	}
	if h.usageStore == nil && strings.TrimSpace(apiKeyID) == "" {
		apiKeyID = "default"
	}
	if h.usageStore == nil && strings.TrimSpace(apiKeyID) != "" {
		return strings.TrimSpace(apiKeyID), true
	}
	return h.requireExistingClientAPIKeyID(w, ctx, apiKeyID)
}
func decodeAdminBody(w http.ResponseWriter, r *http.Request, target any) bool {
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(target) != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return false
	}
	return true
}
func splitAdminCSV(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
