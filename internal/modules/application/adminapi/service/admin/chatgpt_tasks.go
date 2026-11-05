package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	taskevents "aetherrelay/internal/modules/application/chatgptimagetask/pkg/events"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/chatgptimageoutput"
)

type chatGPTTaskBody struct {
	APIKeyID         string   `json:"api_key_id"`
	ClientTaskID     string   `json:"client_task_id"`
	Prompt           string   `json:"prompt"`
	Model            string   `json:"model"`
	Size             string   `json:"size"`
	Quality          string   `json:"quality"`
	ResponseFormat   string   `json:"response_format"`
	Images           []string `json:"images"`
	Image            string   `json:"image"`
	ExtraTimeoutSecs int      `json:"extra_timeout_secs"`
}

func (h *Handler) listChatGPTImageTasks(w http.ResponseWriter, r *http.Request) {
	apiKeyID, ok := h.imageAPIKeyID(w, r.Context(), r.URL.Query().Get("api_key_id"))
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
	if !decodeChatGPTTaskBody(w, r, &body) {
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	apiKeyID, ok := h.imageAPIKeyID(w, r.Context(), body.APIKeyID)
	if !ok {
		return
	}
	if h.rejectDeletingClientKey(w, apiKeyID) {
		return
	}
	if strings.TrimSpace(body.ClientTaskID) == "" || strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "client_task_id and prompt are required")
		return
	}
	if !validateChatGPTTaskImageOptions(w, body) {
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
	if !decodeChatGPTTaskBody(w, r, &body) {
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	apiKeyID, ok := h.imageAPIKeyID(w, r.Context(), body.APIKeyID)
	if !ok {
		return
	}
	if h.rejectDeletingClientKey(w, apiKeyID) {
		return
	}
	if strings.TrimSpace(body.ClientTaskID) == "" || strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "client_task_id and prompt are required")
		return
	}
	if !validateChatGPTTaskImageOptions(w, body) {
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

func validateChatGPTTaskImageOptions(w http.ResponseWriter, body chatGPTTaskBody) bool {
	if err := chatgptimageoutput.ValidateRequest(body.Prompt, body.Size, body.ResponseFormat); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}
func (h *Handler) resumeChatGPTImageTask(w http.ResponseWriter, r *http.Request, rel string) {
	var body chatGPTTaskBody
	if !decodeChatGPTTaskBody(w, r, &body) {
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 5 {
		writeError(w, http.StatusBadRequest, "task_id is required")
		return
	}
	apiKeyID, ok := h.imageAPIKeyID(w, r.Context(), body.APIKeyID)
	if !ok {
		return
	}
	if h.rejectDeletingClientKey(w, apiKeyID) {
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
	if !decodeChatGPTTaskBody(w, r, &body) {
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 5 {
		writeError(w, http.StatusBadRequest, "task_id is required")
		return
	}
	apiKeyID, ok := h.imageAPIKeyID(w, r.Context(), body.APIKeyID)
	if !ok {
		return
	}
	if h.rejectDeletingClientKey(w, apiKeyID) {
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
	if !decodeChatGPTTaskBody(w, r, &body) {
		return
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 5 {
		writeError(w, http.StatusBadRequest, "task_id is required")
		return
	}
	apiKeyID, ok := h.imageAPIKeyID(w, r.Context(), body.APIKeyID)
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
	if !decodeChatGPTTaskBody(w, r, &body) {
		return
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 4 {
		writeError(w, http.StatusBadRequest, "task_id is required")
		return
	}
	apiKeyID, ok := h.imageAPIKeyID(w, r.Context(), body.APIKeyID)
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

func (h *Handler) imageAPIKeyID(w http.ResponseWriter, ctx context.Context, apiKeyID string) (string, bool) {
	if strings.TrimSpace(apiKeyID) == "" {
		apiKeyID = config.BuiltinClientAPIKeyID
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

func decodeChatGPTTaskBody(w http.ResponseWriter, r *http.Request, target *chatGPTTaskBody) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid task json; use current fields including api_key_id")
		return false
	}
	return true
}
