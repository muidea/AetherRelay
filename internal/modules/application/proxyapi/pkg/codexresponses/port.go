// Package codexresponses defines proxyapi's local native Codex Responses port.
package codexresponses

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type Header struct{ Name, Value string }

// HTTPAttempt is a credential-safe observation produced by codexupstream for
// interaction archival. Sensitive header values are already redacted at the
// owning Block boundary and are sanitized again by the HTTP adapter.
type HTTPAttempt struct {
	Request  HTTPRequestObservation
	Response HTTPResponseObservation
}

type HTTPRequestObservation struct {
	At        time.Time
	Method    string
	URL       string
	BodyBytes int
	Headers   []Header
}

type HTTPResponseObservation struct {
	Observed      bool
	At            time.Time
	Status        int
	ContentLength int64
	DurationMS    int64
	Headers       []Header
}

type PromptCacheKeySource string

const (
	PromptCacheKeyAbsent    PromptCacheKeySource = "absent"
	PromptCacheKeyExplicit  PromptCacheKeySource = "explicit"
	PromptCacheKeyGenerated PromptCacheKeySource = "generated"
)

// TurnStateSource is the bounded provenance enum for X-Codex-Turn-State
// (CP-HDR-023). The opaque value itself stays in the outbound header only and
// never enters diagnostics, archives or logs.
type TurnStateSource string

const (
	// TurnStateSourceAbsent means no turn state was sent for this attempt.
	TurnStateSourceAbsent TurnStateSource = "absent"
	// TurnStateSourceClient means the client supplied the value.
	TurnStateSourceClient TurnStateSource = "client"
	// TurnStateSourceSession means CP-HDR-022 filled the value from the record of
	// the same account-scoped downstream session.
	TurnStateSourceSession TurnStateSource = "session"
	// TurnStateSourceDefault means CP-HDR-022 filled the built-in or configured
	// fallback because the session had no observation.
	TurnStateSourceDefault TurnStateSource = "default"
)

// ValidTurnStateSource reports whether value is one of the bounded enum members.
func ValidTurnStateSource(value TurnStateSource) bool {
	switch value {
	case TurnStateSourceAbsent, TurnStateSourceClient, TurnStateSourceSession, TurnStateSourceDefault:
		return true
	default:
		return false
	}
}

// TurnStateFallback reports whether the value was filled by the proxy rather
// than supplied by the client. It drives the archived fallback flag.
func TurnStateFallback(source TurnStateSource) bool {
	return source == TurnStateSourceSession || source == TurnStateSourceDefault
}

// TurnMetadata is the CP-HDR-011 client projection passed to the executor; see
// the codexupstream events contract for the field ownership rules.
type TurnMetadata struct {
	TurnID          string
	RootTurnID      string
	TurnStartedAtMS int64
	Attributes      json.RawMessage
}

type Request struct {
	Diagnostics    Diagnostics
	AccountAttempt int
	Model          string
	Body           []byte
	SessionHash    string
	BetaFeatures   string
	ResponsesLite  bool
	TurnState      string
	// PromptCacheKeySource is a bounded provenance enum. The key itself remains
	// only in the request body and is never copied into diagnostics or logs.
	PromptCacheKeySource PromptCacheKeySource
	// TurnStateSource is set per attempt by the executor (CP-HDR-022) so the
	// diagnostic log can distinguish a supplied value from a filled one.
	TurnStateSource TurnStateSource
	// SessionScope is the CP-HDR-022 turn state record unit: the digest of an
	// explicitly declared downstream conversation, already namespaced by client
	// identity and model. It is empty when the client declared no conversation
	// identity, and an empty scope never records or replays a turn state.
	SessionScope string
	// ClientUserAgent and ClientOriginator are the bounded downstream identity of
	// CP-HDR-003/004. Empty values make the executor fall back to its versioned
	// profile; they never influence credentials or account selection.
	ClientUserAgent  string
	ClientOriginator string
	// TurnMetadata carries the bounded client projection; identity stays proxy-owned.
	TurnMetadata TurnMetadata
}

type Result struct {
	Body    []byte
	Headers []Header
	Attempt HTTPAttempt
	// TurnStateSource is the provenance of the turn state sent for the attempt
	// that produced this result.
	TurnStateSource TurnStateSource
}
type Completion struct {
	Result Result
	Err    error
}

