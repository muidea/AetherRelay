// Package codexresponses defines proxyapi's local native Codex Responses port.
package codexresponses

import (
	"context"
	"errors"
)

type Header struct{ Name, Value string }

type Request struct {
	Model string
	Body  []byte
}

type Result struct {
	Body    []byte
	Headers []Header
}

type StreamStart struct{ Headers []Header }

type ErrorKind string

const (
	KindProviderUnavailable ErrorKind = "provider_unavailable"
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
	RetryAfterSeconds int
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
func AsFailure(err error) (*Failure, bool) { var value *Failure; return value, errors.As(err, &value) }

type Executor interface {
	CompleteCodexResponses(context.Context, Request) (Result, error)
	StreamCodexResponses(context.Context, Request, func(StreamStart) error, func([]byte) error) error
}
