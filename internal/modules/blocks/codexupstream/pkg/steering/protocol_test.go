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
		{"unsupported_after_accepted", []string{parentCreated, accepted, `{"type":"response.steer.failed","steer":{"id":"steer_one","previous_response_id":"resp_parent"},"error":{"code":"steering_not_supported"}}`}, Unsupported, "steering_not_supported", true},
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
