package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

const (
	maxTextSSELineBytes = 1 << 20
	maxTextSSEBytes     = 16 << 20
	maxTextMessages     = 64
	maxTextContentBytes = 256 << 10
)

// TextMessage is a text-only Web conversation input. It deliberately has no
// dynamic content field, so callers cannot accidentally pass tool or image
// payloads through the text path.
type TextMessage struct {
	Role    string
	Content string
	Images  [][]byte
}

type TextRequest struct {
	Model           string
	Messages        []TextMessage
	ThinkingEffort  string
	ConversationID  string
	ParentMessageID string
}

// TextResult is the bounded final state collected from an upstream SSE
// conversation. The response body never escapes the upstream owner.
type TextResult struct {
	ConversationID     string
	AssistantMessageID string
	Text               string
	Done               bool
}

// TextDelta is an incremental assistant update derived from the Web SSE
// message snapshots.
type TextDelta struct {
	ConversationID     string
	AssistantMessageID string
	Text               string
}

// CompleteText runs the authenticated Web conversation lifecycle and collects
// the text SSE into one non-streaming result. Its SSE parsing is intentionally
// separate from the HTTP gateway so it can later back a streaming adapter.
func (c *Client) CompleteText(ctx context.Context, request TextRequest) (TextResult, error) {
	return c.StreamText(ctx, request, nil)
}

