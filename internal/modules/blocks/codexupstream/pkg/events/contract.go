// Package events defines the native Codex Responses upstream Block contract.
package events

import (
	"encoding/json"
	"time"
)

const (
	TopicComplete   = "aetherrelay.codex.upstream.command.complete"
	TopicCompact    = "aetherrelay.codex.upstream.command.compact"
	TopicStart      = "aetherrelay.codex.upstream.command.start"
	TopicPull       = "aetherrelay.codex.upstream.command.pull"
	TopicCancel     = "aetherrelay.codex.upstream.command.cancel"
	TopicWSOpen     = "aetherrelay.codex.upstream.command.ws_open"
	TopicWSSend     = "aetherrelay.codex.upstream.command.ws_send"
	TopicWSPull     = "aetherrelay.codex.upstream.command.ws_pull"
	TopicWSClose    = "aetherrelay.codex.upstream.command.ws_close"
	TopicListModels = "aetherrelay.codex.upstream.command.list_models"
	TopicGetUsage   = "aetherrelay.codex.upstream.command.get_usage"
)

type Header struct {
	Name  string
	Value string
}

// HTTPAttempt is the opt-in archive projection of one upstream handshake.
// Headers are redacted unless explicitly requested otherwise. Body is copied
// only for full-content archival; proxyapi applies archive-only identity and
// attachment redaction. Neither projection may be printed as diagnostic data.
type HTTPAttempt struct {
	Request  HTTPRequestObservation
	Response HTTPResponseObservation
}

type HTTPRequestObservation struct {
	At        time.Time
	Method    string
	URL       string
	BodyBytes int
	// Body is the final wire payload; present only with ArchiveFullContent.
	Body    []byte
	Headers []Header
}

type HTTPResponseObservation struct {
	TransferEncoding string

	Observed      bool
	At            time.Time
	Status        int
	ContentLength int64
	DurationMS    int64
	Headers       []Header
}

// CodexFingerprint is the fully resolved, non-secret outbound identity set.
// Empty/off means that the proxy keeps its ordinary per-client session
// isolation and does not converge any account-level identifiers.
type CodexFingerprint struct {
	Mode                string
	InstallationID      string
	SessionID           string
	ThreadID            string
	TurnID              string
	WindowID            string
	TurnStartedAtUnixMS int64
}

type ErrorClass string

const (
	ErrorInvalidRequest ErrorClass = "invalid_request"
	ErrorModelNotFound  ErrorClass = "model_not_found"
	ErrorInvalidToken   ErrorClass = "invalid_token"
	ErrorRateLimit      ErrorClass = "rate_limit"
	ErrorTimeout        ErrorClass = "timeout"
	ErrorNetwork        ErrorClass = "network"
	ErrorUpstream       ErrorClass = "upstream"
	ErrorProtocol       ErrorClass = "protocol"
	// ErrorEndpoint identifies a route/CDN/proxy-level response (for example an
	// HTML 403) that must not be treated as an account credential failure.
	ErrorEndpoint ErrorClass = "endpoint"
)

// RateLimitObservation is the bounded projection of an upstream limit error.
// It never carries the raw upstream error body.
type RateLimitObservation struct {
	UsageLimited bool
	ResetAt      string
}

// SafeError is the bounded, redacted projection of a structured upstream
// client error. Raw response bodies never cross the codexupstream boundary.
type SafeError struct {
	Type    string
	Code    string
	Param   string
	Message string
}

// TurnMetadata is the CP-HDR-011 client projection: the turn level values the
// client owns plus its bounded scalar attributes. Session identity is
// deliberately absent — the Block always resolves that from the attempt's own
// identity, so a client value can never override it.
type TurnMetadata struct {
	TurnID          string
	RootTurnID      string
	TurnStartedAtMS int64
	// WindowNumber is the client declared window index (CP-HDR-010/CP-HDR-011).
	// The proxy owns the session part of the window identity, the number is the
	// client's when it declared one and 0 otherwise.
	WindowNumber int64
	// Attributes is a bounded JSON object of scalar values, already validated by
	// the inbound adapter (whitelisted keys, scalar types, size limits).
	Attributes json.RawMessage
}

// ClientIdentity is the bounded downstream identity of CP-HDR-003/004. The
// receiver normalizes both fields and falls back to the versioned profile
// whenever one is empty, oversized, or contains control characters.
type ClientIdentity struct {
	UserAgent  string
	Originator string
	// Version is populated only for a validated account-scoped observed profile.
	// It lets account-domain endpoints align their explicit client_version query
	// with the exact UA/originator pair selected by the account pool.
	Version string
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
	SessionHash      string
	BetaFeatures     string
	ResponsesLite    bool
	TurnState        string
	// ArchiveUnredactedHeaders is CP-OBS-009: it only reaches the archived
	// attempt observation, and its zero value keeps the credential redaction.
	ArchiveUnredactedHeaders bool
	ArchiveFullContent       bool
	// ClientIdentity is CP-HDR-003/004: either the scoped account selection or the
	// bounded downstream identity. Empty or invalid fields use the fallback profile.
	ClientIdentity ClientIdentity
	// TurnMetadata is the CP-HDR-011 client projection; identity fields are not
	// part of it.
	TurnMetadata TurnMetadata
	Fingerprint  CodexFingerprint
}
type CompleteResult struct {
	Body              []byte
	Headers           []Header
	Attempt           HTTPAttempt
	HTTPStatus        int
	ErrorClass        ErrorClass
	RetryAfterSeconds int
	RateLimit         RateLimitObservation
	SafeError         SafeError
}

