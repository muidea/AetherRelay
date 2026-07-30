// Package events defines the native Codex Responses upstream Block contract.
package events

const (
	TopicComplete = "aiproxy.codex.upstream.command.complete"
	TopicStart    = "aiproxy.codex.upstream.command.start"
	TopicPull     = "aiproxy.codex.upstream.command.pull"
	TopicCancel   = "aiproxy.codex.upstream.command.cancel"
)

type Header struct {
	Name  string
	Value string
}

type ErrorClass string

const (
	ErrorInvalidToken ErrorClass = "invalid_token"
	ErrorRateLimit    ErrorClass = "rate_limit"
	ErrorTimeout      ErrorClass = "timeout"
	ErrorNetwork      ErrorClass = "network"
	ErrorUpstream     ErrorClass = "upstream"
	ErrorProtocol     ErrorClass = "protocol"
)

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
	ErrorClass        ErrorClass
	RetryAfterSeconds int
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
	ErrorClass        ErrorClass
	RetryAfterSeconds int
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
}

type CancelCommand struct{ StreamID string }
type CancelResult struct{ Cancelled bool }