// StreamStart is emitted when the first business SSE data arrives. The
// duration is measured from starting the upstream Codex stream, rather than
// from receiving its HTTP headers.
type StreamStart struct {
	Headers            []Header
	FirstEventDuration time.Duration
	Attempt            HTTPAttempt
	TurnStateSource    TurnStateSource
}

type WebsocketOpenRequest struct {
	Model         string
	SessionHash   string
	BetaFeatures  string
	ResponsesLite bool
	TurnState     string
	// SessionScope is the CP-HDR-022 record unit; see Request.SessionScope.
	SessionScope string
	// ClientUserAgent and ClientOriginator are the bounded downstream identity of
	// CP-HDR-003/004; see Request.ClientUserAgent.
	ClientUserAgent  string
	ClientOriginator string
	// TurnMetadata carries the bounded client projection; identity stays proxy-owned.
	TurnMetadata TurnMetadata
}
type WebsocketOpenResult struct {
	SessionID       string
	Attempt         HTTPAttempt
	TurnStateSource TurnStateSource
}

// WebsocketUpdate keeps upstream failure classification typed across the local
// proxyapi port. Payload may be present with Failure when the upstream emitted
// a terminal response.failed/error event.
type WebsocketUpdate struct {
	Payload []byte
	Done    bool
	Failure *Failure
}

type ErrorKind string

const (
	KindProviderUnavailable ErrorKind = "provider_unavailable"
	KindAuthentication      ErrorKind = "upstream_authentication_required"
	KindEndpoint            ErrorKind = "endpoint_error"
	KindInvalidRequest      ErrorKind = "invalid_request"
	KindModelNotFound       ErrorKind = "model_not_found"
	KindInvalidToken        ErrorKind = "invalid_token"
	KindRateLimit           ErrorKind = "rate_limit"
	KindTimeout             ErrorKind = "timeout"
	KindFirstEventTimeout   ErrorKind = "first_event_timeout"
	KindIdleTimeout         ErrorKind = "idle_timeout"
	KindStreamLifetime      ErrorKind = "stream_lifetime_timeout"
	KindNetwork             ErrorKind = "network"
	KindUpstream            ErrorKind = "upstream"
	KindProtocol            ErrorKind = "protocol"
	KindClientWrite         ErrorKind = "client_write"
	KindClientCanceled      ErrorKind = "client_canceled"
)

type Failure struct {
	Kind              ErrorKind
	HTTPStatus        int
	RetryAfterSeconds int
	UnavailableReason string
	Retryable         *bool
	QuotaExhausted    bool
	QuotaResetAt      string
	UpstreamType      string
	UpstreamCode      string
	UpstreamParam     string
	UpstreamMessage   string
	Attempt           HTTPAttempt
	Err               error
}

func (e *Failure) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return string(e.Kind)
}
func (e *Failure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
func NewFailure(kind ErrorKind, retryAfter int, err error) *Failure {
	return &Failure{Kind: kind, RetryAfterSeconds: retryAfter, Err: err}
}

func NewQuotaFailure(kind ErrorKind, retryAfter int, exhausted bool, resetAt string, err error) *Failure {
	return &Failure{Kind: kind, RetryAfterSeconds: retryAfter, QuotaExhausted: exhausted, QuotaResetAt: resetAt, Err: err}
}
func AsFailure(err error) (*Failure, bool) { var value *Failure; return value, errors.As(err, &value) }

type Executor interface {
	CompleteCodexResponses(context.Context, Request) (Result, error)
	CompleteCodexCompact(context.Context, Request) (Result, error)
	StartCodexCompact(context.Context, Request) (<-chan Completion, error)
	StreamCodexResponses(context.Context, Request, func(StreamStart) error, func([]byte) error) error
	OpenCodexWebsocket(context.Context, WebsocketOpenRequest) (WebsocketOpenResult, error)
	SendCodexWebsocket(context.Context, string, []byte) error
	PullCodexWebsocket(context.Context, string) (WebsocketUpdate, error)
	CloseCodexWebsocket(context.Context, string)
}
