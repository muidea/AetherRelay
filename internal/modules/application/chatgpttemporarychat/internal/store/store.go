// Package store provides temporary-chat conversation and message persistence.
package store

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ai-proxy/internal/modules/application/chatgpttemporarychat/pkg/common"
	events "ai-proxy/internal/modules/application/chatgpttemporarychat/pkg/events"
	"ai-proxy/internal/pkg/aiproxystate"

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

func (s *Store) CreateConversation(ownerID, model, thinkingEffort, systemPrompt, accountID string) (events.ConversationView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ownerID = strings.TrimSpace(ownerID)
	model = strings.TrimSpace(model)
	accountID = strings.TrimSpace(accountID)
	if ownerID == "" || model == "" || accountID == "" {
		return events.ConversationView{}, fmt.Errorf("owner, model and account are required")
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
	Model                  string
	ThinkingEffort         string
	SystemPrompt           string
}

func (s *Store) StartTurn(ownerID, conversationID, content string) (TurnStart, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content = strings.TrimSpace(content)
	if content == "" {
		return TurnStart{}, fmt.Errorf("content is required")
	}
	if len(content) > s.cfg.MaxMessageBytes {
		return TurnStart{}, fmt.Errorf("message exceeds max size")
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
	user := aiproxystate.TemporaryMessageRow{
		OwnerID:        ownerID,
		ConversationID: conversationID,
		Sequence:       next,
		MessageID:      userID,
		Role:           common.RoleUser,
		Content:        content,
		Status:         common.MessageStatusStreaming,
		CreatedAt:      now,
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
		row.Title = truncateTitle(content)
	}
	row.Status = common.StatusStreaming
	row.UpdatedAt = now
	if err := s.docs.StartTemporaryTurn(row, user, assistant); err != nil {
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
		Model:                  row.Model,
		ThinkingEffort:         row.ThinkingEffort,
		SystemPrompt:           row.SystemPrompt,
	}, nil
}

type TurnComplete struct {
	Conversation events.ConversationView
	Message      events.MessageView
}

func (s *Store) CompleteTurn(ownerID, conversationID string, userSequence, assistantSequence int64, content, upstreamConversationID, assistantMessageID string, cancelled, interrupted, recoveryRequired bool, errorClass, errorMessage string) (TurnComplete, error) {
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
	if !cancelled && errorClass == "" && (upstreamConversationID == "" || assistantMessageID == "") {
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
		Model:          row.Model,
		ThinkingEffort: row.ThinkingEffort,
		SystemPrompt:   row.SystemPrompt,
		Status:         row.Status,
		CreatedAt:      row.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:      row.UpdatedAt.UTC().Format(time.RFC3339),
		ExpiresAt:      row.ExpiresAt.UTC().Format(time.RFC3339),
	}
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
		Status:       row.Status,
		ErrorClass:   row.ErrorClass,
		ErrorMessage: row.ErrorMessage,
		CreatedAt:    row.CreatedAt.UTC().Format(time.RFC3339),
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
