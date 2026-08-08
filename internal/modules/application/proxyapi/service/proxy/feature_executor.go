package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"

	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptsearch"
	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	"ai-proxy/internal/pkg/chatgpttokenusage"
)

type featureResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

type featureExecutionTrace struct {
	provider       string
	conversationID string
	accountID      string
	usage          *tokenusage.Usage
}
type featureExecutionTraceKey struct{}
type featureFileAttachmentsKey struct{}

func newFeatureResponse() *featureResponse     { return &featureResponse{header: make(http.Header)} }
func (w *featureResponse) Header() http.Header { return w.header }
func (w *featureResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *featureResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func (h *Handler) FeatureCatalog(ctx context.Context) proxyevents.FeatureCatalogResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := internalFeatureIdentity(ctx); !ok {
		ctx = withInternalFeatureIdentity(ctx, "feature-catalog")
	}
	snap := h.EffectiveCatalog()
	ids := make([]string, 0, len(snap.Candidates))
	for id := range snap.Candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := proxyevents.FeatureCatalogResult{}
	for _, id := range ids {
		if providers := h.featureProvidersFor(ctx, id, []string{"/v1/chat/completions", "/v1/responses"}); len(providers) > 0 {
			result.TextModels = append(result.TextModels, proxyevents.FeatureModel{ID: id, Providers: providers})
		}
		if providers := h.featureProvidersFor(ctx, id, []string{"/v1/search"}); len(providers) > 0 {
			result.SearchModels = append(result.SearchModels, proxyevents.FeatureModel{ID: id, Providers: providers})
		}
		if providers := h.featureProvidersFor(ctx, id, []string{"/v1/images/generations"}); len(providers) > 0 {
			result.ImageModels = append(result.ImageModels, proxyevents.FeatureModel{ID: id, Providers: providers})
		}
		if providers := h.featureProvidersFor(ctx, id, []string{"/v1/images/edits"}); len(providers) > 0 {
			result.ImageEditModels = append(result.ImageEditModels, proxyevents.FeatureModel{ID: id, Providers: providers})
		}
	}
	return result
}

func (h *Handler) featureProvidersFor(ctx context.Context, model string, paths []string) []proxyevents.FeatureProvider {
	positions := map[string]int{}
	providers := []proxyevents.FeatureProvider{}
	for _, path := range paths {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, path, nil)
		plans, apiErr := h.resolveTransportPlans(req, model)
		if apiErr != nil {
			continue
		}
		for _, plan := range plans {
			supportsFiles := path == "/v1/responses" && transportSupportsFileAttachments(plan)
			if index, ok := positions[plan.RouteOwner]; ok {
				providers[index].SupportsFiles = providers[index].SupportsFiles || supportsFiles
				continue
			}
			positions[plan.RouteOwner] = len(providers)
			providers = append(providers, proxyevents.FeatureProvider{Name: plan.RouteOwner, Protocol: plan.UpstreamProtocol, Priority: plan.Priority, SupportsFiles: supportsFiles})
		}
	}
	sort.SliceStable(providers, func(i, j int) bool {
		if providers[i].Priority != providers[j].Priority {
			return providers[i].Priority > providers[j].Priority
		}
		return providers[i].Name < providers[j].Name
	})
	return providers
}

func transportSupportsFileAttachments(plan TransportPlan) bool {
	// Codex OAuth and protocol-converted providers are excluded until their
	// upstream contracts are verified for Responses input_file payloads.
	switch strings.ToLower(strings.TrimSpace(plan.UpstreamProtocol)) {
	case "openai", "chatgptweb":
		return true
	default:
		return false
	}
}

