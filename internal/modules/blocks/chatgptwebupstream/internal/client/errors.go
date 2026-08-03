package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
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
	Detail     string
	Cause      error
}

func (e *Error) Error() string {
	if e.StatusCode != 0 {
		if e.Detail != "" {
			return fmt.Sprintf("%s: HTTP %d (%s): %s", e.Operation, e.StatusCode, e.Class, e.Detail)
		}
		return fmt.Sprintf("%s: HTTP %d (%s)", e.Operation, e.StatusCode, e.Class)
	}
	return fmt.Sprintf("%s: %s: %v", e.Operation, e.Class, e.Cause)
}

func classifyStatusResponse(operation string, status int, body io.Reader) error {
	classified := classifyStatus(operation, status).(*Error)
	if body == nil {
		return classified
	}
	data, err := io.ReadAll(io.LimitReader(body, 32<<10))
	if err != nil || len(data) == 0 {
		return classified
	}
	var payload any
	if json.Unmarshal(data, &payload) == nil {
		classified.Detail = upstreamErrorDetail(payload)
	}
	return classified
}

func upstreamErrorDetail(value any) string {
	var find func(any) string
	find = func(current any) string {
		switch item := current.(type) {
		case map[string]any:
			for _, key := range []string{"message", "detail", "code"} {
				if text, ok := item[key].(string); ok && strings.TrimSpace(text) != "" {
					return text
				}
			}
			for _, key := range []string{"error", "errors"} {
				if nested, ok := item[key]; ok {
					if text := find(nested); text != "" {
						return text
					}
				}
			}
		case []any:
			for _, nested := range item {
				if text := find(nested); text != "" {
					return text
				}
			}
		}
		return ""
	}
	detail := strings.Join(strings.Fields(find(value)), " ")
	if len(detail) > 512 {
		detail = detail[:512]
	}
	return detail
}
func (e *Error) Unwrap() error { return e.Cause }

func classifyStatus(operation string, status int) error {
	class := Upstream
	// A 401 is an authentication challenge. A 403 is deliberately not treated
	// as an invalid token: ChatGPT Web also uses it for policy, anti-abuse and
	// account-entitlement failures, none of which should permanently evict an
	// otherwise healthy credential.
	if status == 401 {
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
