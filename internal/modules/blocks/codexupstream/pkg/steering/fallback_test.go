package steering

import "testing"

// CP-PROBE-001/002: invoke the alternative exactly when native protocol evidence
// is insufficient, without repeating account-level refusals or conclusive tests.
func TestClientFallbackPolicy(t *testing.T) {
	base := NewEvidence()
	base.Done = true
	base.AuthState = "accepted"
	base.ModelAccessState = "accepted"
	base.ParentID = "resp_parent"
	base.CreateWriteCompleted = true
	base.SteerSent = true
	base.ErrorCode = "upstream_error"
	for _, tc := range []struct {
		name   string
		change func(*Evidence)
		want   string
	}{
		{"unknown", func(e *Evidence) {}, "native_protocol_inconclusive"},
		{"unsupported", func(e *Evidence) { e.Capability = Unsupported; e.ErrorCode = "unsupported_native_inflight_message" }, "native_protocol_unsupported"},
		{"timeout", func(e *Evidence) { e.ErrorCode = "timeout" }, "native_protocol_inconclusive"},
		{"network", func(e *Evidence) { e.ErrorCode = "network" }, "native_protocol_inconclusive"},
		{"supported", func(e *Evidence) { e.Capability = Supported; e.ErrorCode = "successor_failed" }, ""},
		{"pending", func(e *Evidence) { e.Capability = Pending }, ""},
		{"running", func(e *Evidence) { e.Done = false }, ""},
		{"no_parent", func(e *Evidence) { e.ParentID = "" }, ""},
		{"no_steer", func(e *Evidence) { e.SteerSent = false }, ""},
		{"write_failed", func(e *Evidence) { e.CreateWriteCompleted = false }, ""},
		{"auth", func(e *Evidence) { e.AuthState = "rejected" }, ""},
		{"model", func(e *Evidence) { e.ModelAccessState = "rejected" }, ""},
		{"canceled", func(e *Evidence) { e.ErrorCode = "canceled" }, ""},
		{"changed", func(e *Evidence) { e.ErrorCode = "account_changed" }, ""},
		{"configuration", func(e *Evidence) { e.ErrorCode = "configuration_error" }, ""},
		{"quota", func(e *Evidence) { e.ErrorCode = "usage_limit_reached" }, ""},
		{"limit", func(e *Evidence) { e.ErrorCode = "receive_limit" }, ""},
		{"token", func(e *Evidence) { e.ErrorCode = "invalid_token" }, ""},
		{"401", func(e *Evidence) { e.UpstreamError = &UpstreamError{Status: 401} }, ""},
		{"403", func(e *Evidence) { e.UpstreamError = &UpstreamError{Status: 403} }, ""},
		{"429", func(e *Evidence) { e.UpstreamError = &UpstreamError{Status: 429} }, ""},
		{"nested_rate", func(e *Evidence) { e.UpstreamError = &UpstreamError{Code: "rate_limit_exceeded"} }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := base.Clone()
			tc.change(&e)
			if got := ClientFallbackReason(e); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