// StreamText keeps all transport handles inside the upstream client while
// exposing only bounded text deltas to its owner.
func (c *Client) StreamText(ctx context.Context, request TextRequest, emit func(TextDelta) error) (TextResult, error) {
	if err := validateTextRequest(request); err != nil {
		return TextResult{}, err
	}
	requirements, err := c.ChatRequirements()
	if err != nil {
		return TextResult{}, err
	}
	messages, err := c.prepareTextMessages(request.Messages)
	if err != nil {
		return TextResult{}, err
	}
	body, err := json.Marshal(textConversationPayload(request, messages))
	if err != nil {
		return TextResult{}, fmt.Errorf("encode text conversation: %w", err)
	}
	req, err := c.newTextRequest(http.MethodPost, "/backend-api/conversation", body, requirements)
	if err != nil {
		return TextResult{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req = req.WithContext(ctx)
	response, err := c.doer.Do(req)
	if err != nil {
		return TextResult{}, classifyTransport("text_conversation", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return TextResult{}, classifyStatus("text_conversation", response.StatusCode)
	}
	return parseTextSSE(ctx, io.LimitReader(response.Body, maxTextSSEBytes), emit)
}

func (c *Client) newTextRequest(method, path string, body []byte, requirements Requirements) (*http.Request, error) {
	req, err := c.newImageRequest(method, path, body, requirements, "", "text/event-stream")
	if err != nil {
		return nil, err
	}
	if requirements.TurnstileToken != "" {
		req.Header.Set("openai-sentinel-turnstile-token", requirements.TurnstileToken)
	}
	if requirements.SOToken != "" {
		req.Header.Set("openai-sentinel-so-token", requirements.SOToken)
	}
	return req, nil
}

func validateTextRequest(request TextRequest) error {
	if len(request.Messages) == 0 || len(request.Messages) > maxTextMessages {
		return fmt.Errorf("messages must contain between 1 and %d items", maxTextMessages)
	}
	for index, message := range request.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role != "system" && role != "user" && role != "assistant" {
			return fmt.Errorf("message %d has unsupported role", index+1)
		}
		content := strings.TrimSpace(message.Content)
		if content == "" && len(message.Images) == 0 {
			return fmt.Errorf("message %d content is required", index+1)
		}
		if len(content) > maxTextContentBytes {
			return fmt.Errorf("message %d content exceeds %d KiB", index+1, maxTextContentBytes>>10)
		}
	}
	return nil
}

type preparedTextMessage struct {
	Role, Content string
	References    []ImageReference
}

func (c *Client) prepareTextMessages(messages []TextMessage) ([]preparedTextMessage, error) {
	prepared := make([]preparedTextMessage, 0, len(messages))
	for index, message := range messages {
		item := preparedTextMessage{Role: strings.ToLower(strings.TrimSpace(message.Role)), Content: strings.TrimSpace(message.Content)}
		for imageIndex, data := range message.Images {
			reference, err := c.UploadImage(data, fmt.Sprintf("chat_image_%d_%d.png", index+1, imageIndex+1))
			if err != nil {
				return nil, fmt.Errorf("upload chat image %d/%d: %w", index+1, imageIndex+1, err)
			}
			item.References = append(item.References, reference)
		}
		prepared = append(prepared, item)
	}
	return prepared, nil
}

func textConversationPayload(request TextRequest, inputs []preparedTextMessage) textPayload {
	messages := make([]textConversationMessage, 0, len(inputs))
	for _, input := range inputs {
		message := textConversationMessage{ID: uuid.NewString(), CreateTime: float64(time.Now().UnixNano()) / float64(time.Second)}
		message.Author.Role = input.Role
		if len(input.References) == 0 {
			message.Content = textContent{ContentType: "text", Parts: []any{input.Content}}
		} else {
			parts := make([]any, 0, len(input.References)+1)
			attachments := make([]textAttachment, 0, len(input.References))
			for _, ref := range input.References {
				parts = append(parts, textImagePart{ContentType: "image_asset_pointer", AssetPointer: "file-service://" + ref.FileID, Width: ref.Width, Height: ref.Height, SizeBytes: ref.FileSize})
				attachments = append(attachments, textAttachment{ID: ref.FileID, MIMEType: ref.MIMEType, Name: ref.FileName, Size: ref.FileSize, Width: ref.Width, Height: ref.Height})
			}
			if input.Content != "" {
				parts = append(parts, input.Content)
			}
			message.Content = textContent{ContentType: "multimodal_text", Parts: parts}
			message.Metadata = textMetadata{Attachments: attachments}
		}
		messages = append(messages, message)
	}
	parentMessageID := strings.TrimSpace(request.ParentMessageID)
	if parentMessageID == "" {
		parentMessageID = uuid.NewString()
	}
	payload := textPayload{
		Action:                     "next",
		Messages:                   messages,
		Model:                      textModelSlug(request.Model),
		ParentMessageID:            parentMessageID,
		ForceUseSSE:                true,
		HistoryAndTrainingDisabled: true,
		Timezone:                   "Asia/Shanghai",
		TimezoneOffsetMins:         -480,
		VariantPurpose:             "comparison_implicit",
		WebsocketRequestID:         uuid.NewString(),
		Suggestions:                []string{},
		SupportedEncodings:         []string{},
		SystemHints:                []string{},
	}
	if conversationID := strings.TrimSpace(request.ConversationID); conversationID != "" {
		payload.ConversationID = conversationID
	}
	payload.ConversationMode.Kind = "primary_assistant"
	payload.ClientContextualInfo = textClientContext{PageHeight: 900, PageWidth: 1400, PixelRatio: 2, ScreenHeight: 1440, ScreenWidth: 2560, TimeSinceLoaded: 120}
	if effort := normalizeThinkingEffort(request.ThinkingEffort); effort != "" {
		payload.ThinkingEffort = effort
	}
	return payload
}

type textConversationMessage struct {
	ID     string `json:"id"`
	Author struct {
		Role string `json:"role"`
	} `json:"author"`
	CreateTime float64      `json:"create_time"`
	Content    textContent  `json:"content"`
	Metadata   textMetadata `json:"metadata,omitempty"`
}
type textContent struct {
	ContentType string `json:"content_type"`
	Parts       []any  `json:"parts"`
}
type textImagePart struct {
	ContentType  string `json:"content_type"`
	AssetPointer string `json:"asset_pointer"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	SizeBytes    int64  `json:"size_bytes"`
}
type textAttachment struct {
	ID       string `json:"id"`
	MIMEType string `json:"mimeType"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}
type textMetadata struct {
	Attachments []textAttachment `json:"attachments,omitempty"`
}

type textClientContext struct {
	IsDarkMode      bool    `json:"is_dark_mode"`
	TimeSinceLoaded int     `json:"time_since_loaded"`
	PageHeight      int     `json:"page_height"`
	PageWidth       int     `json:"page_width"`
	PixelRatio      float64 `json:"pixel_ratio"`
	ScreenHeight    int     `json:"screen_height"`
	ScreenWidth     int     `json:"screen_width"`
}

type textPayload struct {
	Action           string                    `json:"action"`
	Messages         []textConversationMessage `json:"messages"`
	Model            string                    `json:"model"`
	ParentMessageID  string                    `json:"parent_message_id"`
	ConversationID   string                    `json:"conversation_id,omitempty"`
	ConversationMode struct {
		Kind string `json:"kind"`
	} `json:"conversation_mode"`
	ForceUseSSE                bool              `json:"force_use_sse"`
	HistoryAndTrainingDisabled bool              `json:"history_and_training_disabled"`
	Suggestions                []string          `json:"suggestions"`
	SupportedEncodings         []string          `json:"supported_encodings"`
	SystemHints                []string          `json:"system_hints"`
	Timezone                   string            `json:"timezone"`
	TimezoneOffsetMins         int               `json:"timezone_offset_min"`
	VariantPurpose             string            `json:"variant_purpose"`
	WebsocketRequestID         string            `json:"websocket_request_id"`
	ClientContextualInfo       textClientContext `json:"client_contextual_info"`
	ThinkingEffort             string            `json:"thinking_effort,omitempty"`
}

func textModelSlug(model string) string {
	if model = strings.TrimSpace(model); model != "" {
		return model
	}
	return "auto"
}

func normalizeThinkingEffort(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(value))
	case "xhigh", "extended":
		return "extended"
	default:
		return ""
	}
}

