// Package events defines the ImageTask Module's typed EventHub contract.
package events

import "ai-proxy/internal/pkg/chatgpttokenusage"

const (
	TopicSubmitGeneration = "imagetask/submit_generation"
	TopicSubmitEdit       = "imagetask/submit_edit"
	TopicList             = "imagetask/list"
	TopicResumePoll       = "imagetask/resume_poll"
	TopicRetryGeneration  = "imagetask/retry_generation"
	TopicCancel           = "imagetask/cancel"
	TopicDelete           = "imagetask/delete"
)

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSuccess   = "success"
	StatusError     = "error"
	StatusCancelled = "cancelled"
)

type TaskView struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	Mode           string            `json:"mode"`
	Model          string            `json:"model,omitempty"`
	Provider       string            `json:"provider,omitempty"`
	Prompt         string            `json:"prompt,omitempty"`
	Size           string            `json:"size,omitempty"`
	Quality        string            `json:"quality,omitempty"`
	CreatedAt      string            `json:"created_at"`
	UpdatedAt      string            `json:"updated_at"`
	ConversationID string            `json:"conversation_id,omitempty"`
	Data           []ImageData       `json:"data,omitempty"`
	Error          string            `json:"error,omitempty"`
	Progress       string            `json:"progress,omitempty"`
	ElapsedSecs    float64           `json:"elapsed_secs,omitempty"`
	DurationMs     int64             `json:"duration_ms,omitempty"`
	Usage          *tokenusage.Usage `json:"usage,omitempty"`
}

type ImageData struct {
	B64JSON       string `json:"b64_json,omitempty"`
	URL           string `json:"url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

type SubmitGenerationCommand struct {
	OwnerID      string
	ClientTaskID string
	Prompt       string
	Model        string
	Size         string
	Quality      string
	BaseURL      string
}

type SubmitEditCommand struct {
	OwnerID      string
	ClientTaskID string
	Prompt       string
	Model        string
	Size         string
	Quality      string
	BaseURL      string
	Images       []string
}

type SubmitResult struct{ Task TaskView }

type ListCommand struct {
	OwnerID string
	TaskIDs []string
}

type ListResult struct {
	Items      []TaskView `json:"items"`
	MissingIDs []string   `json:"missing_ids,omitempty"`
}

type ResumePollCommand struct {
	OwnerID          string
	TaskID           string
	ExtraTimeoutSecs int
}

type ResumePollResult struct{ Task TaskView }

// RetryGenerationCommand restarts a generation that failed before ChatGPT
// created a conversation. It is distinct from ResumePollCommand: without a
// conversation ID there is no upstream job to poll.
type RetryGenerationCommand struct {
	OwnerID string
	TaskID  string
	BaseURL string
}

type RetryGenerationResult struct{ Task TaskView }

type CancelCommand struct {
	OwnerID string
	TaskID  string
}

type CancelResult struct{ Task TaskView }

type DeleteCommand struct {
	OwnerID string
	TaskID  string
}

type DeleteResult struct {
	Deleted bool `json:"deleted"`
}
