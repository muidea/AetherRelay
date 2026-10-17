package steering

import (
	"encoding/json"

	"aetherrelay/internal/pkg/aetherrelaycodex"
)

const ClientProfile = "codex_client_drain_no_tools_v2"
const ClientTarget = "codex_client_steering"
const ClientInvocation = "local_queue_drain_create"

// ClientMachine implements the ordinary (non-lite) Codex local queue/drain/create
// workflow. It has no native response.steer acceptance evidence.
type ClientMachine struct{ *Machine }

func NewClientEvidence() Evidence {
	e := NewEvidence()
	e.CheckProfile, e.CapabilityTarget, e.InvocationMode = ClientProfile, ClientTarget, ClientInvocation
	return e
}
func NewClientMachine() *ClientMachine {
	m := NewMachine()
	m.Evidence = NewClientEvidence()
	return &ClientMachine{Machine: m}
}

// Continue uses the normal client create path after the parent terminal event.
// The update is new input; parent output stays in the same connection's lineage.
func Continue(model, parent string) []byte { return createClient(model, parent, SteerInput) }

func createClient(model, parent, input string) []byte {
	type content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type message struct {
		Role    string    `json:"role"`
		Content []content `json:"content"`
	}
	value := struct {
		Type               string    `json:"type"`
		Model              string    `json:"model"`
		Instructions       string    `json:"instructions"`
		Input              []message `json:"input"`
		Store              bool      `json:"store"`
		PreviousResponseID string    `json:"previous_response_id,omitempty"`
	}{"response.create", model, Instructions, []message{{Role: "user", Content: []content{{Type: "input_text", Text: input}}}}, false, parent}
	b, _ := json.Marshal(value)
	return b
}

func (m *ClientMachine) WriteAttempt(kind string) {
	if m.Done {
		return
	}
	if kind == "response.create" {
		if m.parentEnded {
			m.ContinuationSent, m.SteeringState = true, "writing"
		} else {
			m.CreateSent = true
		}
	}
	m.Trace("send", kind, "attempt")
}

func (m *ClientMachine) WriteResult(kind string, err error) {
	if m.Done {
		return
	}
	state := "write_failed"
	if err == nil {
		state = "written"
	}
	if kind == "response.create" {
		if m.parentEnded {
			m.ContinuationWriteCompleted = err == nil
			m.SteeringState = "write_failed"
			if err == nil {
				m.SteeringState = "sent"
			}
		} else {
			m.CreateWriteCompleted = err == nil
		}
	}
	m.Trace("send", kind, state)
}