// ParseTextSSE uses assistant message snapshots to build a final answer. Web
// SSE often sends whole-message snapshots rather than isolated token deltas;
// retaining the latest snapshot avoids duplicate text in both variants.
func ParseTextSSE(ctx context.Context, reader io.Reader) (TextResult, error) {
	return parseTextSSE(ctx, reader, nil)
}

func parseTextSSE(ctx context.Context, reader io.Reader, emit func(TextDelta) error) (TextResult, error) {
	result := TextResult{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxTextSSELineBytes)
	dataLines := make([]string, 0, 2)
	var emitErr error
	flush := func() {
		if len(dataLines) == 0 {
			return
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if payload == "[DONE]" {
			result.Done = true
			return
		}
		var patch textSSEPatch
		if err := json.Unmarshal([]byte(payload), &patch); err != nil {
			return
		}
		if patch.ConversationID != "" && result.ConversationID == "" {
			result.ConversationID = bounded(patch.ConversationID, 512)
		}
		if patch.Message != nil && patch.Message.Author.Role == "assistant" {
			if messageID := strings.TrimSpace(patch.Message.ID); messageID != "" {
				result.AssistantMessageID = bounded(messageID, 512)
			}
			if text := patch.Message.Content.text(); text != "" {
				next := bounded(text, maxTextContentBytes)
				delta := next
				if strings.HasPrefix(next, result.Text) {
					delta = strings.TrimPrefix(next, result.Text)
				}
				result.Text = next
				if delta != "" && emit != nil {
					emitErr = emit(TextDelta{
						ConversationID:     result.ConversationID,
						AssistantMessageID: result.AssistantMessageID,
						Text:               delta,
					})
				}
			}
		}
	}
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return TextResult{}, ctx.Err()
		default:
		}
		line := scanner.Text()
		if line == "" {
			flush()
			if emitErr != nil {
				return TextResult{}, emitErr
			}
			if result.Done {
				return finishTextResult(result)
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return TextResult{}, fmt.Errorf("read text SSE: %w", err)
	}
	flush()
	if emitErr != nil {
		return TextResult{}, emitErr
	}
	return finishTextResult(result)
}

func finishTextResult(result TextResult) (TextResult, error) {
	if strings.TrimSpace(result.Text) == "" {
		return TextResult{}, fmt.Errorf("text conversation: assistant response not found in SSE")
	}
	if strings.TrimSpace(result.ConversationID) == "" || strings.TrimSpace(result.AssistantMessageID) == "" {
		return TextResult{}, fmt.Errorf("text conversation: continuation anchors not found in SSE")
	}
	return result, nil
}

type textSSEPatch struct {
	ConversationID string          `json:"conversation_id"`
	Message        *textSSEMessage `json:"message"`
}

type textSSEMessage struct {
	ID     string `json:"id"`
	Author struct {
		Role string `json:"role"`
	} `json:"author"`
	Content textSSEContent `json:"content"`
}

type textSSEContent struct {
	Parts []json.RawMessage `json:"parts"`
}

func (c textSSEContent) text() string {
	parts := make([]string, 0, len(c.Parts))
	for _, raw := range c.Parts {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "")
}
