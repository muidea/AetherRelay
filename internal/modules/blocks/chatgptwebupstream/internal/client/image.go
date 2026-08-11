package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"aetherrelay/internal/pkg/chatgptimageoutput"
	http "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

const (
	maxImageSSELineBytes = 1 << 20
	maxImageSSEBytes     = 32 << 20
	maxImageReferences   = 16
)

// ImageReference is an already-uploaded image available to a conversation.
// Uploading is intentionally a separate client operation: it has its own
// three-step protocol and is not yet wired into the production image command.
type ImageReference struct {
	FileID   string
	FileName string
	FileSize int64
	MIMEType string
	Width    int
	Height   int
}

// ImageRequest describes one image generation or edit conversation. Its
// model is an OpenAI-facing image model name, not necessarily the Web slug.
type ImageRequest struct {
	Prompt     string
	Model      string
	Size       string
	Quality    string
	References []ImageReference
}

// ImageStreamResult contains the bounded, stable values discovered in a Web
// conversation SSE stream. Dynamic upstream payloads stay inside this client.
type ImageStreamResult struct {
	ConversationID string
	FileIDs        []string
	SedimentIDs    []string
	URLs           []string
	RevisedPrompt  string
	Done           bool
}

// ImageGenerationResult is the complete, bounded output of an image Web
// conversation. It intentionally contains bytes and metadata only, never an
// HTTP response, session, or stream handle.
type ImageGenerationResult struct {
	Images         []DownloadedImage
	ConversationID string
	RevisedPrompt  string
}

// GenerateImage runs the authenticated image conversation lifecycle after any
// edit references have been uploaded. The caller owns persistence of returned
// bytes and can choose a tighter polling budget when appropriate.
func (c *Client) GenerateImage(ctx context.Context, request ImageRequest, options ImagePollOptions) (ImageGenerationResult, error) {
	requirements, err := c.ChatRequirements()
	if err != nil {
		return ImageGenerationResult{}, err
	}
	conduitToken, err := c.PrepareImageConversation(request, requirements)
	if err != nil {
		return ImageGenerationResult{}, err
	}
	stream, err := c.StartImageConversation(request, requirements, conduitToken)
	if err != nil {
		return ImageGenerationResult{}, err
	}
	if stream.ConversationID == "" {
		return ImageGenerationResult{}, fmt.Errorf("image conversation: conversation ID not found in SSE")
	}
	resolved, err := c.PollImageResults(ctx, stream.ConversationID, stream, options)
	if err != nil {
		return ImageGenerationResult{ConversationID: stream.ConversationID, RevisedPrompt: stream.RevisedPrompt}, err
	}
	images, err := c.DownloadImageResults(resolved)
	if err != nil {
		return ImageGenerationResult{ConversationID: resolved.ConversationID, RevisedPrompt: resolved.RevisedPrompt}, err
	}
	images, err = normalizeImageOutputs(images, request.Size)
	if err != nil {
		// Preserve the conversation ID: an operator can resume polling the
		// already-created upstream task instead of submitting a duplicate.
		return ImageGenerationResult{ConversationID: resolved.ConversationID, RevisedPrompt: resolved.RevisedPrompt}, err
	}
	return ImageGenerationResult{Images: images, ConversationID: resolved.ConversationID, RevisedPrompt: resolved.RevisedPrompt}, nil
}

func imagePrompt(prompt, size, quality string) string {
	prompt = strings.TrimSpace(prompt)
	hints := make([]string, 0, 2)
	if size = strings.TrimSpace(size); size != "" {
		hints = append(hints, "输出图片尺寸为 "+size+"。")
	}
	if quality = strings.TrimSpace(quality); quality != "" {
		hints = append(hints, "输出图片质量为 "+quality+"。")
	}
	if len(hints) == 0 {
		return prompt
	}
	return prompt + "\n\n" + strings.Join(hints, "")
}

