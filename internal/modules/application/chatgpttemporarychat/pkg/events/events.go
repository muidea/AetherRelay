// Package events defines the temporary chat Module's typed EventHub contract.
package events

import "ai-proxy/internal/modules/application/chatgpttemporarychat/pkg/common"

const (
	TopicCreate     = "aiproxy.chatgpt.temporarychat.command.create"
	TopicList       = "aiproxy.chatgpt.temporarychat.command.list"
	TopicGet        = "aiproxy.chatgpt.temporarychat.command.get"
	TopicStartTurn  = "aiproxy.chatgpt.temporarychat.command.start_turn"
	TopicPullTurn   = "aiproxy.chatgpt.temporarychat.command.pull_turn"
	TopicCancelTurn = "aiproxy.chatgpt.temporarychat.command.cancel_turn"
	TopicDelete     = "aiproxy.chatgpt.temporarychat.command.delete"
)

const (
	StatusIdle             = common.StatusIdle
	StatusStreaming        = common.StatusStreaming
	StatusRecoveryRequired = common.StatusRecoveryRequired
	StatusClosed           = common.StatusClosed

	MessageStatusStreaming   = common.MessageStatusStreaming
	MessageStatusCompleted   = common.MessageStatusCompleted
	MessageStatusInterrupted = common.MessageStatusInterrupted
	MessageStatusError       = common.MessageStatusError
	MessageStatusCancelled   = common.MessageStatusCancelled
)

// ConversationView is the bounded Admin-facing conversation projection.
// It never includes tokens, upstream stream IDs, or raw SSE.
type ConversationView struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// AccountID is internal-only. Admin clients receive AccountDisplay, never
	// the persistent account identifier.
	AccountID      string `json:"-"`
	AccountDisplay string `json:"account_display,omitempty"`
	// Model is the model requested when the conversation was created.
	Model string `json:"model"`
	// ActualModel is supplied only when the upstream SSE explicitly reports a
	// resolved model slug. An empty value means the upstream did not disclose it.
	ActualModel    string `json:"actual_model,omitempty"`
	ThinkingEffort string `json:"thinking_effort,omitempty"`
	// SystemPrompt stays in the owner store and is submitted only on the first
	// upstream turn; it is not part of the Admin read projection.
	SystemPrompt string `json:"-"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	ExpiresAt    string `json:"expires_at"`
}

// MessageView is one bounded message projection for Admin clients.
type MessageView struct {
	ID           string `json:"id"`
	Sequence     int64  `json:"sequence"`
	Role         string `json:"role"`
	Content      string `json:"content"`
	ActualModel  string `json:"actual_model,omitempty"`
	Status       string `json:"status"`
	ErrorClass   string `json:"error_class,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	CreatedAt    string `json:"created_at"`
	CompletedAt  string `json:"completed_at,omitempty"`
	TurnID       string `json:"turn_id,omitempty"`
}

type CreateConversationCommand struct {
	OwnerID        string
	Model          string
	ThinkingEffort string
	SystemPrompt   string
}

type ConversationResult struct {
	Conversation ConversationView `json:"conversation"`
}

type ListConversationsCommand struct {
	OwnerID string
	Cursor  string
	Limit   int
}

type ListConversationsResult struct {
	Items      []ConversationView `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type GetConversationCommand struct {
	OwnerID        string
	ConversationID string
	BeforeSequence *int64
	Limit          int
}

type ConversationDetailResult struct {
	Conversation ConversationView `json:"conversation"`
	Messages     []MessageView    `json:"messages"`
	HasMore      bool             `json:"has_more"`
}

type StartTurnCommand struct {
	OwnerID        string
	ConversationID string
	Content        string
}

type StartTurnResult struct {
	TurnID           string           `json:"turn_id"`
	Conversation     ConversationView `json:"conversation"`
	UserMessage      MessageView      `json:"user_message"`
	AssistantMessage MessageView      `json:"assistant_message"`
}

type PullTurnCommand struct {
	OwnerID        string
	ConversationID string
	TurnID         string
	TimeoutMillis  int
}

type PullTurnResult struct {
	Delta        string       `json:"delta,omitempty"`
	ActualModel  string       `json:"actual_model,omitempty"`
	Done         bool         `json:"done"`
	Message      *MessageView `json:"message,omitempty"`
	ErrorClass   string       `json:"error_class,omitempty"`
	ErrorMessage string       `json:"error_message,omitempty"`
}

type CancelTurnCommand struct {
	OwnerID        string
	ConversationID string
	TurnID         string
}

type CancelTurnResult struct {
	Message MessageView `json:"message"`
}

type DeleteConversationCommand struct {
	OwnerID        string
	ConversationID string
}

type DeleteConversationResult struct {
	Deleted bool `json:"deleted"`
}