func (h *Handler) ExecuteFeatureText(ctx context.Context, command proxyevents.ExecuteFeatureTextCommand) (proxyevents.ExecuteFeatureTextResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := internalFeatureIdentity(ctx); !ok {
		ctx = withInternalFeatureIdentity(ctx, "feature-text")
	}
	model := strings.TrimSpace(command.Model)
	if model == "" || len(command.Messages) == 0 {
		return proxyevents.ExecuteFeatureTextResult{}, fmt.Errorf("feature text model and messages are required")
	}
	hasFiles := false
	for _, message := range command.Messages {
		if len(message.Files) > 0 {
			hasFiles = true
			break
		}
	}
	if command.WebSearch {
		var queryMessage *proxyevents.FeatureTextMessage
		for index := len(command.Messages) - 1; index >= 0; index-- {
			if strings.EqualFold(strings.TrimSpace(command.Messages[index].Role), "user") {
				queryMessage = &command.Messages[index]
				break
			}
		}
		if queryMessage == nil || strings.TrimSpace(queryMessage.Content) == "" {
			return proxyevents.ExecuteFeatureTextResult{}, fmt.Errorf("web search requires a non-empty user message")
		}
		if len(queryMessage.Images) > 0 || len(queryMessage.Files) > 0 {
			return proxyevents.ExecuteFeatureTextResult{}, fmt.Errorf("web search does not support attachments on the query message")
		}
		query := strings.TrimSpace(queryMessage.Content)
		search, err := h.ExecuteFeatureSearch(ctx, proxyevents.ExecuteFeatureSearchCommand{OwnerID: command.OwnerID, Model: model, Query: query})
		if err != nil {
			return proxyevents.ExecuteFeatureTextResult{Provider: search.Provider, ActualModel: search.ActualModel}, err
		}
		sources := make([]chatgptsearch.Source, 0, len(search.Sources))
		for _, source := range search.Sources {
			sources = append(sources, chatgptsearch.Source{Title: source.Title, URL: source.URL, Snippet: source.Snippet})
		}
		text, _ := presentChatGPTWebSearch(chatgptsearch.Result{Text: search.Text, Sources: sources})
		return proxyevents.ExecuteFeatureTextResult{Provider: search.Provider, ActualModel: search.ActualModel, Text: text}, nil
	}
	endpoint := "/v1/chat/completions"
	if hasFiles {
		endpoint = "/v1/responses"
		ctx = context.WithValue(ctx, featureFileAttachmentsKey{}, true)
	}
	probe, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	plans, apiErr := h.resolveTransportPlans(probe, model)
	if !hasFiles && (apiErr != nil || len(plans) == 0) {
		endpoint = "/v1/responses"
		probe, _ = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
		plans, apiErr = h.resolveTransportPlans(probe, model)
	}
	if apiErr != nil || len(plans) == 0 {
		return proxyevents.ExecuteFeatureTextResult{}, fmt.Errorf("no compatible provider can serve model %q", model)
	}

	messages := make([]any, 0, len(command.Messages))
	for _, message := range command.Messages {
		content := any(message.Content)
		if len(message.Images) > 0 || len(message.Files) > 0 {
			parts := []any{}
			if message.Content != "" {
				partType := "text"
				if endpoint == "/v1/responses" {
					partType = "input_text"
				}
				parts = append(parts, map[string]any{"type": partType, "text": message.Content})
			}
			for _, image := range message.Images {
				mime := http.DetectContentType(image)
				dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image)
				if endpoint == "/v1/responses" {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": dataURL})
				} else {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}})
				}
			}
			for _, file := range message.Files {
				if endpoint != "/v1/responses" {
					return proxyevents.ExecuteFeatureTextResult{}, fmt.Errorf("file attachments require a Responses-compatible provider")
				}
				dataURL := "data:" + file.ContentType + ";base64," + base64.StdEncoding.EncodeToString(file.Bytes)
				parts = append(parts, map[string]any{"type": "input_file", "filename": file.Name, "file_data": dataURL})
			}
			content = parts
		}
		messages = append(messages, map[string]any{"role": message.Role, "content": content})
	}
	body := map[string]any{"model": model, "stream": false}
	if command.WebSearch {
		body["tools"] = []any{map[string]any{"type": "web_search"}}
	}
	if endpoint == "/v1/responses" {
		body["input"] = messages
	} else {
		body["messages"] = messages
	}
	if effort := strings.TrimSpace(command.ThinkingEffort); effort != "" {
		if endpoint == "/v1/responses" {
			body["reasoning"] = map[string]any{"effort": effort}
		} else {
			body["reasoning_effort"] = effort
		}
	}
	payload, _ := json.Marshal(body)
	response, trace, err := h.executeFeatureRequest(ctx, command.OwnerID, endpoint, "application/json", payload)
	if err != nil {
		return proxyevents.ExecuteFeatureTextResult{}, err
	}
	text, actualModel, err := extractFeatureText(response, endpoint)
	if err != nil {
		return proxyevents.ExecuteFeatureTextResult{}, err
	}
	return proxyevents.ExecuteFeatureTextResult{Provider: firstNonEmpty(trace.provider, plans[0].RouteOwner), ActualModel: firstNonEmpty(actualModel, model), Text: text}, nil
}