// ResumeImage continues polling an existing conversation without replaying
// image generation. Its caller must supply the same account token that owns
// the conversation.
func (c *Client) ResumeImage(ctx context.Context, conversationID string, extraTimeout time.Duration, requestedSize ...string) (ImageGenerationResult, error) {
	if strings.TrimSpace(conversationID) == "" {
		return ImageGenerationResult{}, fmt.Errorf("image resume: conversation ID is required")
	}
	if extraTimeout <= 0 {
		extraTimeout = 30 * time.Second
	}
	resolved, err := c.PollImageResults(ctx, conversationID, ImageStreamResult{ConversationID: conversationID}, ImagePollOptions{
		Timeout:  extraTimeout,
		Interval: 5 * time.Second,
	})
	if err != nil {
		return ImageGenerationResult{ConversationID: conversationID}, err
	}
	images, err := c.DownloadImageResults(resolved)
	if err != nil {
		return ImageGenerationResult{ConversationID: conversationID, RevisedPrompt: resolved.RevisedPrompt}, err
	}
	size := ""
	if len(requestedSize) > 0 {
		size = requestedSize[0]
	}
	images, err = normalizeImageOutputs(images, size)
	if err != nil {
		return ImageGenerationResult{ConversationID: conversationID, RevisedPrompt: resolved.RevisedPrompt}, err
	}
	return ImageGenerationResult{Images: images, ConversationID: resolved.ConversationID, RevisedPrompt: resolved.RevisedPrompt}, nil
}

// UploadImageReferences turns edit bytes into uploaded file-service pointers.
func (c *Client) UploadImageReferences(images [][]byte) ([]ImageReference, error) {
	if len(images) == 0 {
		return nil, fmt.Errorf("at least one edit image is required")
	}
	references := make([]ImageReference, 0, len(images))
	for index, image := range images {
		reference, err := c.UploadImage(image, fmt.Sprintf("image_%d.png", index+1))
		if err != nil {
			return nil, fmt.Errorf("upload image %d: %w", index+1, err)
		}
		references = append(references, reference)
	}
	return references, nil
}

// PrepareImageConversation obtains the short-lived conduit token required by
// the subsequent image conversation request.
func (c *Client) PrepareImageConversation(request ImageRequest, requirements Requirements) (string, error) {
	// ChatGPT Web has no dedicated size/quality fields in this conversation
	// protocol. Preserve the OpenAI request intent in both protocol messages.
	request.Prompt = imagePrompt(request.Prompt, request.Size, request.Quality)
	if err := validateImageRequest(request, requirements, false); err != nil {
		return "", err
	}
	payload := struct {
		Action                string `json:"action"`
		ForkFromSharedPost    bool   `json:"fork_from_shared_post"`
		ParentMessageID       string `json:"parent_message_id"`
		Model                 string `json:"model"`
		ClientPrepareState    string `json:"client_prepare_state"`
		TimezoneOffsetMinutes int    `json:"timezone_offset_min"`
		Timezone              string `json:"timezone"`
		ConversationMode      struct {
			Kind string `json:"kind"`
		} `json:"conversation_mode"`
		SystemHints  []string `json:"system_hints"`
		PartialQuery struct {
			ID     string `json:"id"`
			Author struct {
				Role string `json:"role"`
			} `json:"author"`
			Content struct {
				ContentType string   `json:"content_type"`
				Parts       []string `json:"parts"`
			} `json:"content"`
		} `json:"partial_query"`
		SupportsBuffering    bool     `json:"supports_buffering"`
		SupportedEncodings   []string `json:"supported_encodings"`
		ClientContextualInfo struct {
			AppName string `json:"app_name"`
		} `json:"client_contextual_info"`
	}{
		Action:                "next",
		ForkFromSharedPost:    false,
		ParentMessageID:       uuid.NewString(),
		Model:                 imageModelSlug(request.Model),
		ClientPrepareState:    "success",
		TimezoneOffsetMinutes: -480,
		Timezone:              "Asia/Shanghai",
		SystemHints:           []string{"picture_v2"},
		SupportsBuffering:     true,
		SupportedEncodings:    []string{"v1"},
	}
	payload.ConversationMode.Kind = "primary_assistant"
	payload.PartialQuery.ID = uuid.NewString()
	payload.PartialQuery.Author.Role = "user"
	payload.PartialQuery.Content.ContentType = "text"
	payload.PartialQuery.Content.Parts = []string{request.Prompt}
	payload.ClientContextualInfo.AppName = "chatgpt.com"

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode image conversation prepare: %w", err)
	}
	response, err := c.imageRequest(http.MethodPost, "/backend-api/f/conversation/prepare", body, requirements, "", "*/*", "image_prepare")
	if err != nil {
		return "", err
	}
	var parsed struct {
		ConduitToken string `json:"conduit_token"`
	}
	if err := json.Unmarshal(response, &parsed); err != nil {
		return "", fmt.Errorf("decode image conversation prepare: %w", err)
	}
	if parsed.ConduitToken == "" {
		return "", fmt.Errorf("image conversation prepare: missing conduit_token")
	}
	return parsed.ConduitToken, nil
}

