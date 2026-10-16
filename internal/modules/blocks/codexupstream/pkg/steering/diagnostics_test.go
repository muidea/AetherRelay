package steering

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFailureDiagnosticsPreserveUnknownErrorsAndHandshakeStatus(t *testing.T) {
	for _, tc := range []struct {
		name, wire, outcome, code, kind, param, message string
		status                                          int
	}{
		{"unknown", `{"type":"error","error":{"type":"future_error","code":"future_rejection","param":"input","message":"unsupported combination"}}`, "upstream_error", "future_rejection", "future_error", "input", "unsupported combination", 0},
		{"frame status", `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"future_parameter","param":"input","message":"cannot apply update"}}`, "invalid_request", "future_parameter", "invalid_request_error", "input", "cannot apply update", 400},
		{"missing code", `{"type":"error","error":{"type":"invalid_request_error","code":null,"param":null,"message":"invalid input"}}`, "invalid_request_error", "", "invalid_request_error", "", "invalid input", 0},
		{"numeric code", `{"type":"error","error":{"code":12345,"status":503,"message":"retry later"}}`, "upstream_error", "12345", "", "", "retry later", 503},
		{"top level code", `{"type":"error","code":"future_top_code","error":{"message":"details"}}`, "upstream_error", "future_top_code", "", "", "details", 0},
		{"empty error", `{"type":"error","error":{}}`, "upstream_error", "", "", "", "", 0},
		{"not an HTTP failure", `{"type":"error","status":101,"error":{"code":"future_rejection"}}`, "upstream_error", "future_rejection", "", "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMachine()
			m.HTTPStatus = 101
			m.Observe([]byte(parentCreated))
			m.WriteAttempt("response.steer")
			m.WriteResult("response.steer", nil)
			m.Observe([]byte(tc.wire))
			d := m.UpstreamError
			if !m.Done || m.Capability != Unknown || m.ExecutionOutcome != tc.outcome || m.HTTPStatus != 101 || d == nil {
				t.Fatalf("unexpected classification: %+v", m.Evidence)
			}
			if d.EventType != "error" || d.Code != tc.code || d.Type != tc.kind || d.Param != tc.param || d.Message != tc.message || d.Status != tc.status || d.Truncated {
				t.Fatalf("diagnostic lost: %+v", d)
			}
			if !m.SteerWriteCompleted || m.SteerID != "" || m.SuccessorID != "" {
				t.Fatal("write completion became protocol acceptance", m.Evidence)
			}
		})
	}
}

func TestFailureEventSourcesKeepCorrelationAndCapability(t *testing.T) {
	for _, tc := range []struct {
		name, wire, outcome, capability, eventType string
		committed                                  bool
	}{
		{"steer failed", `{"type":"response.steer.failed","steer":{"previous_response_id":"resp_parent"},"error":{"code":"new_error","message":"diagnostic"}}`, "upstream_error", Unknown, "response.steer.failed", false},
		{"unrelated steer", `{"type":"response.steer.failed","steer":{"previous_response_id":"unrelated"},"error":{"code":"steering_not_supported","message":"diagnostic"}}`, "correlation_error", Unknown, "response.steer.failed", false},
		{"parent failed", `{"type":"response.failed","response":{"id":"resp_parent","error":{"code":"new_error","message":"diagnostic"}}}`, "upstream_error", Unknown, "response.failed", false},
		{"unrelated response", `{"type":"response.failed","response":{"id":"unrelated","error":{"code":"new_error","message":"diagnostic"}}}`, "correlation_error", Unknown, "response.failed", false},
		{"successor failed", `{"type":"response.failed","response":{"id":"resp_next","error":{"code":"new_error","message":"diagnostic"}}}`, "successor_failed", Supported, "response.failed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMachine()
			m.Observe([]byte(parentCreated))
			if tc.committed {
				for _, e := range []string{accepted, parentSteered, successorCreated} {
					m.Observe([]byte(e))
				}
			}
			m.Observe([]byte(tc.wire))
			if !m.Done || m.ExecutionOutcome != tc.outcome || m.Capability != tc.capability || m.UpstreamError == nil || m.UpstreamError.EventType != tc.eventType || m.UpstreamError.Message != "diagnostic" {
				t.Fatalf("lost failure/correlation evidence: %+v", m.Evidence)
			}
			last := m.Timeline[len(m.Timeline)-1]
			if last.Type != "probe.finished" || last.State != tc.outcome {
				t.Fatal(last)
			}
		})
	}
}

