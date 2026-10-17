package steering

import (
	"encoding/json"
	"errors"
	"testing"
)

func clientParent(t *testing.T) *ClientMachine {
	t.Helper()
	m := NewClientMachine()
	m.WriteAttempt("response.create")
	m.WriteResult("response.create", nil)
	if m.Observe([]byte(`{"type":"response.created","response":{"id":"resp_parent"}}`)) || !m.InputQueued || m.Capability != Unknown || m.SteerSent || m.ContinuationSent {
		t.Fatal(m.Evidence)
	}
	return m
}
func clientReady(t *testing.T) *ClientMachine {
	t.Helper()
	m := clientParent(t)
	if !m.Observe([]byte(`{"type":"response.completed","response":{"id":"resp_parent","status":"completed","usage":{"input_tokens":3,"output_tokens":4}}}`)) || m.Capability != Unknown {
		t.Fatal(m.Evidence)
	}
	return m
}
func clientCommitted(t *testing.T) *ClientMachine {
	t.Helper()
	m := clientReady(t)
	m.WriteAttempt("response.create")
	m.WriteResult("response.create", nil)
	m.Observe([]byte(`{"type":"response.created","response":{"id":"resp_next","previous_response_id":"resp_parent"}}`))
	if m.Capability != Supported || m.SteerSent || m.SteerWriteCompleted || !m.ContinuationWriteCompleted {
		t.Fatal(m.Evidence)
	}
	return m
}

// CP-PROBE-001: official-style create preserves model, instructions and incremental input.
func TestClientContinuationWire(t *testing.T) {
	var wire struct {
		Type, Model, Instructions string
		Store                     bool
		Parent                    string `json:"previous_response_id"`
		Input                     []struct {
			Role    string
			Content []struct{ Type, Text string }
		}
	}
	if err := json.Unmarshal(Continue("gpt-test", "resp_parent"), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Type != "response.create" || wire.Parent != "resp_parent" || wire.Model != "gpt-test" || wire.Instructions != Instructions || wire.Store || len(wire.Input) != 1 || wire.Input[0].Role != "user" || wire.Input[0].Content[0].Text != SteerInput {
		t.Fatal(wire)
	}
}
func TestClientQueueDrainAndIndependentUsage(t *testing.T) {
	m := clientParent(t)
	for _, s := range []string{`{"type":"response.in_progress"}`, `{"type":"response.output_text.delta","response_id":"resp_parent","delta":"1"}`, `{"type":"codex.response.metadata","usage":{"output_tokens":999}}`} {
		if m.Observe([]byte(s)) || m.Capability != Unknown || m.ContinuationSent {
			t.Fatal(m.Evidence)
		}
	}
	m = clientCommitted(t)
	m.Observe([]byte(`{"type":"response.completed","response":{"id":"resp_next","usage":{"input_tokens":5,"output_tokens":2}}}`))
	if !m.Done || m.ExecutionOutcome != "completed" || *m.ParentUsage.OutputTokens != 4 || *m.SuccessorUsage.OutputTokens != 2 || m.SteerID != "" || m.CapabilityTarget != ClientTarget {
		t.Fatal(m.Evidence)
	}
	before := m.Evidence.Clone()
	m.Observe([]byte(`{"type":"error","error":{"code":"steering_not_supported"}}`))
	m.Reject("steering_not_supported", 400)
	if m.Capability != before.Capability || m.ExecutionOutcome != before.ExecutionOutcome {
		t.Fatal(m.Evidence)
	}
}
func TestClientRejectScopeAndPositiveEvidence(t *testing.T) {
	for _, code := range []string{"unsupported_native_inflight_message", "steering_not_supported", "unknown_refusal", "invalid_request_error"} {
		for _, phase := range []string{"drain", "continuation", "committed"} {
			t.Run(code+"/"+phase, func(t *testing.T) {
				m := clientParent(t)
				if phase == "continuation" {
					m = clientReady(t)
					m.WriteAttempt("response.create")
					m.WriteResult("response.create", nil)
				}
				if phase == "committed" {
					m = clientCommitted(t)
				}
				m.Observe([]byte(`{"type":"error","error":{"code":"` + code + `"}}`))
				want := Unknown
				if phase == "committed" {
					want = Supported
				} else if phase == "continuation" && (code == "unsupported_native_inflight_message" || code == "steering_not_supported") {
					want = Unsupported
				}
				if !m.Done || m.Capability != want || m.UpstreamError == nil || m.UpstreamError.Code != code {
					t.Fatal(m.Evidence)
				}
			})
		}
	}
}

// CP-PROBE-001/002: required inputs, parent failure and correlation block continuation.
func TestClientPendingParentFailureAndCorrelation(t *testing.T) {
	for _, tc := range []struct{ name, event, capability, outcome string }{
		{"tool", `{"type":"response.completed","response":{"id":"resp_parent","output":[{"type":"function_call"}]}}`, Pending, "waiting_for_required_input"},
		{"approval", `{"type":"response.completed","response":{"id":"resp_parent","output":[{"type":"mcp_approval_request"}]}}`, Pending, "waiting_for_required_input"},
		{"failed", `{"type":"response.failed","response":{"id":"resp_parent","error":{"code":"upstream_error"}}}`, Unknown, "upstream_error"},
		{"incomplete", `{"type":"response.incomplete","response":{"id":"resp_parent","incomplete_details":{"reason":"steered"}}}`, Unknown, "upstream_error"},
		{"foreign", `{"type":"response.completed","response":{"id":"resp_other"}}`, Unknown, "correlation_error"},
		{"premature", `{"type":"response.created","response":{"id":"resp_next"}}`, Unknown, "correlation_error"},
		{"unknown", `{"type":"response.future_business_event"}`, Unknown, "unknown_business_event"},
		{"invalid", `{"type":`, Unknown, "protocol_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := clientParent(t)
			if m.Observe([]byte(tc.event)) || !m.Done || m.Capability != tc.capability || m.ExecutionOutcome != tc.outcome || m.ContinuationSent {
				t.Fatal(m.Evidence)
			}
		})
	}
	m := clientReady(t)
	m.WriteAttempt("response.create")
	m.WriteResult("response.create", errors.New("failed"))
	m.Observe([]byte(`{"type":"response.created","response":{"id":"resp_next"}}`))
	if m.Capability != Unknown || m.ErrorCode != "correlation_error" || m.ContinuationWriteCompleted {
		t.Fatal(m.Evidence)
	}
	for _, event := range []string{`{"type":"response.created","response":{"id":"resp_parent"}}`, `{"type":"response.created","response":{"id":"resp_next","previous_response_id":"foreign"}}`} {
		m := clientReady(t)
		m.WriteAttempt("response.create")
		m.WriteResult("response.create", nil)
		m.Observe([]byte(event))
		if m.Capability != Unknown || !m.Done {
			t.Fatal(m.Evidence)
		}
	}
	m = clientCommitted(t)
	m.Observe([]byte(`{"type":"response.failed","response":{"id":"resp_next","error":{"code":"server_error","message":"late failure"}}}`))
	if m.Capability != Supported || m.ExecutionOutcome != "successor_failed" || m.UpstreamError == nil {
		t.Fatal(m.Evidence)
	}
}