// StartImageConversation executes the SSE request and returns only values
// needed by later polling/downloading. It does not expose a response body or
// other mutable transport resource outside the upstream owner.
func (c *Client) StartImageConversation(request ImageRequest, requirements Requirements, conduitToken string) (ImageStreamResult, error) {
	request.Prompt = imagePrompt(request.Prompt, request.Size, request.Quality)
	if err := validateImageRequest(request, requirements, true); err != nil {
		return ImageStreamResult{}, err
	}
	if strings.TrimSpace(conduitToken) == "" {
		return ImageStreamResult{}, fmt.Errorf("image conversation: conduit token is required")
	}
	payload := imageConversationPayload(request)
	body, err := json.Marshal(payload)
	if err != nil {
		return ImageStreamResult{}, fmt.Errorf("encode image conversation: %w", err)
	}
	return c.imageSSERequest("/backend-api/f/conversation", body, requirements, conduitToken)
}

func (c *Client) imageSSERequest(path string, body []byte, requirements Requirements, conduitToken string) (ImageStreamResult, error) {
	req, err := c.newImageRequest(http.MethodPost, path, body, requirements, conduitToken, "text/event-stream")
	if err != nil {
		return ImageStreamResult{}, err
	}
	response, err := c.doer.Do(req)
	if err != nil {
		return ImageStreamResult{}, classifyTransport("image_conversation", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ImageStreamResult{}, classifyStatus("image_conversation", response.StatusCode)
	}
	return ParseImageSSE(io.LimitReader(response.Body, maxImageSSEBytes))
}

func (c *Client) imageRequest(method, path string, body []byte, requirements Requirements, conduitToken, accept, operation string) ([]byte, error) {
	req, err := c.newImageRequest(method, path, body, requirements, conduitToken, accept)
	if err != nil {
		return nil, err
	}
	response, err := c.doer.Do(req)
	if err != nil {
		return nil, classifyTransport(operation, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, classifyTransport(operation, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyStatus(operation, response.StatusCode)
	}
	return responseBody, nil
}

func (c *Client) newImageRequest(method, path string, body []byte, requirements Requirements, conduitToken, accept string) (*http.Request, error) {
	req, err := http.NewRequest(method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("image request: %w", err)
	}
	req.Header.Set("accept", accept)
	req.Header.Set("authorization", "Bearer "+c.accessToken)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("openai-sentinel-chat-requirements-token", requirements.Token)
	if requirements.ProofToken != "" {
		req.Header.Set("openai-sentinel-proof-token", requirements.ProofToken)
	}
	req.Header.Set("origin", c.baseURL)
	req.Header.Set("referer", c.baseURL+"/")
	req.Header.Set("user-agent", c.userAgent)
	req.Header.Set("accept-language", "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7")
	req.Header.Set("cache-control", "no-cache")
	req.Header.Set("pragma", "no-cache")
	req.Header.Set("priority", "u=1, i")
	req.Header.Set("sec-ch-ua", `"Google Chrome";v="133", "Chromium";v="133", "Not_A Brand";v="24"`)
	req.Header.Set("sec-ch-ua-arch", `"x86"`)
	req.Header.Set("sec-ch-ua-bitness", `"64"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("oai-device-id", c.deviceID)
	req.Header.Set("oai-session-id", c.sessionID)
	req.Header.Set("oai-language", "zh-CN")
	req.Header.Set("x-openai-target-path", path)
	req.Header.Set("x-openai-target-route", path)
	if conduitToken != "" {
		req.Header.Set("x-conduit-token", conduitToken)
	}
	if accept == "text/event-stream" {
		req.Header.Set("x-oai-turn-trace-id", uuid.NewString())
	}
	if c.cookie != "" {
		req.Header.Set("cookie", c.cookie)
	}
	return req, nil
}

func validateImageRequest(request ImageRequest, requirements Requirements, requireReferences bool) error {
	if strings.TrimSpace(request.Prompt) == "" {
		return fmt.Errorf("image prompt is required")
	}
	if err := chatgptimageoutput.ValidateRequest(request.Prompt, request.Size, ""); err != nil {
		return err
	}
	if strings.TrimSpace(requirements.Token) == "" {
		return fmt.Errorf("image requirements token is required")
	}
	if len(request.References) > maxImageReferences {
		return fmt.Errorf("too many image references: %d", len(request.References))
	}
	if requireReferences {
		for index, reference := range request.References {
			if strings.TrimSpace(reference.FileID) == "" || reference.Width <= 0 || reference.Height <= 0 || reference.FileSize <= 0 {
				return fmt.Errorf("invalid image reference %d", index+1)
			}
		}
	}
	return nil
}

func normalizeImageOutputs(images []DownloadedImage, requestedSize string) ([]DownloadedImage, error) {
	_, requested, err := chatgptimageoutput.ParseSize(requestedSize)
	if err != nil {
		return nil, err
	}
	if len(images) == 0 {
		return images, nil
	}
	result := make([]DownloadedImage, 0, len(images))
	for index, item := range images {
		if len(item.Bytes) == 0 {
			if requested {
				return nil, fmt.Errorf("normalize image %d: cannot enforce requested size because image bytes are unavailable", index+1)
			}
			result = append(result, item)
			continue
		}
		payload, _, err := chatgptimageoutput.Normalize(item.Bytes, requestedSize)
		if err != nil {
			return nil, fmt.Errorf("normalize image %d: %w", index+1, err)
		}
		result = append(result, DownloadedImage{Bytes: payload, URL: item.URL})
	}
	return result, nil
}

func imageModelSlug(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "gpt-image-2" {
		return "gpt-5-3"
	}
	if normalized == "codex-gpt-image-2" || normalized == "plus-codex-gpt-image-2" || normalized == "team-codex-gpt-image-2" || normalized == "pro-codex-gpt-image-2" {
		return "codex-gpt-image-2"
	}
	return "auto"
}

func imageConversationPayload(request ImageRequest) any {
	type imagePart struct {
		ContentType  string `json:"content_type"`
		AssetPointer string `json:"asset_pointer"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		SizeBytes    int64  `json:"size_bytes"`
	}
	type attachment struct {
		ID       string `json:"id"`
		MIMEType string `json:"mimeType"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
	}
	parts := make([]any, 0, len(request.References)+1)
	attachments := make([]attachment, 0, len(request.References))
	for _, reference := range request.References {
		parts = append(parts, imagePart{ContentType: "image_asset_pointer", AssetPointer: "file-service://" + reference.FileID, Width: reference.Width, Height: reference.Height, SizeBytes: reference.FileSize})
		attachments = append(attachments, attachment{ID: reference.FileID, MIMEType: reference.MIMEType, Name: reference.FileName, Size: reference.FileSize, Width: reference.Width, Height: reference.Height})
	}
	parts = append(parts, request.Prompt)
	content := any(struct {
		ContentType string `json:"content_type"`
		Parts       []any  `json:"parts"`
	}{ContentType: "multimodal_text", Parts: parts})
	if len(request.References) == 0 {
		content = struct {
			ContentType string   `json:"content_type"`
			Parts       []string `json:"parts"`
		}{ContentType: "text", Parts: []string{request.Prompt}}
	}
	metadata := struct {
		DeveloperModeConnectorIDs []string `json:"developer_mode_connector_ids"`
		SelectedGitHubRepos       []string `json:"selected_github_repos"`
		SelectedAllGitHubRepos    bool     `json:"selected_all_github_repos"`
		SystemHints               []string `json:"system_hints"`
		SerializationMetadata     struct {
			CustomSymbolOffsets []int `json:"custom_symbol_offsets"`
		} `json:"serialization_metadata"`
		Attachments []attachment `json:"attachments,omitempty"`
	}{
		DeveloperModeConnectorIDs: []string{},
		SelectedGitHubRepos:       []string{},
		SelectedAllGitHubRepos:    false,
		SystemHints:               []string{"picture_v2"},
		Attachments:               attachments,
	}
	metadata.SerializationMetadata.CustomSymbolOffsets = []int{}
	type imageMessage struct {
		ID     string `json:"id"`
		Author struct {
			Role string `json:"role"`
		} `json:"author"`
		CreateTime float64 `json:"create_time"`
		Content    any     `json:"content"`
		Metadata   any     `json:"metadata"`
	}
	type conversationPayload struct {
		Action             string         `json:"action"`
		Messages           []imageMessage `json:"messages"`
		ParentMessageID    string         `json:"parent_message_id"`
		Model              string         `json:"model"`
		ClientPrepareState string         `json:"client_prepare_state"`
		TimezoneOffsetMins int            `json:"timezone_offset_min"`
		Timezone           string         `json:"timezone"`
		ConversationMode   struct {
			Kind string `json:"kind"`
		} `json:"conversation_mode"`
		EnableFollowups    bool     `json:"enable_message_followups"`
		SystemHints        []string `json:"system_hints"`
		SupportsBuffering  bool     `json:"supports_buffering"`
		SupportedEncodings []string `json:"supported_encodings"`
		ClientContextual   struct {
			IsDarkMode      bool    `json:"is_dark_mode"`
			TimeSinceLoaded int     `json:"time_since_loaded"`
			PageHeight      int     `json:"page_height"`
			PageWidth       int     `json:"page_width"`
			PixelRatio      float64 `json:"pixel_ratio"`
			ScreenHeight    int     `json:"screen_height"`
			ScreenWidth     int     `json:"screen_width"`
			AppName         string  `json:"app_name"`
		} `json:"client_contextual_info"`
		ParagenOverride     string `json:"paragen_cot_summary_display_override"`
		ForceParallelSwitch string `json:"force_parallel_switch"`
	}
	payload := conversationPayload{
		Action:              "next",
		ParentMessageID:     uuid.NewString(),
		Model:               imageModelSlug(request.Model),
		ClientPrepareState:  "sent",
		TimezoneOffsetMins:  -480,
		Timezone:            "Asia/Shanghai",
		EnableFollowups:     true,
		SystemHints:         []string{"picture_v2"},
		SupportsBuffering:   true,
		SupportedEncodings:  []string{"v1"},
		ParagenOverride:     "allow",
		ForceParallelSwitch: "auto",
	}
	payload.ConversationMode.Kind = "primary_assistant"
	payload.ClientContextual = struct {
		IsDarkMode      bool    `json:"is_dark_mode"`
		TimeSinceLoaded int     `json:"time_since_loaded"`
		PageHeight      int     `json:"page_height"`
		PageWidth       int     `json:"page_width"`
		PixelRatio      float64 `json:"pixel_ratio"`
		ScreenHeight    int     `json:"screen_height"`
		ScreenWidth     int     `json:"screen_width"`
		AppName         string  `json:"app_name"`
	}{
		IsDarkMode:      false,
		TimeSinceLoaded: 1200,
		PageHeight:      1072,
		PageWidth:       1724,
		PixelRatio:      1.2,
		ScreenHeight:    1440,
		ScreenWidth:     2560,
		AppName:         "chatgpt.com",
	}
	message := imageMessage{ID: uuid.NewString(), CreateTime: float64(time.Now().UnixNano()) / float64(time.Second), Content: content, Metadata: metadata}
	message.Author.Role = "user"
	payload.Messages = []imageMessage{message}
	return payload
}

// ParseImageSSE consumes a standard SSE stream. It accepts multi-line data
// frames and ignores non-JSON server keepalives while retaining useful output
// from known and future ChatGPT payload shapes.
func ParseImageSSE(reader io.Reader) (ImageStreamResult, error) {
	result := ImageStreamResult{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxImageSSELineBytes)
	dataLines := make([]string, 0, 2)
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if payload == "[DONE]" {
			result.Done = true
			return nil
		}
		var decoded any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			return nil // upstream can emit non-JSON progress/keepalive frames.
		}
		collectImageStreamValues(decoded, &result)
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return ImageStreamResult{}, err
			}
			if result.Done {
				return result, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			// SSE permits one optional space after the field delimiter.
			dataLines = append(dataLines, strings.TrimPrefix(data, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return ImageStreamResult{}, fmt.Errorf("read image SSE: %w", err)
	}
	if err := flush(); err != nil {
		return ImageStreamResult{}, err
	}
	return result, nil
}

func collectImageStreamValues(value any, result *ImageStreamResult) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(key)
			if text, ok := child.(string); ok {
				switch normalized {
				case "conversation_id", "conversationid":
					if result.ConversationID == "" {
						result.ConversationID = bounded(text, 512)
					}
				case "file_id", "fileid":
					addImageValue(&result.FileIDs, text)
				case "sediment_id", "sedimentid", "attachment_id", "attachmentid":
					addImageValue(&result.SedimentIDs, text)
				case "asset_pointer":
					collectImagePointer(text, result)
				case "url", "download_url", "downloadurl":
					if parsed, err := url.Parse(text); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" {
						addImageValue(&result.URLs, text)
					}
				case "revised_prompt", "revisedprompt":
					if result.RevisedPrompt == "" {
						result.RevisedPrompt = bounded(text, 16<<10)
					}
				}
				collectImagePointer(text, result)
			}
			collectImageStreamValues(child, result)
		}
	case []any:
		for _, child := range typed {
			collectImageStreamValues(child, result)
		}
	case string:
		collectImagePointer(typed, result)
	}
}

func collectImagePointer(value string, result *ImageStreamResult) {
	const filePrefix = "file-service://"
	const sedimentPrefix = "sediment://"
	if start := strings.Index(value, filePrefix); start >= 0 {
		addImageValue(&result.FileIDs, pointerID(value[start+len(filePrefix):]))
	}
	if start := strings.Index(value, sedimentPrefix); start >= 0 {
		addImageValue(&result.SedimentIDs, pointerID(value[start+len(sedimentPrefix):]))
	}
}

func pointerID(value string) string {
	return strings.TrimRightFunc(value, func(r rune) bool {
		return !(r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})
}

func addImageValue(target *[]string, value string) {
	value = bounded(strings.TrimSpace(value), 512)
	if value == "" || len(*target) >= maxImageReferences {
		return
	}
	for _, existing := range *target {
		if existing == value {
			return
		}
	}
	*target = append(*target, value)
}

func bounded(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
