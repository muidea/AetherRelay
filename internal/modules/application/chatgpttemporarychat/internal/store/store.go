// Package store provides temporary-chat conversation and message persistence.
package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ai-proxy/internal/modules/application/chatgpttemporarychat/pkg/common"
	events "ai-proxy/internal/modules/application/chatgpttemporarychat/pkg/events"
	"ai-proxy/internal/pkg/aiproxystate"
	"ai-proxy/internal/pkg/chatattachment"
	"ai-proxy/internal/pkg/chatgptimageinput"

	"github.com/google/uuid"
)

type Config struct {
	RetentionDays              int
	MaxConversations           int
	MaxMessagesPerConversation int
	MaxMessageBytes            int
}

type Store struct {
	docs *aiproxystate.Documents
	cfg  Config
	mu   sync.Mutex
}

func Open(database, memoryLimit string, threads int, cfg Config) (*Store, error) {
	docs, err := aiproxystate.Open(database, memoryLimit, threads)
	if err != nil {
		return nil, err
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 30
	}
	if cfg.MaxConversations <= 0 {
		cfg.MaxConversations = 2000
	}
	if cfg.MaxMessagesPerConversation <= 0 {
		cfg.MaxMessagesPerConversation = 200
	}
	if cfg.MaxMessageBytes <= 0 {
		cfg.MaxMessageBytes = 262144
	}
	return &Store{docs: docs, cfg: cfg}, nil
}

func (s *Store) Close() error {
	if s == nil || s.docs == nil {
		return nil
	}
	return s.docs.Close()
}

func (s *Store) CreateConversation(ownerID, model, thinkingEffort, systemPrompt, provider string, accountIDs ...string) (events.ConversationView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID := ""
	if len(accountIDs) > 0 {
		accountID = accountIDs[0]
	} else {
		// Compatibility for conversations created by the former ChatGPT Web-only
		// call shape, whose fifth argument was the pinned account ID.
		accountID = provider
		provider = "chatgptweb"
	}
	ownerID = strings.TrimSpace(ownerID)
	model = strings.TrimSpace(model)
	accountID = strings.TrimSpace(accountID)
	provider = strings.TrimSpace(provider)
	if ownerID == "" || model == "" || provider == "" {
		return events.ConversationView{}, fmt.Errorf("owner, model and provider are required")
	}
	systemPrompt = strings.TrimSpace(systemPrompt)
	if len(systemPrompt) > s.cfg.MaxMessageBytes {
		return events.ConversationView{}, fmt.Errorf("system prompt exceeds max size")
	}
	now := time.Now().UTC()
	if _, err := s.docs.PurgeExpiredTemporaryConversations(now); err != nil {
		return events.ConversationView{}, err
	}
	count, err := s.docs.CountTemporaryConversations(ownerID)
	if err != nil {
		return events.ConversationView{}, err
	}
	if count >= s.cfg.MaxConversations {
		return events.ConversationView{}, fmt.Errorf("conversation limit reached; delete old conversations first")
	}
	row := aiproxystate.TemporaryConversationRow{
		OwnerID:        ownerID,
		ConversationID: uuid.NewString(),
		Title:          "新对话",
		AccountID:      accountID,
		Provider:       provider,
		Model:          model,
		ThinkingEffort: strings.TrimSpace(thinkingEffort),
		SystemPrompt:   systemPrompt,
		Status:         common.StatusIdle,
		CreatedAt:      now,
		UpdatedAt:      now,
		ExpiresAt:      now.AddDate(0, 0, s.cfg.RetentionDays),
	}
	if err := s.docs.CreateTemporaryConversation(row); err != nil {
		return events.ConversationView{}, err
	}
	return conversationView(row), nil
}

func (s *Store) ListConversations(ownerID, cursor string, limit int) (events.ListConversationsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var before *time.Time
	if strings.TrimSpace(cursor) != "" {
		ts, err := time.Parse(time.RFC3339Nano, cursor)
		if err != nil {
			return events.ListConversationsResult{}, fmt.Errorf("invalid cursor")
		}
		before = &ts
	}
	rows, err := s.docs.ListTemporaryConversations(ownerID, limit+1, before)
	if err != nil {
		return events.ListConversationsResult{}, err
	}
	result := events.ListConversationsResult{}
	for i, row := range rows {
		if i >= limit {
			result.NextCursor = rows[limit-1].UpdatedAt.UTC().Format(time.RFC3339Nano)
			break
		}
		result.Items = append(result.Items, conversationView(row))
	}
	return result, nil
}

