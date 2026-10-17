package steering

// FallbackDiagnostic preserves the previous attempt's failure evidence and
// consumption without introducing a second capability result.
type FallbackDiagnostic struct {
	Reason               string         `json:"reason"`
	Phase                string         `json:"phase"`
	ExecutionOutcome     string         `json:"execution_outcome"`
	ErrorCode            string         `json:"error_code,omitempty"`
	HTTPStatus           int            `json:"upstream_http_status,omitempty"`
	ParentID             string         `json:"parent_response_id,omitempty"`
	SteerID              string         `json:"steering_id,omitempty"`
	SteeringState        string         `json:"steering_state"`
	CreateSent           bool           `json:"create_sent"`
	SteerSent            bool           `json:"steer_sent"`
	CreateWriteCompleted bool           `json:"create_write_completed"`
	SteerWriteCompleted  bool           `json:"steer_write_completed"`
	UpstreamError        *UpstreamError `json:"upstream_error,omitempty"`
	Timeline             []TraceEvent   `json:"timeline,omitempty"`
	TimelineTruncated    bool           `json:"timeline_truncated"`
	ParentUsage          *Usage         `json:"parent_usage,omitempty"`
	SuccessorUsage       *Usage         `json:"successor_usage,omitempty"`
	EventCount           int            `json:"event_count"`
	ReceivedBytes        int64          `json:"received_bytes"`
}

func NewFallback(e Evidence, reason string) *FallbackDiagnostic {
	e = e.Clone()
	return &FallbackDiagnostic{Reason: reason, Phase: e.Phase, ExecutionOutcome: e.ExecutionOutcome, ErrorCode: e.ErrorCode, HTTPStatus: e.HTTPStatus, ParentID: e.ParentID, SteerID: e.SteerID, SteeringState: e.SteeringState, CreateSent: e.CreateSent, SteerSent: e.SteerSent, CreateWriteCompleted: e.CreateWriteCompleted, SteerWriteCompleted: e.SteerWriteCompleted, UpstreamError: e.UpstreamError, Timeline: e.Timeline, TimelineTruncated: e.TimelineTruncated, ParentUsage: e.ParentUsage, SuccessorUsage: e.SuccessorUsage, EventCount: e.EventCount, ReceivedBytes: e.ReceivedBytes}
}
func (d *FallbackDiagnostic) Clone() *FallbackDiagnostic {
	if d == nil {
		return nil
	}
	v := *d
	e := Evidence{UpstreamError: d.UpstreamError, Timeline: d.Timeline, ParentUsage: d.ParentUsage, SuccessorUsage: d.SuccessorUsage}.Clone()
	v.UpstreamError, v.Timeline, v.ParentUsage, v.SuccessorUsage = e.UpstreamError, e.Timeline, e.ParentUsage, e.SuccessorUsage
	return &v
}

// ClientFallbackReason allows one alternative invocation after an inconclusive
// native protocol attempt. Account admission and conclusive results stop here.
// CP-PROBE-001/002.
func ClientFallbackReason(e Evidence) string {
	if !e.Done || (e.Capability != Unknown && e.Capability != Unsupported) || !validID(e.ParentID) || !e.SteerSent || !e.CreateWriteCompleted || e.AuthState != "accepted" || e.ModelAccessState != "accepted" {
		return ""
	}
	switch e.ErrorCode {
	case "canceled", "account_changed", "configuration_error", "receive_limit", "rate_limit", "rate_limit_exceeded", "usage_limit_reached", "invalid_api_key", "invalid_token", "token_expired", "authentication_error", "model_not_found", "model_not_available", "model_access_denied", "access_denied":
		return ""
	}
	if e.UpstreamError != nil {
		switch e.UpstreamError.Status {
		case 401, 403, 429:
			return ""
		}
		switch e.UpstreamError.Code {
		case "rate_limit_exceeded", "usage_limit_reached", "invalid_api_key", "invalid_token", "token_expired", "authentication_error", "model_not_found", "model_not_available", "model_access_denied":
			return ""
		}
	}
	if e.Capability == Unsupported {
		return "native_protocol_unsupported"
	}
	return "native_protocol_inconclusive"
}
