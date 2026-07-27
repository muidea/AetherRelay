package common

const UnitID = "/internal/modules/application/chatgpttemporarychat"

const (
	StatusIdle             = "idle"
	StatusStreaming        = "streaming"
	StatusRecoveryRequired = "recovery_required"
	StatusClosed           = "closed"

	MessageStatusStreaming   = "streaming"
	MessageStatusCompleted   = "completed"
	MessageStatusInterrupted = "interrupted"
	MessageStatusError       = "error"
	MessageStatusCancelled   = "cancelled"

	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)