func (s *Store) GetConversation(ownerID, conversationID string, beforeSequence *int64, limit int) (events.ConversationDetailResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, found, err := s.docs.LoadTemporaryConversation(ownerID, conversationID)
	if err != nil {
		return events.ConversationDetailResult{}, err
	}
	if !found {
		return events.ConversationDetailResult{}, fmt.Errorf("conversation not found")
	}
	if !time.Now().UTC().Before(row.ExpiresAt) {
		return events.ConversationDetailResult{}, fmt.Errorf("conversation expired")
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	messages, err := s.docs.ListTemporaryMessages(ownerID, conversationID, beforeSequence, limit+1)
	if err != nil {
		return events.ConversationDetailResult{}, err
	}
	detail := events.ConversationDetailResult{Conversation: conversationView(row)}
	for i, message := range messages {
		if i >= limit {
			detail.HasMore = true
			break
		}
		detail.Messages = append(detail.Messages, messageView(message))
	}
	return detail, nil
}

func (s *Store) LoadConversationRow(ownerID, conversationID string) (aiproxystate.TemporaryConversationRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docs.LoadTemporaryConversation(ownerID, conversationID)
}

func (s *Store) SetProvider(ownerID, conversationID, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil
	}
	row, found, err := s.docs.LoadTemporaryConversation(ownerID, conversationID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("conversation not found")
	}
	row.Provider = provider
	return s.docs.UpdateTemporaryConversation(row)
}

type TurnStart struct {
	Conversation           events.ConversationView
	UserMessage            events.MessageView
	AssistantMessage       events.MessageView
	TurnID                 string
	UserSequence           int64
	AssistantSequence      int64
	UpstreamConversationID string
	ParentMessageID        string
	AccountID              string
	Provider               string
	Model                  string
	ThinkingEffort         string
	SystemPrompt           string
	Images                 [][]byte
	Files                  []chatattachment.File
}

func (s *Store) StartTurn(ownerID, conversationID, content string, imageInputs ...[]events.ImageInput) (TurnStart, error) {
	var inputs []events.ImageInput
	if len(imageInputs) > 0 {
		inputs = imageInputs[0]
	}
	return s.startTurn(ownerID, conversationID, content, inputs, nil)
}

func (s *Store) StartTurnWithAttachments(ownerID, conversationID, content string, imageInputs []events.ImageInput, attachmentInputs []chatattachment.File) (TurnStart, error) {
	return s.startTurn(ownerID, conversationID, content, imageInputs, attachmentInputs)
}

