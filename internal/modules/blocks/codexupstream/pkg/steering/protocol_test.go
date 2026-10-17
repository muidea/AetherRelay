package steering

import (
	"encoding/json"
	"testing"
)

const parentCreated = `{"type":"response.created","response":{"id":"resp_parent","model":"gpt-test"}}`
const accepted = `{"type":"response.steer.accepted","steer":{"id":"steer_one","previous_response_id":"resp_parent"}}`
const parentSteered = `{"type":"response.incomplete","response":{"id":"resp_parent","incomplete_details":{"reason":"steered"},"usage":{"input_tokens":3,"output_tokens":1}}}`
const successorCreated = `{"type":"response.created","response":{"id":"resp_next","previous_response_id":"resp_parent","model":"gpt-test"}}`

func TestDedicatedSteerWire(t *testing.T) {
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(Steer("resp_parent"), &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire) != 3 || string(wire["type"]) != `"response.steer"` || string(wire["previous_response_id"]) != `"resp_parent"` || wire["input"] == nil {
		t.Fatal(string(Steer("resp_parent")))
	}
}

func TestSteeringClassification(t *testing.T) {
	for _, tc := range []struct {
		name                string
		events              []string
		capability, outcome string
		done                bool
	}{
		{"success", []string{parentCreated, accepted, parentSteered, successorCreated, `{"type":"response.completed","response":{"id":"resp_next","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`}, Supported, "completed", true},
		{"original_completed", []string{parentCreated, `{"type":"response.completed","response":{"id":"resp_parent"}}`, accepted, successorCreated}, Supported, "running", false},
		{"accepted_only", []string{parentCreated, accepted}, Unknown, "awaiting_commit", false},
		{"pending", []string{parentCreated, accepted, `{"type":"response.steer.pending","steer":{"id":"steer_one","previous_response_id":"resp_parent"},"required_input":[{"type":"function_call_output","name":"secret-do-not-project"}]}`}, Pending, "waiting_for_required_input", true},
		{"unsupported_before_accepted", []string{parentCreated, `{"type":"response.steer.failed","steer":{"previous_response_id":"resp_parent"},"error":{"code":"steering_not_supported","message":"do-not-project"}}`}, Unsupported, "steering_not_supported", true},
		{"unsupported_after_accepted", []string{parentCreated, accepted, `{"type":"response.steer.failed","steer":{"id":"steer_one","previous_response_id":"resp_parent"},"error":{"code":"steering_not_supported"}}`}, Unknown, "steering_not_supported", true},
		{"successor_creation_failed", []string{parentCreated, accepted, parentSteered, `{"type":"response.steer.failed","steer":{"id":"steer_one","previous_response_id":"resp_parent"},"error":{"code":"successor_creation_failed"}}`}, Unknown, "successor_creation_failed", true},
		{"wrong_steer", []string{parentCreated, accepted, `{"type":"response.steer.failed","steer":{"id":"other","previous_response_id":"resp_parent"},"error":{"code":"steering_not_supported"}}`}, Unknown, "correlation_error", true},
		{"unrelated_created", []string{parentCreated, successorCreated}, Unknown, "correlation_error", true},
		{"wrong_parent", []string{parentCreated, accepted, parentSteered, `{"type":"response.created","response":{"id":"resp_next","previous_response_id":"other"}}`}, Unknown, "correlation_error", true},
		{"successor_failed", []string{parentCreated, accepted, parentSteered, successorCreated, `{"type":"response.failed","response":{"id":"resp_next"}}`}, Supported, "successor_failed", true},
		{"unknown_event_after_commit", []string{parentCreated, accepted, parentSteered, successorCreated, `{"type":"response.future_business"}`}, Supported, "unknown_business_event", true},
		{"ancillary", []string{parentCreated, `{"type":"response.metadata","future":{"anything":true}}`, `{"type":"responsesapi.websocket_timing","output_tokens":900}`, accepted}, Unknown, "awaiting_commit", false},
		{"race", []string{parentCreated, `{"type":"response.steer.failed","steer":{"previous_response_id":"resp_parent"},"error":{"code":"response_not_active"}}`}, Unknown, "response_not_active", true},
		{"invalid_json", []string{`not json`}, Unknown, "protocol_error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMachine()
			for _, e := range tc.events {
				m.Observe([]byte(e))
			}
			if m.Capability != tc.capability || m.ExecutionOutcome != tc.outcome || m.Done != tc.done {
				t.Fatalf("%+v", m.Evidence)
			}
			if tc.name == "ancillary" && m.ParentUsage != nil {
				t.Fatal("timing supplied usage")
			}
			if tc.name == "success" && (m.ParentUsage == nil || m.SuccessorUsage == nil || *m.SuccessorUsage.TotalTokens != 7) {
				t.Fatal("lost separate formal usage")
			}
		})
	}
}