// ExecuteFeatureSearch runs the same isolated forced-search flow exposed by
// /v1/search. Keeping the Admin page on this endpoint guarantees that a
// same-model third-party Provider cannot receive a search request.
func (h *Handler) ExecuteFeatureSearch(ctx context.Context, command proxyevents.ExecuteFeatureSearchCommand) (proxyevents.ExecuteFeatureSearchResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := internalFeatureIdentity(ctx); !ok {
		ctx = withInternalFeatureIdentity(ctx, "feature-search")
	}
	model, query := strings.TrimSpace(command.Model), strings.TrimSpace(command.Query)
	if model == "" || query == "" {
		return proxyevents.ExecuteFeatureSearchResult{}, fmt.Errorf("feature search model and query are required")
	}
	probe, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/search", nil)
	plans, apiErr := h.resolveTransportPlans(probe, model)
	if apiErr != nil || len(plans) == 0 {
		return proxyevents.ExecuteFeatureSearchResult{}, fmt.Errorf("no ChatGPT Web search provider can serve model %q", model)
	}
	payload, _ := json.Marshal(webSearchRequest{Model: model, Query: query})
	response, trace, err := h.executeFeatureRequest(ctx, command.OwnerID, "/v1/search", "application/json", payload)
	if err != nil {
		return proxyevents.ExecuteFeatureSearchResult{Provider: trace.provider}, err
	}
	var decoded webSearchResponse
	if err := json.Unmarshal(response, &decoded); err != nil || strings.TrimSpace(decoded.OutputText) == "" {
		return proxyevents.ExecuteFeatureSearchResult{}, fmt.Errorf("invalid web search response")
	}
	out := proxyevents.ExecuteFeatureSearchResult{
		Provider:    firstNonEmpty(trace.provider, plans[0].RouteOwner),
		ActualModel: firstNonEmpty(decoded.Model, model),
		Text:        decoded.OutputText,
		Sources:     make([]proxyevents.FeatureSearchSource, 0, len(decoded.Sources)),
	}
	for _, source := range decoded.Sources {
		out.Sources = append(out.Sources, proxyevents.FeatureSearchSource{Title: source.Title, URL: source.URL, Snippet: source.Snippet})
	}
	return out, nil
}

