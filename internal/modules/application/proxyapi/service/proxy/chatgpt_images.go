package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptfail"
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptimage"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	archive "ai-proxy/internal/pkg/aiproxyarchive"
	"ai-proxy/internal/pkg/aiproxyconfig"
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
	start := time.Now()
	round := archiveRoundFromContext(r.Context())
	h.cfgMu.RLock()
	executor, cfg := h.chatGPTImage, h.cfg
	h.cfgMu.RUnlock()
	bodyLimit := cfg.MaxRequestBodyBytes
	if bodyLimit <= 0 {
		bodyLimit = config.DefaultMaxRequestBodyBytes
	}
	if executor == nil {
		h.writeChatGPTImageAPIError(w, round, r, start, "", "", false, http.StatusServiceUnavailable, APIError{Code: ErrorCodeProviderUnavailable, Message: "chatgpt image executor is unavailable"}, streamFailFromKind(chatgptfail.KindProviderUnavailable, "provider_unavailable: chatgpt image executor is unavailable", nil), tokenUsage{})
		return
	}
	var body chatGPTImageBody
	var editImages, masks [][]byte
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		var err error
		body, editImages, masks, err = parseChatGPTImageMultipart(w, r, bodyLimit)
		if err != nil {
			fail := newStreamFailWithCode(streamKindError, ErrorCodeInvalidRequest, "invalid_request: "+err.Error(), err, false)
			h.writeChatGPTImageAPIError(w, round, r, start, "", "", false, http.StatusBadRequest, APIError{Code: ErrorCodeInvalidRequest, Message: err.Error()}, fail, tokenUsage{})
			return
		}
	} else if strings.HasPrefix(contentType, "application/json") {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, bodyLimit)).Decode(&body); err != nil {
			fail := newStreamFailWithCode(streamKindError, ErrorCodeInvalidRequest, "invalid_request: invalid JSON image request", err, false)
			h.writeChatGPTImageAPIError(w, round, r, start, "", "", false, http.StatusBadRequest, APIError{Code: ErrorCodeInvalidRequest, Message: "invalid JSON image request"}, fail, tokenUsage{})
			return
		}
	} else {
		fail := newStreamFailWithCode(streamKindError, ErrorCodeInvalidRequest, "invalid_request: image request must be JSON or multipart/form-data", nil, false)
		h.writeChatGPTImageAPIError(w, round, r, start, "", "", false, http.StatusBadRequest, APIError{Code: ErrorCodeInvalidRequest, Message: "image request must be JSON or multipart/form-data"}, fail, tokenUsage{})
		return
	}
	if body.Model == "" {
		body.Model = "gpt-image-2"
	}
	plan, apiErr := ResolveTransportPlan(cfg, h.EffectiveCatalog(), r.Method, r.URL.Path, body.Model)
	if apiErr != nil {
		// Route errors still need explicit Complete so completePendingUsage is not the success path.
		h.writeArchivedAPIError(w, round, r, start, "", body.Model, false, statusForAPIError(apiErr), *apiErr)
		return
	}
	if plan.UpstreamProtocol != "chatgptweb" {
		fail := newStreamFailWithCode(streamKindError, ErrorCodeEndpointUnsupported, "endpoint_unsupported: image route owner is not ChatGPT Web", nil, false)
		h.writeChatGPTImageAPIError(w, round, r, start, plan.RouteOwner, body.Model, false, http.StatusBadGateway, APIError{Code: ErrorCodeEndpointUnsupported, Message: "image route owner is not ChatGPT Web", Model: body.Model}, fail, tokenUsage{})
		return
	}
	if round != nil {
		h.archiveAndLogTransportPlan(round, r, plan, effectivecatalog.BuiltinProviderView(), false)
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
		} else {
			fail := newStreamFailWithCode(streamKindError, ErrorCodeInvalidRequest, "invalid_request: "+err.Error(), err, false)
			h.writeChatGPTImageAPIError(w, round, r, start, plan.RouteOwner, body.Model, false, http.StatusBadRequest, APIError{Code: ErrorCodeInvalidRequest, Message: err.Error(), Model: body.Model}, fail, tokenUsage{})
			return
		}
	} else {
		result, err = executor.GenerateImage(r.Context(), request)
	}
	tok := tokenUsageFromImageResult(result)
	if err != nil {
		fail := streamFailFromChatGPTImageErr(err)
		status := statusForChatGPTFailure(fail)
		// Partial n>1 failures still carry accumulated Usage on result.
		h.writeChatGPTImageAPIError(w, round, r, start, plan.RouteOwner, body.Model, false, status, APIError{Code: ErrorCodeUpstreamUnavailable, Message: "chatgpt image request failed", Model: body.Model}, fail, tok)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// Encode only the public JSON fields; Usage is json:"-".
	if encErr := json.NewEncoder(w).Encode(result); encErr != nil {
		h.settleChatGPTWeb(round, r, plan.RouteOwner, body.Model, false, http.StatusOK, time.Since(start), tok, newStreamFailWithCode(streamKindClientWrite, chatgptfail.ErrorCode(chatgptfail.KindClientWrite), "client write failed", encErr, false))
		return
	}
	if round != nil {
		if payload, mErr := json.Marshal(result); mErr == nil {
			_ = round.WriteResponse("response.json", append(payload, '\n'))
		}
	}
	h.settleChatGPTWeb(round, r, plan.RouteOwner, body.Model, false, http.StatusOK, time.Since(start), tok, nil)
}

func tokenUsageFromImageResult(result chatgptimage.Result) tokenUsage {
	if result.Usage == nil {
		return tokenUsage{Known: false, Estimated: false}
	}
	u := result.Usage
	prompt := u.PromptTokens
	if prompt == 0 {
		prompt = u.InputTokens
	}
	completion := u.CompletionTokens
	if completion == 0 {
		completion = u.OutputTokens
	}
	total := u.TotalTokens
	if total == 0 {
		total = prompt + completion
	}
	return tokenUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
		Estimated:        false,
		Known:            true,
	}
}

func streamFailFromChatGPTImageErr(err error) *streamFail {
	if err == nil {
		return nil
	}
	if f, ok := chatgptimage.AsFailure(err); ok && f != nil {
		return streamFailFromKind(f.Kind, f.Error(), f)
	}
	return streamFailFromKind(chatgptfail.KindUpstream, err.Error(), err)
}

func (h *Handler) writeChatGPTImageAPIError(w http.ResponseWriter, round *archive.Round, r *http.Request, start time.Time, provider, model string, stream bool, status int, apiErr APIError, fail *streamFail, tok tokenUsage) {
	// Reuse the text-path API error writer + settlement for identical Complete semantics.
	h.writeChatGPTWebAPIError(w, round, r, start, provider, model, stream, status, apiErr, fail, tok)
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
