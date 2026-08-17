// Package codexresponses defines proxyapi's local native Codex Responses port.
package codexresponses

import (
	"context"
	"errors"
)

type Header struct{ Name, Value string }

type Request struct {
	Model         string
	Body          []byte
	SessionHash   string
	BetaFeatures  string
	ResponsesLite bool
	TurnState     string
}

type Result struct {
	Body    []byte
	Headers []Header
}
type Completion struct {
	Result Result
	Err    error
}

type StreamStart struct{ Headers []Header }

type WebsocketOpenRequest struct {
	Model         string
	SessionHash   string
	BetaFeatures  string
	ResponsesLite bool
	TurnState     string
}
type WebsocketOpenResult struct{ SessionID string }

type ErrorKind string

const (
	KindProviderUnavailable ErrorKind = "provider_unavailable"
	KindEndpoint            ErrorKind = "endpoint_error"
	KindInvalidRequest      ErrorKind = "invalid_request"
	KindInvalidToken        ErrorKind = "invalid_token"
	KindRateLimit           ErrorKind = "rate_limit"
	KindTimeout             ErrorKind = "timeout"
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
	QuotaExhausted    bool
	QuotaResetAt      string
	UpstreamType      string
	UpstreamCode      string
	UpstreamParam     string
	UpstreamMessage   string
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
	PullCodexWebsocket(context.Context, string) ([]byte, bool, error)
	CloseCodexWebsocket(context.Context, string)
}
