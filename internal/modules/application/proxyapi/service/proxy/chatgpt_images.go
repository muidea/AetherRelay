package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptimage"
	"ai-proxy/internal/pkg/chatgptimageinput"
)

type chatGPTImageBody struct {
	Prompt         string   `json:"prompt"`
	Model          string   `json:"model"`
	N              int      `json:"n"`
	Size           string   `json:"size"`
	Quality        string   `json:"quality"`
	ResponseFormat string   `json:"response_format"`
	Image          string   `json:"image"`
	Images         []string `json:"images"`
	ImageURL       string   `json:"image_url"`
}

func (h *Handler) handleImages(w http.ResponseWriter, r *http.Request, requestID string) {
	h.cfgMu.RLock()
	executor, cfg := h.chatGPTImage, h.cfg
	h.cfgMu.RUnlock()
	if executor == nil {
		writeAPIError(w, http.StatusServiceUnavailable, APIError{Code: ErrorCodeProviderUnavailable, Message: "chatgpt image executor is unavailable"})
		return
	}
	var body chatGPTImageBody
	var editImages, masks [][]byte
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		var err error
		body, editImages, masks, err = parseChatGPTImageMultipart(w, r, cfg.MaxRequestBodyBytes)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: ErrorCodeInvalidRequest, Message: err.Error()})
			return
		}
	} else if strings.HasPrefix(contentType, "application/json") {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, cfg.MaxRequestBodyBytes)).Decode(&body); err != nil {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: ErrorCodeInvalidRequest, Message: "invalid JSON image request"})
			return
		}
	} else {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: ErrorCodeInvalidRequest, Message: "image request must be JSON or multipart/form-data"})
		return
	}
	if body.Model == "" {
		body.Model = "gpt-image-2"
	}
	plan, apiErr := ResolveTransportPlan(cfg, h.EffectiveCatalog(), r.Method, r.URL.Path, body.Model)
	if apiErr != nil {
		writeAPIError(w, statusForAPIError(apiErr), *apiErr)
		return
	}
	if plan.UpstreamProtocol != "chatgptweb" {
		writeAPIError(w, http.StatusBadGateway, APIError{Code: ErrorCodeEndpointUnsupported, Message: "image route owner is not ChatGPT Web", Model: body.Model})
		return
	}
	if body.N == 0 {
		body.N = 1
	}
	if body.Quality == "" {
		body.Quality = "auto"
	}
	if body.ResponseFormat == "" {
		body.ResponseFormat = "b64_json"
	}
	request := chatgptimage.Request{Prompt: body.Prompt, Model: body.Model, N: body.N, Size: body.Size, Quality: body.Quality, ResponseFormat: body.ResponseFormat, BaseURL: imageBaseURL(r)}
	var result chatgptimage.Result
	var err error
	if r.URL.Path == "/v1/images/edits" {
		if len(editImages) == 0 {
			values := append([]string(nil), body.Images...)
			if body.Image != "" {
				values = append(values, body.Image)
			}
			if body.ImageURL != "" {
				values = append(values, body.ImageURL)
			}
			editImages, err = imageinput.DecodeBase64Images(values)
		}
		if err == nil && len(masks) > 0 {
			editImages, err = imageinput.CompositeMasks(editImages, masks)
		}
		request.Images = editImages
		if err == nil {
			result, err = executor.EditImage(r.Context(), request)
		}
	} else {
		result, err = executor.GenerateImage(r.Context(), request)
	}
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, APIError{Code: ErrorCodeUpstreamUnavailable, Message: "chatgpt image request failed", Model: body.Model})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func parseChatGPTImageMultipart(w http.ResponseWriter, r *http.Request, limit int64) (chatGPTImageBody, [][]byte, [][]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(limit); err != nil || r.MultipartForm == nil {
		return chatGPTImageBody{}, nil, nil, fmt.Errorf("invalid multipart image request")
	}
	f := r.MultipartForm
	body := chatGPTImageBody{Prompt: firstForm(f.Value, "prompt"), Model: firstForm(f.Value, "model"), Size: firstForm(f.Value, "size"), Quality: firstForm(f.Value, "quality"), ResponseFormat: firstForm(f.Value, "response_format")}
	if n := firstForm(f.Value, "n"); n != "" {
		parsed, err := strconv.Atoi(n)
		if err != nil {
			return body, nil, nil, fmt.Errorf("n must be an integer")
		}
		body.N = parsed
	}
	images, err := multipartImages(f, []string{"image", "image[]", "images", "images[]", "image_url", "image_url[]"}, true)
	if err != nil {
		return body, nil, nil, err
	}
	masks, err := multipartImages(f, []string{"mask", "mask[]"}, false)
	return body, images, masks, err
}

func multipartImages(form *multipart.Form, keys []string, required bool) ([][]byte, error) {
	values := []string{}
	for _, k := range keys {
		values = append(values, form.Value[k]...)
	}
	images := [][]byte{}
	if len(values) > 0 {
		decoded, err := imageinput.DecodeBase64Images(values)
		if err != nil {
			return nil, err
		}
		images = append(images, decoded...)
	}
	for _, k := range keys {
		for _, header := range form.File[k] {
			file, err := header.Open()
			if err != nil {
				return nil, fmt.Errorf("cannot read image file")
			}
			data, readErr := io.ReadAll(io.LimitReader(file, (20<<20)+1))
			closeErr := file.Close()
			if readErr != nil || closeErr != nil || len(data) == 0 {
				return nil, fmt.Errorf("cannot read image file")
			}
			if len(data) > 20<<20 {
				return nil, fmt.Errorf("image exceeds 20 MiB")
			}
			images = append(images, data)
		}
	}
	if required && len(images) == 0 {
		return nil, fmt.Errorf("image file is required")
	}
	return images, nil
}
func firstForm(values map[string][]string, key string) string {
	if values[key] == nil {
		return ""
	}
	return strings.TrimSpace(values[key][0])
}

func imageBaseURL(r *http.Request) string {
	if r == nil || r.Host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