func TestDisconnectCommitAndErrorStages(t *testing.T) {
	for _, committed := range []bool{false, true} {
		m := NewMachine()
		m.Observe([]byte(parentCreated))
		m.Observe([]byte(accepted))
		if committed {
			m.Observe([]byte(parentSteered))
			m.Observe([]byte(successorCreated))
		}
		m.Finish("network")
		want := Unknown
		if committed {
			want = Supported
		}
		if m.Capability != want {
			t.Fatal(m.Evidence)
		}
	}
	for _, tc := range []struct {
		status            int
		code, auth, model string
	}{
		{403, "access_denied", "not_observed", "not_observed"}, {401, "authentication_error", "rejected", "not_observed"}, {429, "rate_limit", "not_observed", "not_observed"}, {503, "upstream_error", "not_observed", "not_observed"},
	} {
		m := NewMachine()
		m.Reject("", tc.status)
		if m.ErrorCode != tc.code || m.AuthState != tc.auth || m.ModelAccessState != tc.model || m.Capability != Unknown {
			t.Fatal(m.Evidence)
		}
	}
}

const nativeInflightError = `{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_native_inflight_message","message":"The experimental native turn lane cannot accept stateful WebSocket messages while a native turn is running. Start a new independent response.create turn instead."}}`

func TestNativeInflightRejectionClassification(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		events                     []string
		submit                     bool
		capability, state, outcome string
	}{
		{"live_sequence", []string{`{"type":"codex.rate_limits"}`, `{"type":"codex.response.metadata"}`, parentCreated, `{"type":"response.in_progress"}`}, true, Unsupported, "failed", "unsupported_native_inflight_message"},
		{"create_phase", nil, false, Unknown, "not_observed", "unsupported_native_inflight_message"},
		{"before_steer_attempt", []string{parentCreated}, false, Unknown, "ready", "unsupported_native_inflight_message"},
		{"parent_completed", []string{parentCreated, `{"type":"response.completed","response":{"id":"resp_parent"}}`}, true, Unknown, "sent", "unsupported_native_inflight_message"},
		{"accepted", []string{parentCreated, accepted}, true, Unknown, "accepted", "unsupported_native_inflight_message"},
		{"committed", []string{parentCreated, accepted, parentSteered, successorCreated}, true, Supported, "committed", "unsupported_native_inflight_message"},
		{"pending_is_terminal", []string{parentCreated, accepted, `{"type":"response.steer.pending","steer":{"id":"steer_one","previous_response_id":"resp_parent"}}`}, true, Pending, "pending", "waiting_for_required_input"},
		{"completed_is_terminal", []string{parentCreated, accepted, parentSteered, successorCreated, `{"type":"response.completed","response":{"id":"resp_next"}}`}, true, Supported, "committed", "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMachine()
			m.HTTPStatus = 101
			for _, e := range tc.events {
				if m.Observe([]byte(e)) && tc.submit {
					m.WriteAttempt("response.steer")
					m.WriteResult("response.steer", nil)
				}
			}
			wasDone := m.Done
			m.Observe([]byte(nativeInflightError))
			if !m.Done || m.Capability != tc.capability || m.SteeringState != tc.state || m.ExecutionOutcome != tc.outcome || m.HTTPStatus != 101 {
				t.Fatal(m.Evidence)
			}
			if wasDone {
				if m.UpstreamError != nil {
					t.Fatal("terminal evidence changed", m.Evidence)
				}
			} else if m.UpstreamError == nil || m.UpstreamError.Code != "unsupported_native_inflight_message" || m.UpstreamError.Type != "invalid_request_error" || m.UpstreamError.Message == "" || m.ErrorCode != tc.outcome {
				t.Fatal("lost rejection details", m.Evidence)
			}
		})
	}
}

func TestUnsupportedClassificationUsesExactCodeAndPreservesEvidence(t *testing.T) {
	for _, code := range []string{"steering_not_supported", "unsupported_native_inflight_message"} {
		for _, stage := range []string{"accepted", "committed", "pending", "timeout"} {
			t.Run(code+"/"+stage, func(t *testing.T) {
				m := NewMachine()
				m.Observe([]byte(parentCreated))
				m.WriteAttempt("response.steer")
				want := Unknown
				if stage == "timeout" {
					m.Finish("timeout")
				} else {
					m.Observe([]byte(accepted))
					if stage == "committed" {
						m.Observe([]byte(parentSteered))
						m.Observe([]byte(successorCreated))
						want = Supported
					} else if stage == "pending" {
						m.Observe([]byte(`{"type":"response.steer.pending","steer":{"id":"steer_one","previous_response_id":"resp_parent"}}`))
						want = Pending
					}
				}
				wasDone, outcome := m.Done, m.ExecutionOutcome
				m.Reject(code, 400)
				if m.Capability != want || !m.Done || wasDone && m.ExecutionOutcome != outcome {
					t.Fatal("later rejection overwrote evidence", m.Evidence)
				}
			})
		}
	}
	for _, code := range []string{"future_rejection", "unsupported_native_inflight_message_v2", "Unsupported_native_inflight_message", ""} {
		m := NewMachine()
		m.Observe([]byte(parentCreated))
		m.WriteAttempt("response.steer")
		raw, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"code": code, "type": "invalid_request_error", "message": "steering_not_supported unsupported_native_inflight_message"}})
		m.Observe(raw)
		if m.Capability != Unknown {
			t.Fatal("inferred unsupported from non-exact code or text", m.Evidence)
		}
	}
}
