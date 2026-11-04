package client

import (
	completion "aetherrelay/internal/pkg/aetherrelaycompletion"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	generation "aetherrelay/internal/pkg/aetherrelaygeneration"
	"aetherrelay/internal/pkg/chatattachment"
	http "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

const (
	maxTextSSELineBytes = 1 << 20
	maxTextSSEBytes     = 16 << 20
	maxTextMessages     = 64
	maxTextContentBytes = completion.MaxTextBytes
)

// TextMessage is a text-only Web conversation input. It deliberately has no
// dynamic content field, so callers cannot accidentally pass tool or image
// payloads through the text path.
type TextMessage struct {
	Role    string
	Content string
	Images  [][]byte
	Files   []chatattachment.File
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
	Completion         completion.Text
	ErrorOperation     string
	UpstreamStatus     int
	GenerationNotSent  bool
	Generation         generation.Sample
	ConversationID     string
	AssistantMessageID string
	ActualModel        string
	Text               string
	Done               bool
}

// TextDelta is an incremental assistant update derived from the Web SSE
// message snapshots.
type TextDelta struct {
	Generation         generation.Sample
	ConversationID     string
	AssistantMessageID string
	ActualModel        string
	Text               string
}

// CompleteText runs the authenticated Web conversation lifecycle and collects
// the text SSE into one non-streaming result. Its SSE parsing is intentionally
// separate from the HTTP gateway so it can later back a streaming adapter.
func (c *Client) CompleteText(ctx context.Context, request TextRequest) (TextResult, error) {
	return c.StreamText(ctx, request, nil, nil)
}

// StreamText keeps all transport handles inside the upstream client while
// exposing bounded text deltas and a frozen generation sample to its owner.
// finishGeneration runs before response-body cleanup, including on read failure.
func (c *Client) StreamText(ctx context.Context, request TextRequest, emit func(TextDelta) error, finishGeneration func(generation.Sample)) (result TextResult, resultErr error) {
	notSent := true
	observedStatus := 0
	defer func() {
		result.UpstreamStatus = observedStatus
		if resultErr != nil {
			result.GenerationNotSent = notSent
			if !notSent {
				result.ErrorOperation = "text_conversation"
			}
			var failure *Error
			if errors.As(resultErr, &failure) {
				result.ErrorOperation = failure.Operation
				if failure.StatusCode != 0 {
					result.UpstreamStatus = failure.StatusCode
				}
			}
		}
	}()
	if err := validateTextRequest(request); err != nil {
		return TextResult{}, &Error{Class: InvalidRequest, Operation: "text_validate", Detail: "invalid text request", Cause: err}
	}
	requirements, err := c.ChatRequirementsContext(ctx)
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
	notSent = false // Once Do starts, transport errors may hide a submitted generation.
	response, err := c.doer.Do(req)
	if err != nil {
		return TextResult{}, classifyTransport("text_conversation", err)
	}
	observedStatus = response.StatusCode
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return TextResult{}, classifyStatusResponse("text_conversation", response.StatusCode, response.Body)
	}
	return parseTextSSEObserved(ctx, response.Body, emit, finishGeneration)
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
	fileCount, fileBytes := 0, 0
	for index, message := range request.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role != "system" && role != "user" && role != "assistant" {
			return fmt.Errorf("message %d has unsupported role", index+1)
		}
		content := strings.TrimSpace(message.Content)
		if content == "" && len(message.Images) == 0 && len(message.Files) == 0 {
			return fmt.Errorf("message %d content is required", index+1)
		}
		if len(content) > maxTextContentBytes {
			return fmt.Errorf("message %d content exceeds %d KiB", index+1, maxTextContentBytes>>10)
		}
		for _, file := range message.Files {
			fileCount++
			fileBytes += len(file.Bytes)
			if fileCount > chatattachment.MaxFileCount || fileBytes > chatattachment.MaxFileBytes {
				return fmt.Errorf("file attachments exceed request limits")
			}
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
		for fileIndex, file := range message.Files {
			reference, err := c.UploadAttachment(file)
			if err != nil {
				return nil, fmt.Errorf("upload chat attachment %d/%d: %w", index+1, fileIndex+1, err)
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
				if ref.Width > 0 && ref.Height > 0 {
					parts = append(parts, textImagePart{ContentType: "image_asset_pointer", AssetPointer: "file-service://" + ref.FileID, Width: ref.Width, Height: ref.Height, SizeBytes: ref.FileSize})
				}
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
		Timezone:                   browserTimezone,
		TimezoneOffsetMins:         browserTimezoneOffsetMinutes,
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
	MIMEType string `json:"mime_type"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
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
	return parseTextSSEObserved(ctx, reader, emit, nil)
}

// textLimits remain local policy; tests use small exact boundaries without
// changing production defaults or allocating unbounded input.
type textLimits struct {
	total                        int64
	line, event, content, output int
}

var defaultTextLimits = textLimits{maxTextSSEBytes, maxTextSSELineBytes, maxTextSSELineBytes, maxTextContentBytes, maxTextContentBytes}

type textCompletionError struct {
	reason completion.Reason
	limit  int64
}

func (e *textCompletionError) Error() string            { return "text conversation: " + string(e.reason) }
func (e *textCompletionError) TextFailureClass() string { return e.reason.FailureClass() }

// budgetReader probes at most one byte past the boundary only if parsing needs
// more input. A terminal event within the budget does not require draining EOF.
type textBudgetReader struct {
	reader           io.Reader
	remaining, bytes int64
}

func (r *textBudgetReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		r.bytes += int64(n)
		if n > 0 {
			return 0, &textCompletionError{reason: completion.SSELimit}
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.bytes += int64(n)
	r.remaining -= int64(n)
	return n, err
}

func parseTextSSEObserved(ctx context.Context, reader io.Reader, emit func(TextDelta) error, finishGeneration func(generation.Sample)) (TextResult, error) {
	return parseTextSSELimited(ctx, reader, emit, finishGeneration, defaultTextLimits)
}

func parseTextSSELimited(ctx context.Context, reader io.Reader, emit func(TextDelta) error, finishGeneration func(generation.Sample), limits textLimits) (out TextResult, resultErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var result TextResult
	var clock generation.Clock
	var firstOutputAt time.Time
	digest := sha256.New()
	counted := &textBudgetReader{reader: io.TeeReader(reader, digest), remaining: limits.total}
	fail := func(reason completion.Reason, limit int64) error {
		return &textCompletionError{reason: reason, limit: limit}
	}
	defer func() {
		at := time.Now()
		if resultErr == nil {
			clock.Finish(at)
		}
		clock.Stop(at)
		out.Generation = clock.Snapshot(at)
		out.Completion.SSEBytes = counted.bytes
		out.Completion.SSESHA256 = fmt.Sprintf("%x", digest.Sum(nil))
		out.Completion.TextBytes = len(out.Text)
		out.Completion.TextSHA256 = completion.Hash(out.Text)
		if resultErr != nil {
			var failure *textCompletionError
			if errors.As(resultErr, &failure) {
				out.Completion.Reason = failure.reason
				out.Completion.Limit = failure.limit
			}
			out.Done = false
			out.Completion.Completed = false
		}
		if finishGeneration != nil {
			finishGeneration(out.Generation)
		}
	}()
	buffered := bufio.NewReaderSize(counted, 32<<10)
	var data bytes.Buffer
	var channel string
	boundIdentity := false
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		result.Completion.Events++
		if strings.TrimSpace(payload) == "" {
			return nil
		}
		if payload == "[DONE]" {
			result.Completion.Terminal = "done"
			if strings.TrimSpace(result.Text) == "" || result.ConversationID == "" || result.AssistantMessageID == "" {
				return fail(completion.InvalidResult, 0)
			}
			result.Done = true
			result.Completion.Completed = true
			result.Completion.Reason = completion.Stop
			return nil
		}
		if !utf8.ValidString(payload) || !json.Valid([]byte(payload)) {
			return fail(completion.Protocol, 0)
		}
		if failure := parseSSEError("text_conversation", payload); failure != nil {
			result.Completion.Reason = completion.ReadError
			var anchor struct {
				ConversationID string `json:"conversation_id"`
				Value          struct {
					ConversationID string `json:"conversation_id"`
				} `json:"v"`
			}
			_ = json.Unmarshal([]byte(payload), &anchor)
			if result.ConversationID == "" {
				result.ConversationID = bounded(anchor.ConversationID, 512)
				if result.ConversationID == "" {
					result.ConversationID = bounded(anchor.Value.ConversationID, 512)
				}
			}
			return failure
		}
		var patch textSSEPatch
		if err := json.Unmarshal([]byte(payload), &patch); err != nil {
			return fail(completion.Protocol, 0)
		}
		// Only explicitly recognized control/metadata shapes may omit a message.
		if patch.Message == nil {
			var fields map[string]json.RawMessage
			if json.Unmarshal([]byte(payload), &fields) != nil || fields == nil {
				return fail(completion.Protocol, 0)
			}
			for name := range fields {
				switch name {
				case "conversation_id", "type", "moderation_response", "is_completion", "conversation_signature", "server_response", "message_id":
				default:
					return fail(completion.Protocol, 0)
				}
			}
			if patch.Type != "" {
				switch patch.Type {
				case "message_stream_complete", "moderation", "conversation_detail_metadata", "title_generation", "ping":
				default:
					return fail(completion.Protocol, 0)
				}
			}
			if boundIdentity && patch.ConversationID != "" && patch.ConversationID != result.ConversationID {
				return fail(completion.Snapshot, 0)
			}
			if !boundIdentity && patch.ConversationID != "" {
				result.ConversationID = bounded(patch.ConversationID, 512)
			}
			return nil
		}
		message := patch.Message
		if message.Author.Role != "assistant" {
			switch message.Author.Role {
			case "user", "system", "tool":
				return nil
			default:
				return fail(completion.Protocol, 0)
			}
		}
		next, err := message.Content.text()
		if err != nil {
			return fail(completion.Protocol, 0)
		}
		if !utf8.ValidString(next) {
			return fail(completion.Protocol, 0)
		}
		if len(next) > limits.content {
			return fail(completion.ContentLimit, int64(limits.content))
		}
		conversationID := strings.TrimSpace(patch.ConversationID)
		if conversationID == "" {
			conversationID = result.ConversationID
		}
		messageID := strings.TrimSpace(message.ID)
		// Metadata-only assistant updates must not rebind an answer either.
		if boundIdentity && (conversationID != result.ConversationID || messageID != "" && messageID != result.AssistantMessageID || message.Channel != channel) {
			return fail(completion.Snapshot, 0)
		}
		if !boundIdentity && next != "" {
			if conversationID == "" || len(conversationID) > 512 || messageID == "" || len(messageID) > 512 {
				result.Text = next
				return fail(completion.InvalidResult, 0)
			}
			if message.Channel != "" && message.Channel != "final" {
				return fail(completion.Protocol, 0)
			}
			result.ConversationID = conversationID
			result.AssistantMessageID = messageID
			channel = message.Channel
			boundIdentity = true
		}
		if !boundIdentity {
			return nil
		}
		if !strings.HasPrefix(next, result.Text) {
			return fail(completion.Snapshot, 0)
		}
		delta := strings.TrimPrefix(next, result.Text)
		if result.Completion.OutputBytes+len(delta) > limits.output {
			return fail(completion.OutputLimit, int64(limits.output))
		}
		if model := strings.TrimSpace(message.Metadata.ModelSlug); model != "" {
			result.ActualModel = bounded(model, 256)
		}
		result.Text = next
		if delta != "" {
			at := time.Now()
			clock.Output(at)
			if firstOutputAt.IsZero() {
				firstOutputAt = at
			}
			// Count accepted append-only output, including non-streaming collection.
			result.Completion.OutputBytes += len(delta)
			if emit != nil {
				if err := emit(TextDelta{Generation: generation.Sample{FirstOutputAt: firstOutputAt, Duration: at.Sub(firstOutputAt), Partial: true}, ConversationID: result.ConversationID, AssistantMessageID: result.AssistantMessageID, ActualModel: result.ActualModel, Text: delta}); err != nil {
					result.Completion.Reason = completion.EmitError
					return err
				}
			}
		}
		switch message.Metadata.FinishDetails.Type {
		case "", "stop":
		case "max_tokens", "length":
			return fail(completion.ModelLength, 0)
		default:
			return fail(completion.Protocol, 0)
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			result.Completion.Reason = completion.Canceled
			return result, err
		}
		var line []byte
		var readErr error
		for {
			fragment, err := buffered.ReadSlice('\n')
			line = append(line, fragment...)
			if len(line) > limits.line+2 {
				return result, fail(completion.LineLimit, int64(limits.line))
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			readErr = err
			break
		}
		if readErr != nil && readErr != io.EOF {
			var failure *textCompletionError
			if errors.As(readErr, &failure) {
				return result, fail(failure.reason, limits.total)
			}
			result.Completion.Reason = completion.ReadError
			return result, fmt.Errorf("read text SSE: %w", readErr)
		}
		raw := bytes.TrimSuffix(line, []byte("\n"))
		raw = bytes.TrimSuffix(raw, []byte("\r"))
		if len(raw) > limits.line {
			return result, fail(completion.LineLimit, int64(limits.line))
		}
		if len(raw) == 0 {
			if err := flush(); err != nil {
				return result, err
			}
		} else if bytes.HasPrefix(raw, []byte("data:")) {
			payload := bytes.TrimPrefix(raw, []byte("data:"))
			payload = bytes.TrimPrefix(payload, []byte(" "))
			if data.Len()+len(payload)+1 > limits.event {
				return result, fail(completion.EventLimit, int64(limits.event))
			}
			data.Write(payload)
			data.WriteByte('\n')
		}
		if result.Done {
			return result, nil
		}
		if readErr != nil {
			if err := flush(); err != nil {
				return result, err
			}
			if result.Done {
				return result, nil
			}
			return result, fail(completion.EOF, 0)
		}
	}
}

type textSSEPatch struct {
	ConversationID string          `json:"conversation_id"`
	Type           string          `json:"type"`
	Message        *textSSEMessage `json:"message"`
}
type textSSEMessage struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
	Author  struct {
		Role string `json:"role"`
	} `json:"author"`
	Content  textSSEContent  `json:"content"`
	Metadata textSSEMetadata `json:"metadata"`
}
type textSSEMetadata struct {
	ModelSlug     string `json:"model_slug"`
	FinishDetails struct {
		Type string `json:"type"`
	} `json:"finish_details"`
}
type textSSEContent struct {
	Type  string            `json:"content_type"`
	Parts []json.RawMessage `json:"parts"`
}

func (c textSSEContent) text() (string, error) {
	if c.Type != "" && c.Type != "text" && c.Type != "multimodal_text" {
		return "", fmt.Errorf("unsupported text content")
	}
	if c.Parts == nil {
		return "", fmt.Errorf("missing text parts")
	}
	parts := make([]string, 0, len(c.Parts))
	for _, raw := range c.Parts {
		var value string
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return "", fmt.Errorf("unsupported text part")
		}
		parts = append(parts, value)
	}
	return strings.Join(parts, ""), nil
}