// Observe returns true exactly once, after the initial response completes and
// the locally queued update can be submitted as a normal response.create.
func (m *ClientMachine) Observe(raw []byte) bool {
	if m.Done {
		return false
	}
	defer func() {
		if m.Done && (len(m.Timeline) == 0 || m.Timeline[len(m.Timeline)-1].Type != "probe.finished") {
			m.Trace("local", "probe.finished", m.ExecutionOutcome)
		}
	}()
	type outputItem struct {
		Type string `json:"type"`
	}
	var e struct {
		Type       string `json:"type"`
		ResponseID string `json:"response_id"`
		Response   struct {
			ID         string       `json:"id"`
			Model      string       `json:"model"`
			PreviousID string       `json:"previous_response_id"`
			Status     string       `json:"status"`
			Usage      *Usage       `json:"usage"`
			Output     []outputItem `json:"output"`
			Error      errorWire    `json:"error"`
		} `json:"response"`
		Item   outputItem      `json:"item"`
		Error  errorWire       `json:"error"`
		Code   json.RawMessage `json:"code"`
		Status json.RawMessage `json:"status"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Type == "" {
		m.Trace("receive", "invalid_event", "")
		m.Finish("protocol_error")
		return false
	}
	m.EventCount++
	m.Trace("receive", e.Type, "")
	if aetherrelaycodex.ObservationFeature(e.Type) != "" {
		return false
	}
	if e.Type == "error" {
		code := m.eventError(e.Type, e.Error, e.Status, e.Code)
		m.Reject(code, m.UpstreamError.Status)
		return false
	}
	switch e.Type {
	case "response.created":
		if !validID(e.Response.ID) {
			m.Finish("protocol_error")
			return false
		}
		if m.ParentID == "" {
			if !m.CreateWriteCompleted || e.Response.PreviousID != "" {
				m.Finish("correlation_error")
				return false
			}
			m.ParentID = e.Response.ID
			m.AuthState, m.ModelAccessState = "accepted", "accepted"
			m.Phase, m.SteeringState = "steering", "queued_local"
			m.InputQueued = true
			m.ExecutionOutcome = "draining_parent"
			m.Trace("local", "input.queued", "queued_local")
		} else {
			if !m.parentEnded || !m.ContinuationWriteCompleted || m.SuccessorID != "" || e.Response.ID == m.ParentID || e.Response.PreviousID != "" && e.Response.PreviousID != m.ParentID {
				m.Finish("correlation_error")
				return false
			}
			m.SuccessorID = e.Response.ID
			m.Capability, m.SteeringState = Supported, "committed"
			m.Phase, m.ExecutionOutcome = "successor", "running"
		}
		if len(e.Response.Model) > 0 && len(e.Response.Model) <= 256 {
			m.ResponseModel = e.Response.Model
		}
	case "response.completed", "response.done", "response.incomplete", "response.failed", "response.cancelled", "response.canceled":
		success := (e.Type == "response.completed" || e.Type == "response.done") && (e.Response.Status == "" || e.Response.Status == "completed")
		failureCode := ""
		if !success {
			failureCode = m.eventError(e.Type, e.Response.Error, e.Status, nil)
		}
		if e.Response.ID == "" || e.Response.ID != m.ParentID && e.Response.ID != m.SuccessorID || e.Response.ID == m.ParentID && m.parentEnded {
			m.Finish("correlation_error")
			return false
		}
		if e.Response.ID == m.ParentID {
			m.ParentUsage, m.parentEnded = e.Response.Usage, true
			if !success {
				m.Reject(failureCode, m.UpstreamError.Status)
				return false
			}
			for _, item := range e.Response.Output {
				m.requireInput(item.Type)
			}
			if len(m.RequiredInput) > 0 {
				m.Capability, m.SteeringState = Pending, "pending"
				m.ExecutionOutcome, m.Done = "waiting_for_required_input", true
				return false
			}
			if !m.InputQueued || m.ContinuationSent {
				m.Finish("correlation_error")
				return false
			}
			m.SteeringState, m.ExecutionOutcome = "ready", "awaiting_commit"
			return true
		}
		m.SuccessorUsage = e.Response.Usage
		if len(e.Response.Model) > 0 && len(e.Response.Model) <= 256 {
			m.ResponseModel = e.Response.Model
		}
		if success {
			m.ExecutionOutcome = "completed"
		} else {
			m.ExecutionOutcome, m.ErrorCode = "successor_failed", "successor_failed"
		}
		m.Done = true
	default:
		if !knownOutputEvent(e.Type) {
			m.Finish("unknown_business_event")
			return false
		}
		if e.Type == "keepalive" {
			return false
		}
		if e.Response.ID != "" && e.Response.ID != m.ParentID && e.Response.ID != m.SuccessorID || e.ResponseID != "" && (m.ParentID == "" || e.ResponseID != m.ParentID && e.ResponseID != m.SuccessorID) {
			m.Finish("correlation_error")
			return false
		}
		if m.ParentID == "" {
			m.Finish("correlation_error")
			return false
		}
		if e.Type == "response.output_item.done" && !m.parentEnded {
			m.requireInput(e.Item.Type)
		}
	}
	return false
}

func (m *ClientMachine) requireInput(kind string) {
	var required string
	switch kind {
	case "function_call":
		required = "function_call_output"
	case "custom_tool_call":
		required = "custom_tool_call_output"
	case "mcp_approval_request":
		required = "mcp_approval_response"
	default:
		return
	}
	for _, old := range m.RequiredInput {
		if old == required {
			return
		}
	}
	m.RequiredInput = append(m.RequiredInput, required)
}

// Reject scopes an explicit incompatibility to the written continuation, never
// to the earlier local queue operation or a completed capability observation.
func (m *ClientMachine) Reject(code string, status int) {
	if m.Done {
		return
	}
	if code == "steering_not_supported" || code == "unsupported_native_inflight_message" {
		if m.ContinuationSent && m.SuccessorID == "" && m.Phase == "steering" {
			m.Capability, m.SteeringState = Unsupported, "failed"
		}
		m.Finish(code)
		return
	}
	m.Machine.Reject(code, status)
}
