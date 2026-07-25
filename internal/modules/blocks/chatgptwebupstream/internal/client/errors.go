package client

import (
	"errors"
	"fmt"
	"net"
)

type ErrorClass string

const (
	InvalidToken ErrorClass = "invalid_token"
	RateLimit    ErrorClass = "rate_limit"
	Timeout      ErrorClass = "timeout"
	TLS          ErrorClass = "tls"
	Upstream     ErrorClass = "upstream"
)

type Error struct {
	Class      ErrorClass
	Operation  string
	StatusCode int
	Cause      error
}

func (e *Error) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s: HTTP %d (%s)", e.Operation, e.StatusCode, e.Class)
	}
	return fmt.Sprintf("%s: %s: %v", e.Operation, e.Class, e.Cause)
}
func (e *Error) Unwrap() error { return e.Cause }

func classifyStatus(operation string, status int) error {
	class := Upstream
	if status == 401 || status == 403 {
		class = InvalidToken
	}
	if status == 429 {
		class = RateLimit
	}
	return &Error{Class: class, Operation: operation, StatusCode: status}
}
func classifyTransport(operation string, cause error) error {
	class := TLS
	var networkError net.Error
	if errors.As(cause, &networkError) && networkError.Timeout() {
		class = Timeout
	}
	return &Error{Class: class, Operation: operation, Cause: cause}
}
