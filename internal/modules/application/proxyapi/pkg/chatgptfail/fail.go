// Package chatgptfail defines proxyapi-local typed failures for ChatGPT Web
// text and image execution. HTTP adapters classify with errors.As and never
// import upstream owner event types or parse error strings for outcome.
package chatgptfail

import "fmt"

// Kind is the stable failure class projected into usage ErrorCode and metrics.
type Kind string

const (
	KindClientCanceled      Kind = "client_canceled"
	KindClientWrite         Kind = "client_write"
	KindProviderUnavailable Kind = "provider_unavailable"
	KindInvalidToken        Kind = "invalid_token"
	KindRateLimit           Kind = "rate_limit"
	KindContentPolicy       Kind = "content_policy"
	KindTLS                 Kind = "tls"
	KindTimeout             Kind = "timeout"
	KindUpstream            Kind = "upstream"
	KindInternal            Kind = "internal"
)

// Failure is a typed proxy-local error. Callers use errors.As for classification.
type Failure struct {
	Kind  Kind
	Cause error
}

func (f *Failure) Error() string {
	if f == nil {
		return ""
	}
	if f.Cause != nil {
		return fmt.Sprintf("chatgpt %s: %v", f.Kind, f.Cause)
	}
	return fmt.Sprintf("chatgpt %s", f.Kind)
}

func (f *Failure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Cause
}

// New builds a Failure with the given kind. Cause may be nil.
func New(kind Kind, cause error) *Failure {
	return &Failure{Kind: kind, Cause: cause}
}

// Outcome maps a failure kind to the usage/metrics outcome label.
// A nil failure is success.
func Outcome(kind Kind) string {
	switch kind {
	case "":
		return "success"
	case KindClientCanceled:
		return "client_canceled"
	case KindClientWrite:
		return "client_write"
	case KindProviderUnavailable, KindInvalidToken, KindRateLimit, KindContentPolicy, KindTLS, KindTimeout, KindUpstream:
		return "upstream_failed"
	case KindInternal:
		return "error"
	default:
		return "error"
	}
}

// ErrorCode is the usage ErrorCode for a failure kind.
func ErrorCode(kind Kind) string {
	switch kind {
	case "":
		return ""
	case KindInternal:
		return "proxy_internal_error"
	default:
		return string(kind)
	}
}

// CountUpstream reports whether midflight stream failures of this kind should
// increment provider upstream error metrics.
func CountUpstream(kind Kind) bool {
	switch kind {
	case KindInvalidToken, KindRateLimit, KindTLS, KindTimeout, KindUpstream:
		return true
	default:
		return false
	}
}

// FromUpstreamClass maps chatgptwebupstream ErrorClass strings into local kinds.
// Unknown classes become KindUpstream rather than internal, so local bugs stay distinct.
func FromUpstreamClass(class string) Kind {
	switch class {
	case "invalid_token":
		return KindInvalidToken
	case "rate_limit":
		return KindRateLimit
	case "content_policy":
		return KindContentPolicy
	case "tls":
		return KindTLS
	case "timeout":
		return KindTimeout
	case "not_implemented":
		return KindUpstream
	case "upstream", "":
		return KindUpstream
	default:
		return KindUpstream
	}
}
