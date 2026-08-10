// Package events defines the temporary chat Module's typed EventHub contract.
package events

import (
	"aetherrelay/internal/modules/application/chatgpttemporarychat/pkg/common"
	"aetherrelay/internal/pkg/chatattachment"
)

const (
	TopicCreate        = "aetherrelay.chatgpt.temporarychat.command.create"
	TopicList          = "aetherrelay.chatgpt.temporarychat.command.list"
	TopicGet           = "aetherrelay.chatgpt.temporarychat.command.get"
	TopicStartTurn     = "aetherrelay.chatgpt.temporarychat.command.start_turn"
	TopicPullTurn      = "aetherrelay.chatgpt.temporarychat.command.pull_turn"
	TopicCancelTurn    = "aetherrelay.chatgpt.temporarychat.command.cancel_turn"
	TopicDelete        = "aetherrelay.chatgpt.temporarychat.command.delete"
	TopicGetImage      = "aetherrelay.chatgpt.temporarychat.command.get_image"
	TopicGetAttachment = "aetherrelay.chatgpt.temporarychat.command.get_attachment"
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
	Provider       string `json:"provider"`
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
	ID           string                  `json:"id"`
	Sequence     int64                   `json:"sequence"`
	Role         string                  `json:"role"`
	Content      string                  `json:"content"`
	Images       []MessageImageView      `json:"images,omitempty"`
	Attachments  []MessageAttachmentView `json:"attachments,omitempty"`
	ActualModel  string                  `json:"actual_model,omitempty"`
	Status       string                  `json:"status"`
	ErrorClass   string                  `json:"error_class,omitempty"`
	ErrorMessage string                  `json:"error_message,omitempty"`
	CreatedAt    string                  `json:"created_at"`
	CompletedAt  string                  `json:"completed_at,omitempty"`
	TurnID       string                  `json:"turn_id,omitempty"`
}

type MessageAttachmentView struct {
	ID          string `json:"id"`
	FileName    string `json:"file_name"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

// MessageImageView deliberately contains only display metadata. Image bytes
// are served through an owner-scoped Admin endpoint, never embedded in a
// conversation JSON response or browser storage.
type MessageImageView struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

// ImageInput carries an already validated attachment through the typed
// temporary-chat boundary. It has no JSON tag because HTTP adapters decode
// data URIs or multipart uploads before invoking this contract.
type ImageInput struct {
	Bytes       []byte
	ContentType string
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
	// WebSearch requests one forced web-search turn. It is intentionally
	// ephemeral: the conversation remains server-persisted, but no hidden
	// browser search state is reused by later turns.
	WebSearch   bool
	Images      []ImageInput
	Attachments []chatattachment.File
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

type GetMessageImageCommand struct {
	OwnerID        string
	ConversationID string
	MessageID      string
	ImageID        string
}

type GetMessageImageResult struct {
	Bytes       []byte
	ContentType string
}

type GetMessageAttachmentCommand struct {
	OwnerID        string
	ConversationID string
	MessageID      string
	AttachmentID   string
}

type GetMessageAttachmentResult struct {
	Bytes       []byte
	FileName    string
	ContentType string
}
