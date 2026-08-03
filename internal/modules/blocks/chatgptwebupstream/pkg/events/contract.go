// Package events defines the ChatGPT Web upstream owner's typed EventHub contract.
package events

import (
	"ai-proxy/internal/pkg/chatattachment"
	"ai-proxy/internal/pkg/chatgpttokenusage"
)

const (
	TopicGetUserInfo   = "aiproxy.chatgpt.webupstream.command.get_user_info"
	TopicListModels    = "aiproxy.chatgpt.webupstream.command.list_models"
	TopicGenerateImage = "aiproxy.chatgpt.webupstream.command.generate_image"
	TopicEditImage     = "aiproxy.chatgpt.webupstream.command.edit_image"
	TopicResumeImage   = "aiproxy.chatgpt.webupstream.command.resume_image"
	TopicCompleteText  = "aiproxy.chatgpt.webupstream.command.complete_text"
	TopicStartText     = "aiproxy.chatgpt.webupstream.command.start_text"
	TopicPullText      = "aiproxy.chatgpt.webupstream.command.pull_text"
	TopicCancelText    = "aiproxy.chatgpt.webupstream.command.cancel_text"
)

// ModelOperation is the restricted set of operations the upstream models
// enumeration may project into the effective catalog.
type ModelOperation string

const (
	ModelOperationChatCompletions  ModelOperation = "chat_completions"
	ModelOperationImageGenerations ModelOperation = "image_generations"
)

// ModelDescriptor is a constrained projection of an upstream model entry.
// Unknown upstream fields never enter this contract.
type ModelDescriptor struct {
	ID         string
	Operations []ModelOperation
	CreatedAt  int64
	OwnedBy    string
}

type ListModelsCommand struct {
	AccessToken string
	Proxy       string
}

type ListModelsResult struct {
	Models []ModelDescriptor
}

type ErrorClass string

const (
	ErrClassInvalidToken   ErrorClass = "invalid_token"
	ErrClassRateLimit      ErrorClass = "rate_limit"
	ErrClassContentPolicy  ErrorClass = "content_policy"
	ErrClassTLS            ErrorClass = "tls"
	ErrClassTimeout        ErrorClass = "timeout"
	ErrClassUpstream       ErrorClass = "upstream"
	ErrClassNotImplemented ErrorClass = "not_implemented"
)

type GetUserInfoCommand struct {
	AccessToken string
	Proxy       string
}

type GetUserInfoResult struct {
	Email     string
	PlanType  string
	Quota     int
	RestoreAt string
}

type GenerateImageCommand struct {
	AccessToken string
	Proxy       string
	Prompt      string
	Model       string
	Size        string
	Quality     string
}

type ImageOutput struct {
	Bytes          []byte
	B64JSON        string
	URL            string
	RevisedPrompt  string
	ConversationID string
}

type GenerateImageResult struct {
	Images         []ImageOutput
	ConversationID string
	Usage          *tokenusage.Usage
	// ErrorClass is populated alongside a partial result when the upstream
	// request failed. It lets the proxy owner make a safe retry decision
	// without parsing an upstream error string.
	ErrorClass ErrorClass
}

type EditImageCommand struct {
	AccessToken string
	Proxy       string
	Prompt      string
	Model       string
	Size        string
	Quality     string
	Images      [][]byte
}

type EditImageResult struct {
	Images         []ImageOutput
	ConversationID string
	Usage          *tokenusage.Usage
	ErrorClass     ErrorClass
}

type ResumeImageCommand struct {
	AccessToken      string
	Proxy            string
	ConversationID   string
	ExtraTimeoutSecs int
}

type ResumeImageResult struct {
	Images         []ImageOutput
	ConversationID string
	ErrorClass     ErrorClass
}

type TextMessage struct {
	Role    string
	Content string
	Images  [][]byte
	Files   []chatattachment.File
}

type CompleteTextCommand struct {
	AccessToken     string
	Proxy           string
	Model           string
	Messages        []TextMessage
	ThinkingEffort  string
	ConversationID  string
	ParentMessageID string
}

type CompleteTextResult struct {
	ConversationID     string
	AssistantMessageID string
	ActualModel        string
	Text               string
	ErrorClass         ErrorClass
}

type StartTextCommand struct {
	AccessToken     string
	Proxy           string
	Model           string
	Messages        []TextMessage
	ThinkingEffort  string
	ConversationID  string
	ParentMessageID string
	// TimeoutMillis bounds the full upstream request. It is supplied by the
	// temporary-chat owner and is not a client-controlled HTTP value.
	TimeoutMillis int
}

type StartTextResult struct{ StreamID string }

type PullTextCommand struct {
	StreamID      string
	TimeoutMillis int
}

type PullTextResult struct {
	Delta              string
	Done               bool
	ConversationID     string
	AssistantMessageID string
	ActualModel        string
	ErrorClass         ErrorClass
	ErrorMessage       string
}

type CancelTextCommand struct{ StreamID string }
type CancelTextResult struct{ Cancelled bool }
