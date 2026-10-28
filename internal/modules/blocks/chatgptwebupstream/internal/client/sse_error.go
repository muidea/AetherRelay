package client

import (
	"encoding/json"
	"strings"
)

// parseSSEError only inspects explicit protocol error envelopes, never message
// content. HTTP 200 does not imply success: Web conversations can report a
// usage_limit after first sending their conversation ID.
func parseSSEError(operation, payload string) error {
	var value map[string]any
	if json.Unmarshal([]byte(payload), &value) != nil {
		return nil
	}
	if failure := sseEnvelopeError(operation, value); failure != nil {
		return failure
	}
	// v1 delta frames can wrap the same envelope in a root replacement. Do
	// not recursively search user/assistant content for error-looking keys.
	if path, _ := value["p"].(string); path == "" {
		if op, _ := value["o"].(string); op == "add" || op == "replace" {
			if nested, ok := value["v"].(map[string]any); ok {
				return sseEnvelopeError(operation, nested)
			}
		}
	}
	return nil
}

func sseEnvelopeError(operation string, value map[string]any) error {
	code, _ := value["error_code"].(string)
	code = strings.TrimSpace(code)
	raw := value["error"]
	explicit := raw != nil
	switch v := raw.(type) {
	case string:
		explicit = strings.TrimSpace(v) != ""
	case bool:
		explicit = v
	}
	kind, _ := value["type"].(string)
	if code == "" && !explicit && kind != "error" {
		return nil
	}
	if nested, ok := raw.(map[string]any); ok && code == "" {
		code, _ = nested["error_code"].(string)
		if code == "" {
			code, _ = nested["code"].(string)
		}
	}
	if code == "" && kind == "error" {
		code, _ = value["code"].(string)
	}
	detail, _ := raw.(string)
	if detail == "" {
		detail = upstreamErrorDetail(raw)
	}
	if detail == "" {
		detail = upstreamErrorDetail(value)
	}
	if detail == "" {
		detail = "upstream SSE error"
	}
	class := Upstream
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "usage_limit", "rate_limit", "rate_limit_exceeded", "too_many_requests", "quota_exceeded", "insufficient_quota":
		class = RateLimit
	}
	// An SSE error occurs after submission. Unknown codes, including auth-like
	// strings, are not an HTTP authentication challenge and must not evict a
	// credential or authorize replaying this conversation.
	return &Error{Class: class, Operation: operation, Code: bounded(strings.TrimSpace(code), 128), Detail: bounded(strings.TrimSpace(detail), 512)}
}