func (h *Handler) ExecuteFeatureImage(ctx context.Context, command proxyevents.ExecuteFeatureImageCommand) (proxyevents.ExecuteFeatureImageResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := internalFeatureIdentity(ctx); !ok {
		ctx = withInternalFeatureIdentity(ctx, "feature-image")
	}
	model := strings.TrimSpace(command.Model)
	if model == "" {
		return proxyevents.ExecuteFeatureImageResult{}, fmt.Errorf("feature image model is required")
	}
	endpoint := "/v1/images/generations"
	body := map[string]any{"model": model, "prompt": command.Prompt, "n": 1, "response_format": "b64_json"}
	if command.Size != "" {
		body["size"] = command.Size
	}
	if command.Quality != "" {
		body["quality"] = command.Quality
	}
	if len(command.Images) > 0 {
		endpoint = "/v1/images/edits"
		encoded := make([]string, 0, len(command.Images))
		for _, image := range command.Images {
			encoded = append(encoded, base64.StdEncoding.EncodeToString(image))
		}
		body["images"] = encoded
	}
	probe, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	plans, apiErr := h.resolveTransportPlans(probe, model)
	if apiErr != nil || len(plans) == 0 {
		return proxyevents.ExecuteFeatureImageResult{}, fmt.Errorf("no compatible image provider can serve model %q", model)
	}
	contentType := "application/json"
	payload, _ := json.Marshal(body)
	if len(command.Images) > 0 {
		var encoded bytes.Buffer
		writer := multipart.NewWriter(&encoded)
		_ = writer.WriteField("model", model)
		_ = writer.WriteField("prompt", command.Prompt)
		if command.Size != "" {
			_ = writer.WriteField("size", command.Size)
		}
		if command.Quality != "" {
			_ = writer.WriteField("quality", command.Quality)
		}
		for index, image := range command.Images {
			part, createErr := writer.CreateFormFile("image", fmt.Sprintf("image-%d", index+1))
			if createErr != nil {
				return proxyevents.ExecuteFeatureImageResult{}, createErr
			}
			if _, writeErr := part.Write(image); writeErr != nil {
				return proxyevents.ExecuteFeatureImageResult{}, writeErr
			}
		}
		if closeErr := writer.Close(); closeErr != nil {
			return proxyevents.ExecuteFeatureImageResult{}, closeErr
		}
		contentType = writer.FormDataContentType()
		payload = encoded.Bytes()
	}
	response, trace, err := h.executeFeatureRequest(ctx, command.OwnerID, endpoint, contentType, payload)
	if err != nil {
		return proxyevents.ExecuteFeatureImageResult{Provider: trace.provider, ConversationID: trace.conversationID, AccountID: trace.accountID, Usage: trace.usage}, err
	}
	var decoded struct {
		Data []struct {
			URL           string `json:"url"`
			B64JSON       string `json:"b64_json"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil || len(decoded.Data) == 0 {
		return proxyevents.ExecuteFeatureImageResult{}, fmt.Errorf("invalid image provider response")
	}
	out := proxyevents.ExecuteFeatureImageResult{Provider: firstNonEmpty(trace.provider, plans[0].RouteOwner), Model: model, ConversationID: trace.conversationID, AccountID: trace.accountID, Usage: trace.usage}
	for _, item := range decoded.Data {
		out.Data = append(out.Data, proxyevents.FeatureImageData{URL: item.URL, B64JSON: item.B64JSON, RevisedPrompt: item.RevisedPrompt})
	}
	return out, nil
}

func (h *Handler) executeFeatureRequest(ctx context.Context, ownerID, endpoint, contentType string, payload []byte) ([]byte, featureExecutionTrace, error) {
	trace := &featureExecutionTrace{}
	ctx = context.WithValue(withInternalFeatureIdentity(ctx, ownerID), featureExecutionTraceKey{}, trace)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, featureExecutionTrace{}, err
	}
	request.Header.Set("Content-Type", contentType)
	response := newFeatureResponse()
	h.ServeHTTP(response, request)
	if response.status < 200 || response.status >= 300 {
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(response.body.Bytes(), &envelope)
		message := strings.TrimSpace(envelope.Error.Message)
		if message == "" {
			message = fmt.Sprintf("feature provider request failed with HTTP %d", response.status)
		}
		return nil, *trace, fmt.Errorf("%s", message)
	}
	return bytes.Clone(response.body.Bytes()), *trace, nil
}

func extractFeatureText(body []byte, endpoint string) (string, string, error) {
	if endpoint == "/v1/chat/completions" {
		var payload struct {
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || len(payload.Choices) == 0 {
			return "", "", fmt.Errorf("invalid chat completion response")
		}
		return payload.Choices[0].Message.Content, payload.Model, nil
	}
	var payload struct {
		Model      string `json:"model"`
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", fmt.Errorf("invalid responses result")
	}
	var text strings.Builder
	text.WriteString(payload.OutputText)
	if text.Len() == 0 {
		for _, output := range payload.Output {
			for _, content := range output.Content {
				if content.Type == "output_text" || content.Type == "text" || content.Type == "" {
					text.WriteString(content.Text)
				}
			}
		}
	}
	if text.Len() == 0 {
		return "", payload.Model, fmt.Errorf("responses result has no output text")
	}
	return text.String(), payload.Model, nil
}
