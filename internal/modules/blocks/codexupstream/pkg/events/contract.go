// Package events defines the native Codex Responses upstream Block contract.
package events

const (
	TopicComplete   = "aetherrelay.codex.upstream.command.complete"
	TopicStart      = "aetherrelay.codex.upstream.command.start"
	TopicPull       = "aetherrelay.codex.upstream.command.pull"
	TopicCancel     = "aetherrelay.codex.upstream.command.cancel"
	TopicListModels = "aetherrelay.codex.upstream.command.list_models"
	TopicGetUsage   = "aetherrelay.codex.upstream.command.get_usage"
)

type Header struct {
	Name  string
	Value string
}

type ErrorClass string

const (
	ErrorInvalidRequest ErrorClass = "invalid_request"
	ErrorInvalidToken   ErrorClass = "invalid_token"
	ErrorRateLimit      ErrorClass = "rate_limit"
	ErrorTimeout        ErrorClass = "timeout"
	ErrorNetwork        ErrorClass = "network"
	ErrorUpstream       ErrorClass = "upstream"
	ErrorProtocol       ErrorClass = "protocol"
)

// RateLimitObservation is the bounded projection of an upstream limit error.
// It never carries the raw upstream error body.
type RateLimitObservation struct {
	UsageLimited bool
	ResetAt      string
}

// CompleteCommand and StartCommand deliberately carry bounded source-wire JSON
// as bytes. This preserves native Responses objects without map/any EventHub
// envelopes or a lossy proxy-side protocol translation.
type CompleteCommand struct {
	AccessToken      string
	AccountIDHeader  string
	Proxy            string
	Body             []byte
	MaxResponseBytes int64
}
type CompleteResult struct {
	Body              []byte
	Headers           []Header
	HTTPStatus        int
	ErrorClass        ErrorClass
	RetryAfterSeconds int
	RateLimit         RateLimitObservation
}

type StartCommand struct {
	AccessToken     string
	AccountIDHeader string
	Proxy           string
	Body            []byte
	MaxLineBytes    int64
}
type StartResult struct {
	StreamID          string
	Headers           []Header
	HTTPStatus        int
	ErrorClass        ErrorClass
	RetryAfterSeconds int
	RateLimit         RateLimitObservation
}

type PullCommand struct {
	StreamID      string
	TimeoutMillis int
}
type PullResult struct {
	Data              []byte
	Done              bool
	ErrorClass        ErrorClass
	RetryAfterSeconds int
	RateLimit         RateLimitObservation
}

type CancelCommand struct{ StreamID string }
type CancelResult struct{ Cancelled bool }

// ListModelsCommand is intentionally credential-bearing only on the typed
// EventHub path. It is never returned to an HTTP caller.
type ListModelsCommand struct {
	AccessToken     string
	AccountIDHeader string
	Proxy           string
}

// ModelDescriptor is the small, validated projection of a Codex model-list
// entry. Unknown upstream fields never cross the Block boundary.
type ModelDescriptor struct {
	ID        string
	CreatedAt int64
	OwnedBy   string
}

type ListModelsResult struct{ Models []ModelDescriptor }

// GetUsageCommand is credential-bearing only within the EventHub path. The
// result below is an allowlisted summary rather than the raw WHAM response.
type GetUsageCommand struct {
	AccessToken     string
	AccountIDHeader string
	Proxy           string
}

type UsageWindow struct {
	ID               string
	Label            string
	UsedPercent      float64
	UsedPercentKnown bool
	WindowSeconds    int
	ResetAt          string
	Allowed          bool
	AllowedKnown     bool
	LimitReached     bool
}

type GetUsageResult struct {
	PlanType   string
	Windows    []UsageWindow
	ErrorClass ErrorClass
}
