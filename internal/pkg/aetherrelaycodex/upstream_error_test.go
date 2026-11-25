package aetherrelaycodex

import (
	"strings"
	"testing"
)

func TestExtractUpstreamErrorEnvelopesAndRedaction(t *testing.T) {
	msg := "Invalid schema for response_format 'workorch_runtime_candidate': schema must have type 'object' and not have 'oneOf'/'anyOf'/'allOf'/'enum'/'not' at the top level. Found 'anyOf'."
	got := ExtractUpstreamError([]byte(`{"error":{"type":"invalid_request_error","code":"invalid_json_schema","param":"text.format.schema","message":"` + msg + `"}}`))
	if got.UpstreamErrorType != "invalid_request_error" || got.UpstreamErrorCode != "invalid_json_schema" || got.UpstreamErrorParam != "text.format.schema" || got.UpstreamErrorMessage != msg {
		t.Fatalf("unexpected OpenAI/DeepSeek extraction: %+v", got)
	}

	numeric := ExtractUpstreamError([]byte(`{"error":{"type":"invalid_request_error","code":400,"param":null,"message":"bad payload"}}`))
	if numeric.UpstreamErrorType != "invalid_request_error" || numeric.UpstreamErrorCode != "400" || numeric.UpstreamErrorParam != "" || numeric.UpstreamErrorMessage != "bad payload" {
		t.Fatalf("unexpected numeric code extraction: %+v", numeric)
	}

	anthropic := ExtractUpstreamError([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad anthropic input"}}`))
	if anthropic.UpstreamErrorType != "invalid_request_error" || anthropic.UpstreamErrorCode != "" || anthropic.UpstreamErrorMessage != "bad anthropic input" {
		t.Fatalf("unexpected Anthropic extraction: %+v", anthropic)
	}

	strErr := ExtractUpstreamError([]byte(`{"error":"bad gateway"}`))
	if strErr.UpstreamErrorMessage != "bad gateway" || strErr.UpstreamErrorType != "" || strErr.UpstreamErrorCode != "" {
		t.Fatalf("unexpected string error extraction: %+v", strErr)
	}

	wrapped := ExtractUpstreamError([]byte(`{"response":{"error":{"type":"server_error","code":"upstream_down","message":"try again"}}}`))
	if wrapped.UpstreamErrorType != "server_error" || wrapped.UpstreamErrorCode != "upstream_down" || wrapped.UpstreamErrorMessage != "try again" {
		t.Fatalf("unexpected wrapped response extraction: %+v", wrapped)
	}

	detail := ExtractUpstreamError([]byte(`{"detail":"Invalid input role"}`))
	if detail.UpstreamErrorMessage != "Invalid input role" {
		t.Fatalf("unexpected detail extraction: %+v", detail)
	}

	topLevel := ExtractUpstreamError([]byte(`{"type":"rate_limit_error","code":"rate_limited","param":"model","message":"slow down"}`))
	if topLevel.UpstreamErrorType != "rate_limit_error" || topLevel.UpstreamErrorCode != "rate_limited" || topLevel.UpstreamErrorParam != "model" || topLevel.UpstreamErrorMessage != "slow down" {
		t.Fatalf("unexpected top-level extraction: %+v", topLevel)
	}

	for _, raw := range []string{"", "<html>502 Bad Gateway</html>", "plain text error", `{"ok":true}`} {
		if nonJSON := ExtractUpstreamError([]byte(raw)); !nonJSON.Empty() {
			t.Fatalf("expected empty UpstreamError for %q, got %+v", raw, nonJSON)
		}
	}

	for _, msg := range []string{
		"Authorization: Bearer secret-token",
		"invalid sk-1234567890abcdef",
		"missing api_key in header",
		"task-123 failed validation",
	} {
		preserved := ExtractUpstreamError([]byte(`{"error":{"message":"` + msg + `"}}`))
		if preserved.UpstreamErrorMessage != msg {
			t.Fatalf("expected message %q to be preserved, got %q", msg, preserved.UpstreamErrorMessage)
		}
	}

	bounded := ExtractUpstreamError([]byte(`{
		"error": {
			"type": "` + strings.Repeat("t", 80) + `",
			"code": "` + strings.Repeat("c", 120) + `",
			"param": "` + strings.Repeat("p", 300) + `",
			"message": "` + strings.Repeat("m", 600) + `"
		}
	}`))
	if len(bounded.UpstreamErrorType) != 64 || len(bounded.UpstreamErrorCode) != 96 || len(bounded.UpstreamErrorParam) != 256 || len(bounded.UpstreamErrorMessage) != 512 {
		t.Fatalf("unexpected bounds: type=%d code=%d param=%d msg=%d",
			len(bounded.UpstreamErrorType), len(bounded.UpstreamErrorCode), len(bounded.UpstreamErrorParam), len(bounded.UpstreamErrorMessage))
	}
}