func (s *Store) startTurn(ownerID, conversationID, content string, inputs []events.ImageInput, attachmentInputs []chatattachment.File) (TurnStart, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content = strings.TrimSpace(content)
	if len(content) > s.cfg.MaxMessageBytes {
		return TurnStart{}, fmt.Errorf("message exceeds max size")
	}
	images, err := validateImages(inputs)
	if err != nil {
		return TurnStart{}, err
	}
	attachments, err := validateAttachments(attachmentInputs)
	if err != nil {
		return TurnStart{}, err
	}
	if len(images)+len(attachments) > chatattachment.MaxFileCount {
		return TurnStart{}, fmt.Errorf("at most %d attachments are supported per turn", chatattachment.MaxFileCount)
	}
	totalBytes := 0
	for _, image := range images {
		totalBytes += len(image.Bytes)
	}
	for _, attachment := range attachments {
		totalBytes += len(attachment.Bytes)
	}
	if totalBytes > chatattachment.MaxFileBytes {
		return TurnStart{}, fmt.Errorf("attachments exceed %d MiB per turn", chatattachment.MaxFileBytes>>20)
	}
	if content == "" && len(images) == 0 && len(attachments) == 0 {
		return TurnStart{}, fmt.Errorf("content or attachment is required")
	}
	row, found, err := s.docs.LoadTemporaryConversation(ownerID, conversationID)
	if err != nil {
		return TurnStart{}, err
	}
	if !found {
		return TurnStart{}, fmt.Errorf("conversation not found")
	}
	if time.Now().UTC().After(row.ExpiresAt) {
		return TurnStart{}, fmt.Errorf("conversation expired")
	}
	switch row.Status {
	case common.StatusStreaming:
		return TurnStart{}, fmt.Errorf("conversation is streaming")
	case common.StatusRecoveryRequired:
		return TurnStart{}, fmt.Errorf("conversation requires recovery")
	case common.StatusClosed:
		return TurnStart{}, fmt.Errorf("conversation closed")
	}
	next, err := s.docs.NextTemporaryMessageSequence(ownerID, conversationID)
	if err != nil {
		return TurnStart{}, err
	}
	if next+1 > int64(s.cfg.MaxMessagesPerConversation) {
		return TurnStart{}, fmt.Errorf("message limit reached for conversation")
	}
	now := time.Now().UTC()
	userID := uuid.NewString()
	assistantID := uuid.NewString()
	metadata := make([]temporaryImageMetadata, 0, len(images))
	imageRows := make([]aiproxystate.TemporaryMessageImageRow, 0, len(images))
	for _, image := range images {
		imageID := uuid.NewString()
		metadata = append(metadata, temporaryImageMetadata{ID: imageID, ContentType: image.ContentType, SizeBytes: int64(len(image.Bytes))})
		imageRows = append(imageRows, aiproxystate.TemporaryMessageImageRow{
			OwnerID: ownerID, ConversationID: conversationID, MessageID: userID, ImageID: imageID, ContentType: image.ContentType, Bytes: image.Bytes,
		})
	}
	imageMetadata, err := json.Marshal(metadata)
	if err != nil {
		return TurnStart{}, fmt.Errorf("encode image metadata: %w", err)
	}
	attachmentMetadata := make([]temporaryAttachmentMetadata, 0, len(attachments))
	attachmentRows := make([]aiproxystate.TemporaryMessageAttachmentRow, 0, len(attachments))
	for _, attachment := range attachments {
		attachmentID := uuid.NewString()
		attachmentMetadata = append(attachmentMetadata, temporaryAttachmentMetadata{ID: attachmentID, FileName: attachment.Name, ContentType: attachment.ContentType, SizeBytes: int64(len(attachment.Bytes))})
		attachmentRows = append(attachmentRows, aiproxystate.TemporaryMessageAttachmentRow{OwnerID: ownerID, ConversationID: conversationID, MessageID: userID, AttachmentID: attachmentID, FileName: attachment.Name, ContentType: attachment.ContentType, Bytes: attachment.Bytes})
	}
	encodedAttachments, err := json.Marshal(attachmentMetadata)
	if err != nil {
		return TurnStart{}, fmt.Errorf("encode attachment metadata: %w", err)
	}
	user := aiproxystate.TemporaryMessageRow{
		OwnerID:            ownerID,
		ConversationID:     conversationID,
		Sequence:           next,
		MessageID:          userID,
		Role:               common.RoleUser,
		Content:            content,
		ImageMetadata:      imageMetadata,
		AttachmentMetadata: encodedAttachments,
		Status:             common.MessageStatusStreaming,
		CreatedAt:          now,
	}
	assistant := aiproxystate.TemporaryMessageRow{
		OwnerID:        ownerID,
		ConversationID: conversationID,
		Sequence:       next + 1,
		MessageID:      assistantID,
		Role:           common.RoleAssistant,
		Content:        "",
		Status:         common.MessageStatusStreaming,
		CreatedAt:      now,
	}
	if row.Title == "新对话" || strings.TrimSpace(row.Title) == "" {
		if content != "" {
			row.Title = truncateTitle(content)
		} else if len(images) > 0 && len(attachments) == 0 {
			row.Title = "图片对话"
		} else {
			row.Title = "附件对话"
		}
	}
	row.Status = common.StatusStreaming
	row.UpdatedAt = now
	if err := s.docs.StartTemporaryTurn(row, user, assistant, imageRows, attachmentRows); err != nil {
		return TurnStart{}, err
	}
	return TurnStart{
		Conversation:           conversationView(row),
		UserMessage:            messageView(user),
		AssistantMessage:       messageView(assistant),
		TurnID:                 assistantID,
		UserSequence:           user.Sequence,
		AssistantSequence:      assistant.Sequence,
		UpstreamConversationID: row.UpstreamConversationID,
		ParentMessageID:        row.ParentMessageID,
		AccountID:              row.AccountID,
		Provider:               defaultProvider(row.Provider),
		Model:                  row.Model,
		ThinkingEffort:         row.ThinkingEffort,
		SystemPrompt:           row.SystemPrompt,
		Images:                 imageBytes(images),
		Files:                  attachments,
	}, nil
}

