// Package steering implements the management probe's fixed profile and pure
// protocol classifier. It has no account state, transport, or health writes.
package steering

import (
	"encoding/json"
	"strings"
	"time"

	"aetherrelay/internal/pkg/aetherrelaycodex"
)

const (
	Profile      = "single_agent_no_tools_v1"
	Instructions = "This is a synthetic protocol capability probe. Follow the user request."
	InitialInput = "Write a numbered list of the integers from 1 to 100, one per line."
	SteerInput   = "Stop the list. Reply with exactly STEERING_PROBE_OK."
	Unknown      = "UNKNOWN"
	Supported    = "SUPPORTED"
	Unsupported  = "UNSUPPORTED_FOR_PROFILE"
	Pending      = "ACCEPTED_PENDING"
)

type Usage struct {
	InputTokens  *int64 `json:"input_tokens,omitempty"`
	OutputTokens *int64 `json:"output_tokens,omitempty"`
	TotalTokens  *int64 `json:"total_tokens,omitempty"`
}

type Evidence struct {
	Capability           string         `json:"capability"`
	Phase                string         `json:"phase"`
	ExecutionOutcome     string         `json:"execution_outcome"`
	AuthState            string         `json:"auth_state"`
	ModelAccessState     string         `json:"model_access_state"`
	WebsocketState       string         `json:"websocket_state"`
	SteeringState        string         `json:"steering_state"`
	UpstreamRequestSent  bool           `json:"upstream_request_sent"`
	CreateSent           bool           `json:"create_sent"`
	SteerSent            bool           `json:"steer_sent"`
	CreateWriteCompleted bool           `json:"create_write_completed"`
	SteerWriteCompleted  bool           `json:"steer_write_completed"`
	UpstreamError        *UpstreamError `json:"upstream_error,omitempty"`
	Timeline             []TraceEvent   `json:"timeline,omitempty"`
	TimelineTruncated    bool           `json:"timeline_truncated"`
	HTTPStatus           int            `json:"upstream_http_status,omitempty"`
	ParentID             string         `json:"parent_response_id,omitempty"`
	SuccessorID          string         `json:"successor_response_id,omitempty"`
	SteerID              string         `json:"steering_id,omitempty"`
	ResponseModel        string         `json:"response_model,omitempty"`
	ErrorCode            string         `json:"error_code,omitempty"`
	RequiredInput        []string       `json:"required_input_types,omitempty"`
	ParentUsage          *Usage         `json:"parent_usage,omitempty"`
	SuccessorUsage       *Usage         `json:"successor_usage,omitempty"`
	EventCount           int            `json:"event_count"`
	ReceivedBytes        int64          `json:"received_bytes"`
	Done                 bool           `json:"done"`
}

func NewEvidence() Evidence {
	return Evidence{Capability: Unknown, Phase: "admission", ExecutionOutcome: "not_started", AuthState: "not_observed", ModelAccessState: "not_observed", WebsocketState: "not_observed", SteeringState: "not_observed"}
}

func Create(model string) []byte {
	type content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type message struct {
		Role    string    `json:"role"`
		Content []content `json:"content"`
	}
	value := struct {
		Type         string    `json:"type"`
		Model        string    `json:"model"`
		Instructions string    `json:"instructions"`
		Input        []message `json:"input"`
		Store        bool      `json:"store"`
	}{"response.create", model, Instructions, []message{{Role: "user", Content: []content{{Type: "input_text", Text: InitialInput}}}}, false}
	b, _ := json.Marshal(value)
	return b
}

// A separate type prevents ordinary create metadata from entering this message.
func Steer(parent string) []byte {
	value := struct {
		Type               string `json:"type"`
		PreviousResponseID string `json:"previous_response_id"`
		Input              string `json:"input"`
	}{"response.steer", parent, SteerInput}
	b, _ := json.Marshal(value)
	return b
}

type Machine struct {
	Evidence
	parentEnded bool
	accepted    bool
	started     time.Time
	sequence    int64
}

func NewMachine() *Machine { return &Machine{Evidence: NewEvidence(), started: time.Now()} }

func (m *Machine) Finish(code string) {
	if m.Done {
		return
	}
	m.ErrorCode = code
	m.ExecutionOutcome = code
	m.Done = true
	m.Trace("local", "probe.finished", code)
}

// Reject preserves the phase and protocol commit evidence. Codes are bounded
// here instead of projecting raw upstream errors into the management report.
func (m *Machine) Reject(code string, status int) {
	switch code {
	case "steering_not_supported":
		if m.Capability != Supported && m.Phase == "steering" {
			m.Capability = Unsupported
		}
	case "invalid_api_key", "invalid_token", "token_expired", "authentication_error":
		m.AuthState = "rejected"
	case "model_not_found", "model_not_available", "model_access_denied":
		m.ModelAccessState = "rejected"
	case "invalid_input", "invalid_request_error", "invalid_request", "unsupported_parameter":
	case "response_already_completed", "response_not_active", "response_not_found", "too_many_pending_steers", "successor_creation_failed":
	case "rate_limit_exceeded", "usage_limit_reached":
	default:
		switch {
		case status == 401:
			code = "authentication_error"
			m.AuthState = "rejected"
		case status == 403:
			code = "access_denied"
		case status == 429:
			code = "rate_limit"
		case status >= 500:
			code = "upstream_error"
		case status == 400:
			code = "invalid_request"
		default:
			code = "upstream_error"
		}
	}
	m.Finish(code)
}