type CompactCommand struct {
	AccessToken      string
	AccountIDHeader  string
	Proxy            string
	Body             []byte
	MaxResponseBytes int64
	SessionHash      string
	BetaFeatures     string
	ResponsesLite    bool
	TurnState        string
	// ArchiveUnredactedHeaders is CP-OBS-009: it only reaches the archived
	// attempt observation, and its zero value keeps the credential redaction.
	ArchiveUnredactedHeaders bool
	ArchiveFullContent       bool
	// ClientIdentity is CP-HDR-003/004: either the scoped account selection or the
	// bounded downstream identity. Empty or invalid fields use the fallback profile.
	ClientIdentity ClientIdentity
	// TurnMetadata is the CP-HDR-011 client projection; identity fields are not
	// part of it.
	TurnMetadata TurnMetadata
	Fingerprint  CodexFingerprint
}

type CompactResult struct {
	Body                        []byte
	Headers                     []Header
	Attempt                     HTTPAttempt
	HTTPStatus                  int
	ErrorClass                  ErrorClass
	RetryAfterSeconds           int
	RateLimit                   RateLimitObservation
	SafeError                   SafeError
	NativeCompactionUnsupported bool
}

type StartCommand struct {
	AccessToken     string
	AccountIDHeader string
	Proxy           string
	Body            []byte
	MaxLineBytes    int64
	SessionHash     string
	BetaFeatures    string
	ResponsesLite   bool
	TurnState       string
	// ArchiveUnredactedHeaders is CP-OBS-009: it only reaches the archived
	// attempt observation, and its zero value keeps the credential redaction.
	ArchiveUnredactedHeaders bool
	ArchiveFullContent       bool
	// ClientIdentity is CP-HDR-003/004: either the scoped account selection or the
	// bounded downstream identity. Empty or invalid fields use the fallback profile.
	ClientIdentity ClientIdentity
	// TurnMetadata is the CP-HDR-011 client projection; identity fields are not
	// part of it.
	TurnMetadata TurnMetadata
	Fingerprint  CodexFingerprint
}
type StartResult struct {
	StreamID          string
	Headers           []Header
	Attempt           HTTPAttempt
	HTTPStatus        int
	ErrorClass        ErrorClass
	RetryAfterSeconds int
	RateLimit         RateLimitObservation
	SafeError         SafeError
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
	SafeError         SafeError
}

type CancelCommand struct{ StreamID string }
type CancelResult struct{ Cancelled bool }

type WSOpenCommand struct {
	AccessToken     string
	AccountIDHeader string
	Proxy           string
	MaxMessageBytes int64
	SessionHash     string
	BetaFeatures    string
	ResponsesLite   bool
	TurnState       string
	// ArchiveUnredactedHeaders is CP-OBS-009: it only reaches the archived
	// attempt observation, and its zero value keeps the credential redaction.
	ArchiveUnredactedHeaders bool
	// ClientIdentity is CP-HDR-003/004: either the scoped account selection or the
	// bounded downstream identity. Empty or invalid fields use the fallback profile.
	ClientIdentity ClientIdentity
	// TurnMetadata is the CP-HDR-011 client projection; identity fields are not
	// part of it.
	TurnMetadata TurnMetadata
	Fingerprint  CodexFingerprint
}
type WSOpenResult struct {
	SessionID         string
	Headers           []Header
	Attempt           HTTPAttempt
	HTTPStatus        int
	ErrorClass        ErrorClass
	RetryAfterSeconds int
	RateLimit         RateLimitObservation
	SafeError         SafeError
}

type WSSendCommand struct {
	SessionID   string
	Payload     []byte
	Fingerprint CodexFingerprint
}
type WSSendResult struct{ Sent bool }

type WSPullCommand struct {
	SessionID     string
	TimeoutMillis int
}
type WSPullResult struct {
	Payload           []byte
	Done              bool
	ErrorClass        ErrorClass
	RetryAfterSeconds int
	RateLimit         RateLimitObservation
}

type WSCloseCommand struct{ SessionID string }
type WSCloseResult struct{ Closed bool }

// ListModelsCommand is intentionally credential-bearing only on the typed
// EventHub path. It is never returned to an HTTP caller.
type ListModelsCommand struct {
	AccessToken     string
	AccountIDHeader string
	Proxy           string
	ClientIdentity  ClientIdentity
}

// ModelDescriptor is the small, validated projection of a Codex model-list
// entry. Unknown upstream fields never cross the Block boundary.
type ModelDescriptor struct {
	ID        string
	CreatedAt int64
	OwnedBy   string
}

type ListModelsResult struct {
	Models []ModelDescriptor
	// ErrorClass is present alongside an EventHub error so account discovery can
	// distinguish an invalid credential from a transient transport failure.
	ErrorClass ErrorClass
}

// GetUsageCommand is credential-bearing only within the EventHub path. The
// result below is an allowlisted summary rather than the raw WHAM response.
type GetUsageCommand struct {
	AccessToken     string
	AccountIDHeader string
	Proxy           string
	ClientIdentity  ClientIdentity
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