func TestWriteAttemptCompletionAndAcceptanceAreIndependent(t *testing.T) {
	for _, writeErr := range []error{nil, errors.New("partial write")} {
		m := NewMachine()
		m.WriteAttempt("response.create")
		if !m.CreateSent || m.CreateWriteCompleted {
			t.Fatal(m.Evidence)
		}
		m.WriteResult("response.create", nil)
		if !m.CreateWriteCompleted {
			t.Fatal(m.Evidence)
		}
		if !m.Observe([]byte(parentCreated)) || m.SteerSent || m.SteeringState != "ready" {
			t.Fatal(m.Evidence)
		}
		m.WriteAttempt("response.steer")
		if !m.SteerSent || m.SteerWriteCompleted || m.SteeringState != "writing" {
			t.Fatal(m.Evidence)
		}
		m.WriteResult("response.steer", writeErr)
		if m.SteerWriteCompleted != (writeErr == nil) || m.SteerID != "" || m.Capability != Unknown {
			t.Fatal(m.Evidence)
		}
		if writeErr != nil {
			m.Finish("network")
			if m.SteeringState != "write_failed" {
				t.Fatal(m.Evidence)
			}
			continue
		}
		if m.SteeringState != "sent" {
			t.Fatal(m.Evidence)
		}
		m.Observe([]byte(accepted))
		if m.SteeringState != "accepted" || m.Capability != Unknown {
			t.Fatal(m.Evidence)
		}
	}
}

func TestDiagnosticsAreBoundedAndExcludeOutput(t *testing.T) {
	m := NewMachine()
	m.Observe([]byte(parentCreated))
	for range MaxTimelineEvents + 20 {
		m.Observe([]byte(`{"type":"response.output_text.delta","delta":"generated-content-must-not-persist"}`))
	}
	wire, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{
		"type": strings.Repeat("类", 100), "code": strings.Repeat("错", 100),
		"param": strings.Repeat("参", 100), "message": "hello\n" + strings.Repeat("错", 2000),
	}})
	m.Observe(wire)
	d := m.UpstreamError
	if d == nil || !d.Truncated || len(d.Type) > 128 || len(d.Code) > 128 || len(d.Param) > 256 || len(d.Message) > 4096 {
		t.Fatal("unbounded diagnostic", d)
	}
	if !m.TimelineTruncated || len(m.Timeline) != MaxTimelineEvents || m.Timeline[0].Sequence <= 1 {
		t.Fatal(m.Timeline)
	}
	for i, e := range m.Timeline {
		if i > 0 && (e.Sequence != m.Timeline[i-1].Sequence+1 || e.ElapsedMS < m.Timeline[i-1].ElapsedMS) {
			t.Fatal(m.Timeline)
		}
	}
	for _, text := range []string{d.Type, d.Code, d.Param, d.Message} {
		if !utf8.ValidString(text) || strings.ContainsAny(text, "\n\r\x00") {
			t.Fatal("invalid bounded text")
		}
	}
	raw, _ := json.Marshal(m.Evidence)
	if strings.Contains(string(raw), "generated-content-must-not-persist") {
		t.Fatal("output leaked into diagnostics")
	}
}

func TestEvidenceCloneSeparatesMutableDiagnostics(t *testing.T) {
	m := NewMachine()
	m.Observe([]byte(parentCreated))
	m.Observe([]byte(`{"type":"error","error":{"code":"future","message":"original"}}`))
	n := int64(7)
	m.ParentUsage = &Usage{InputTokens: &n}
	m.RequiredInput = []string{"function_call_output"}
	copy := m.Evidence.Clone()
	copy.UpstreamError.Message = "changed"
	copy.Timeline[0].Type = "changed"
	copy.RequiredInput[0] = "changed"
	*copy.ParentUsage.InputTokens = 1
	if m.UpstreamError.Message != "original" || m.Timeline[0].Type != "response.created" || m.RequiredInput[0] != "function_call_output" || *m.ParentUsage.InputTokens != 7 {
		t.Fatal("copy mutated source", m.Evidence)
	}
}