func validID(v string) bool {
	if len(v) == 0 || len(v) > 256 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// Observe returns whether the caller must send the one permitted steer.
func (m *Machine) Observe(raw []byte) bool {
	if m.Done {
		return false
	}
	defer func() {
		if m.Done && (len(m.Timeline) == 0 || m.Timeline[len(m.Timeline)-1].Type != "probe.finished") {
			m.Trace("local", "probe.finished", m.ExecutionOutcome)
		}
	}()
	var e struct {
		Type     string `json:"type"`
		Response struct {
			ID         string `json:"id"`
			Model      string `json:"model"`
			PreviousID string `json:"previous_response_id"`
			Usage      *Usage `json:"usage"`
			Incomplete struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
			Error errorWire `json:"error"`
		} `json:"response"`
		Steer struct {
			ID     string `json:"id"`
			Parent string `json:"previous_response_id"`
		} `json:"steer"`
		Error    errorWire       `json:"error"`
		Code     json.RawMessage `json:"code"`
		Status   json.RawMessage `json:"status"`
		Required []struct {
			Type string `json:"type"`
		} `json:"required_input"`
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
			m.ParentID = e.Response.ID
			m.AuthState = "accepted"
			m.ModelAccessState = "accepted"
			m.Phase = "steering"
			m.SteeringState = "ready"
			if len(e.Response.Model) > 0 && len(e.Response.Model) <= 256 {
				m.ResponseModel = e.Response.Model
			}
			return true
		}
		if !m.accepted || !m.parentEnded || m.SuccessorID != "" || e.Response.ID == m.ParentID || e.Response.PreviousID != "" && e.Response.PreviousID != m.ParentID {
			m.Finish("correlation_error")
			return false
		}
		m.SuccessorID = e.Response.ID
		m.Capability = Supported
		m.SteeringState = "committed"
		m.Phase = "successor"
		m.ExecutionOutcome = "running"
		if len(e.Response.Model) > 0 && len(e.Response.Model) <= 256 {
			m.ResponseModel = e.Response.Model
		}
	case "response.steer.accepted", "response.steer.pending", "response.steer.failed":
		failureCode := ""
		if e.Type == "response.steer.failed" {
			failureCode = m.eventError(e.Type, e.Error, e.Status, e.Code)
		}
		if m.ParentID == "" || e.Steer.Parent != m.ParentID || m.accepted && e.Steer.ID != m.SteerID || e.Steer.ID != "" && !validID(e.Steer.ID) || m.SuccessorID != "" {
			m.Finish("correlation_error")
			return false
		}
		switch e.Type {
		case "response.steer.accepted":
			if m.accepted || !validID(e.Steer.ID) {
				m.Finish("correlation_error")
				return false
			}
			m.accepted = true
			m.SteerID = e.Steer.ID
			m.SteeringState = "accepted"
			m.ExecutionOutcome = "awaiting_commit"
		case "response.steer.pending":
			if !m.accepted {
				m.Finish("correlation_error")
				return false
			}
			for _, v := range e.Required {
				switch v.Type {
				case "function_call_output", "custom_tool_call_output", "mcp_approval_response":
					if len(m.RequiredInput) < 16 {
						m.RequiredInput = append(m.RequiredInput, v.Type)
					}
				}
			}
			m.Capability = Pending
			m.SteeringState = "pending"
			m.ExecutionOutcome = "waiting_for_required_input"
			m.Done = true
		case "response.steer.failed":
			m.SteeringState = "failed"
			m.Reject(failureCode, m.UpstreamError.Status)
		}
	case "response.completed", "response.done", "response.incomplete", "response.failed", "response.cancelled", "response.canceled":
		failed := e.Type != "response.completed" && e.Type != "response.done" && !(e.Type == "response.incomplete" && e.Response.Incomplete.Reason == "steered" && e.Response.ID == m.ParentID)
		failureCode := ""
		if failed {
			failureCode = m.eventError(e.Type, e.Response.Error, e.Status, nil)
		}
		if e.Response.ID == "" || e.Response.ID != m.ParentID && e.Response.ID != m.SuccessorID {
			m.Finish("correlation_error")
			return false
		}
		if e.Response.ID == m.ParentID {
			if e.Response.Usage != nil {
				m.ParentUsage = e.Response.Usage
			}
			m.parentEnded = true
			if failed {
				m.Reject(failureCode, m.UpstreamError.Status)
			}
		} else {
			if e.Response.Usage != nil {
				m.SuccessorUsage = e.Response.Usage
			}
			if len(e.Response.Model) > 0 && len(e.Response.Model) <= 256 {
				m.ResponseModel = e.Response.Model
			}
			m.Done = true
			if e.Type == "response.completed" || e.Type == "response.done" {
				m.ExecutionOutcome = "completed"
			} else {
				m.ExecutionOutcome = "successor_failed"
				m.ErrorCode = "successor_failed"
			}
		}
	default:
		if !knownOutputEvent(e.Type) {
			m.Finish("unknown_business_event")
		}
	}
	return false
}

func knownOutputEvent(t string) bool {
	switch strings.TrimSpace(t) {
	case "response.in_progress", "response.queued", "response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done", "response.output_text.annotation.added", "response.refusal.delta", "response.refusal.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.delta", "response.reasoning_text.done", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", "keepalive":
		return true
	}
	return false
}
