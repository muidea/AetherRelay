package aetherrelaycodex

import (
	"strings"
	"unicode/utf8"
)

// UpstreamError is the bounded error already extracted from an upstream error
// object. It contains no raw response body or headers and never controls retry.
type UpstreamError struct {
	UpstreamErrorType    string `json:"upstream_error_type,omitempty"`
	UpstreamErrorCode    string `json:"upstream_error_code,omitempty"`
	UpstreamErrorParam   string `json:"upstream_error_param,omitempty"`
	UpstreamErrorMessage string `json:"upstream_error_message,omitempty"`
}

func (e UpstreamError) Bounded() UpstreamError {
	e.UpstreamErrorType = ErrorToken(e.UpstreamErrorType, 64)
	e.UpstreamErrorCode = ErrorToken(e.UpstreamErrorCode, 96)
	e.UpstreamErrorParam = ErrorToken(e.UpstreamErrorParam, 256)
	e.UpstreamErrorMessage = ErrorText(e.UpstreamErrorMessage, 512)
	return e
}

// ErrorToken and ErrorText retain the existing Codex error projection policy.
func ErrorToken(value string, limit int) string {
	value = boundedErrorString(strings.TrimSpace(value), limit)
	for _, char := range value {
		if !(char == '_' || char == '-' || char == '.' || char == '[' || char == ']' || char >= '0' && char <= '9' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z') {
			return ""
		}
	}
	return value
}

func ErrorText(value string, limit int) string {
	value = strings.Map(func(char rune) rune {
		if char < 0x20 || char == 0x7f {
			return ' '
		}
		return char
	}, strings.TrimSpace(value))
	for _, marker := range []string{"bearer ", "authorization", "api_key", "access_token", "refresh_token", "cookie", "sk-", "sess-"} {
		if strings.Contains(strings.ToLower(value), marker) {
			return ""
		}
	}
	return strings.TrimSpace(boundedErrorString(value, limit))
}

func boundedErrorString(value string, limit int) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