type temporaryImageMetadata struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

type temporaryAttachmentMetadata struct {
	ID          string `json:"id"`
	FileName    string `json:"file_name"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

func validateAttachments(inputs []chatattachment.File) ([]chatattachment.File, error) {
	if len(inputs) > chatattachment.MaxFileCount {
		return nil, fmt.Errorf("at most %d files are supported per turn", chatattachment.MaxFileCount)
	}
	result := make([]chatattachment.File, 0, len(inputs))
	for index, input := range inputs {
		file, err := chatattachment.Validate(input.Bytes, input.Name, input.ContentType)
		if err != nil {
			return nil, fmt.Errorf("attachment %d: %w", index+1, err)
		}
		result = append(result, file)
	}
	return result, nil
}

func validateImages(inputs []events.ImageInput) ([]events.ImageInput, error) {
	if len(inputs) > imageinput.MaxChatImageCount {
		return nil, fmt.Errorf("at most %d images are supported per turn", imageinput.MaxChatImageCount)
	}
	images := make([]events.ImageInput, 0, len(inputs))
	totalBytes := 0
	for index, input := range inputs {
		image, err := imageinput.ValidateImage(input.Bytes)
		if err != nil {
			return nil, fmt.Errorf("image %d: %w", index+1, err)
		}
		totalBytes += len(image.Bytes)
		if totalBytes > imageinput.MaxChatImageBytes {
			return nil, fmt.Errorf("images exceed %d MiB per turn", imageinput.MaxChatImageBytes>>20)
		}
		images = append(images, events.ImageInput{Bytes: image.Bytes, ContentType: image.MIMEType})
	}
	return images, nil
}

func imageBytes(images []events.ImageInput) [][]byte {
	result := make([][]byte, 0, len(images))
	for _, image := range images {
		result = append(result, image.Bytes)
	}
	return result
}

type TurnComplete struct {
	Conversation events.ConversationView
	Message      events.MessageView
}

func (s *Store) CompleteTurn(ownerID, conversationID string, userSequence, assistantSequence int64, content, actualModel, upstreamConversationID, assistantMessageID string, cancelled, interrupted, recoveryRequired bool, errorClass, errorMessage string) (TurnComplete, error) {
	return s.completeTurn(ownerID, conversationID, userSequence, assistantSequence, content, actualModel, upstreamConversationID, assistantMessageID, cancelled, interrupted, recoveryRequired, errorClass, errorMessage, true)
}

func (s *Store) CompleteFeatureTurn(ownerID, conversationID string, userSequence, assistantSequence int64, content, actualModel string, cancelled bool, errorClass, errorMessage string) (TurnComplete, error) {
	return s.completeTurn(ownerID, conversationID, userSequence, assistantSequence, content, actualModel, "", "", cancelled, false, false, errorClass, errorMessage, false)
}

func (s *Store) completeTurn(ownerID, conversationID string, userSequence, assistantSequence int64, content, actualModel, upstreamConversationID, assistantMessageID string, cancelled, interrupted, recoveryRequired bool, errorClass, errorMessage string, requireAnchors bool) (TurnComplete, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, found, err := s.docs.LoadTemporaryConversation(ownerID, conversationID)
	if err != nil {
		return TurnComplete{}, err
	}
	if !found {
		return TurnComplete{}, fmt.Errorf("conversation not found")
	}
	now := time.Now().UTC()
	if requireAnchors && !cancelled && errorClass == "" && (upstreamConversationID == "" || assistantMessageID == "") {
		// A text response without both anchors cannot be continued safely. Keep
		// the visible response but force a new conversation instead of guessing.
		interrupted = true
		recoveryRequired = true
		errorClass = "upstream"
		errorMessage = "upstream continuation anchors are missing"
	}
	userStatus := common.MessageStatusCompleted
	assistantStatus := common.MessageStatusCompleted
	if cancelled {
		userStatus = common.MessageStatusCancelled
		assistantStatus = common.MessageStatusCancelled
	} else if interrupted {
		userStatus = common.MessageStatusInterrupted
		assistantStatus = common.MessageStatusInterrupted
	} else if errorClass != "" {
		userStatus = common.MessageStatusError
		assistantStatus = common.MessageStatusError
	}
	// Load existing message content for user (status-only update needs content preserved).
	messages, err := s.docs.ListTemporaryMessages(ownerID, conversationID, nil, int(assistantSequence)+1)
	if err != nil {
		return TurnComplete{}, err
	}
	var userRow, assistantRow aiproxystate.TemporaryMessageRow
	for _, message := range messages {
		if message.Sequence == userSequence {
			userRow = message
		}
		if message.Sequence == assistantSequence {
			assistantRow = message
		}
	}
	if userRow.MessageID == "" || assistantRow.MessageID == "" {
		return TurnComplete{}, fmt.Errorf("turn messages not found")
	}
	userRow.Status = userStatus
	userRow.CompletedAt = &now
	if content != "" {
		assistantRow.Content = content
	}
	assistantRow.Status = assistantStatus
	assistantRow.ErrorClass = errorClass
	assistantRow.ErrorMessage = sanitizeError(errorMessage)
	assistantRow.CompletedAt = &now
	if assistantMessageID != "" {
		assistantRow.UpstreamMessageID = assistantMessageID
	}
	if actualModel = strings.TrimSpace(actualModel); actualModel != "" {
		assistantRow.ActualModel = actualModel
		row.ActualModel = actualModel
	}
	if !cancelled && errorClass == "" {
		row.UpstreamConversationID = upstreamConversationID
		row.ParentMessageID = assistantMessageID
		row.Status = common.StatusIdle
	} else if cancelled {
		row.Status = common.StatusIdle
	} else if recoveryRequired {
		row.Status = common.StatusRecoveryRequired
	} else {
		row.Status = common.StatusIdle
	}
	row.UpdatedAt = now
	if err := s.docs.CompleteTemporaryTurn(row, userRow, assistantRow); err != nil {
		return TurnComplete{}, err
	}
	return TurnComplete{Conversation: conversationView(row), Message: messageView(assistantRow)}, nil
}

func (s *Store) UpdateAssistantDelta(ownerID, conversationID string, assistantSequence int64, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	messages, err := s.docs.ListTemporaryMessages(ownerID, conversationID, nil, int(assistantSequence)+1)
	if err != nil {
		return err
	}
	var assistantRow aiproxystate.TemporaryMessageRow
	for _, message := range messages {
		if message.Sequence == assistantSequence {
			assistantRow = message
			break
		}
	}
	if assistantRow.MessageID == "" {
		return fmt.Errorf("assistant message not found")
	}
	if assistantRow.Status != common.MessageStatusStreaming {
		return nil
	}
	assistantRow.Content = content
	return s.docs.UpdateTemporaryMessage(assistantRow)
}

func (s *Store) InterruptStreaming() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.docs.ListStreamingTemporaryConversations()
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	count := 0
	for _, row := range rows {
		messages, err := s.docs.ListStreamingTemporaryMessages(row.OwnerID, row.ConversationID)
		if err != nil {
			return count, err
		}
		for index := range messages {
			messages[index].Status = common.MessageStatusInterrupted
			messages[index].ErrorClass = "interrupted"
			messages[index].ErrorMessage = "stream interrupted by process restart"
			messages[index].CompletedAt = &now
		}
		row.Status = common.StatusRecoveryRequired
		row.UpdatedAt = now
		if err := s.docs.InterruptTemporaryConversation(row, messages); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func (s *Store) DeleteConversation(ownerID, conversationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docs.DeleteTemporaryConversation(ownerID, conversationID)
}

func (s *Store) GetMessageImage(ownerID, conversationID, messageID, imageID string) (aiproxystate.TemporaryMessageImageRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(conversationID) == "" || strings.TrimSpace(messageID) == "" || strings.TrimSpace(imageID) == "" {
		return aiproxystate.TemporaryMessageImageRow{}, false, fmt.Errorf("invalid image reference")
	}
	conversation, found, err := s.docs.LoadTemporaryConversation(ownerID, conversationID)
	if err != nil {
		return aiproxystate.TemporaryMessageImageRow{}, false, err
	}
	if !found || !time.Now().UTC().Before(conversation.ExpiresAt) {
		return aiproxystate.TemporaryMessageImageRow{}, false, nil
	}
	return s.docs.GetTemporaryMessageImage(ownerID, conversationID, messageID, imageID)
}

func (s *Store) GetMessageAttachment(ownerID, conversationID, messageID, attachmentID string) (aiproxystate.TemporaryMessageAttachmentRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(conversationID) == "" || strings.TrimSpace(messageID) == "" || strings.TrimSpace(attachmentID) == "" {
		return aiproxystate.TemporaryMessageAttachmentRow{}, false, fmt.Errorf("invalid attachment reference")
	}
	conversation, found, err := s.docs.LoadTemporaryConversation(ownerID, conversationID)
	if err != nil {
		return aiproxystate.TemporaryMessageAttachmentRow{}, false, err
	}
	if !found || !time.Now().UTC().Before(conversation.ExpiresAt) {
		return aiproxystate.TemporaryMessageAttachmentRow{}, false, nil
	}
	return s.docs.GetTemporaryMessageAttachment(ownerID, conversationID, messageID, attachmentID)
}

func (s *Store) PurgeExpired(now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docs.PurgeExpiredTemporaryConversations(now)
}

func conversationView(row aiproxystate.TemporaryConversationRow) events.ConversationView {
	return events.ConversationView{
		ID:             row.ConversationID,
		Title:          row.Title,
		AccountDisplay: maskAccountID(row.AccountID),
		Provider:       defaultProvider(row.Provider),
		Model:          row.Model,
		ActualModel:    row.ActualModel,
		ThinkingEffort: row.ThinkingEffort,
		SystemPrompt:   row.SystemPrompt,
		Status:         row.Status,
		CreatedAt:      row.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:      row.UpdatedAt.UTC().Format(time.RFC3339),
		ExpiresAt:      row.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

func defaultProvider(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "chatgptweb"
}

func maskAccountID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return value
	}
	return value[:4] + "…" + value[len(value)-4:]
}

func messageView(row aiproxystate.TemporaryMessageRow) events.MessageView {
	view := events.MessageView{
		ID:           row.MessageID,
		Sequence:     row.Sequence,
		Role:         row.Role,
		Content:      row.Content,
		ActualModel:  row.ActualModel,
		Status:       row.Status,
		ErrorClass:   row.ErrorClass,
		ErrorMessage: row.ErrorMessage,
		CreatedAt:    row.CreatedAt.UTC().Format(time.RFC3339),
	}
	var images []temporaryImageMetadata
	if len(row.ImageMetadata) > 0 && json.Unmarshal(row.ImageMetadata, &images) == nil {
		view.Images = make([]events.MessageImageView, 0, len(images))
		for _, image := range images {
			if image.ID == "" || image.ContentType == "" || image.SizeBytes <= 0 {
				continue
			}
			view.Images = append(view.Images, events.MessageImageView{ID: image.ID, ContentType: image.ContentType, SizeBytes: image.SizeBytes})
		}
	}
	var attachments []temporaryAttachmentMetadata
	if len(row.AttachmentMetadata) > 0 && json.Unmarshal(row.AttachmentMetadata, &attachments) == nil {
		view.Attachments = make([]events.MessageAttachmentView, 0, len(attachments))
		for _, attachment := range attachments {
			if attachment.ID == "" || attachment.FileName == "" || attachment.ContentType == "" || attachment.SizeBytes <= 0 {
				continue
			}
			view.Attachments = append(view.Attachments, events.MessageAttachmentView{ID: attachment.ID, FileName: attachment.FileName, ContentType: attachment.ContentType, SizeBytes: attachment.SizeBytes})
		}
	}
	if row.CompletedAt != nil {
		view.CompletedAt = row.CompletedAt.UTC().Format(time.RFC3339)
	}
	if row.Role == common.RoleAssistant {
		view.TurnID = row.MessageID
	}
	return view
}

func truncateTitle(content string) string {
	content = strings.Join(strings.Fields(content), " ")
	if content == "" {
		return "新对话"
	}
	const maxRunes = 40
	if utf8.RuneCountInString(content) <= maxRunes {
		return content
	}
	runes := []rune(content)
	return string(runes[:maxRunes]) + "…"
}

func sanitizeError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 256 {
		return message[:256]
	}
	return message
}
