package events

import "aetherrelay/internal/modules/blocks/codexupstream/pkg/steering"

const (
	TopicStartSteeringProbe = "aetherrelay.proxy.command.start_steering_probe"
	TopicGetSteeringProbe   = "aetherrelay.proxy.query.steering_probe"
)

type SteeringParameters struct {
	Instructions string `json:"instructions"`
	InitialInput string `json:"initial_input"`
	SteerInput   string `json:"steer_input"`
	Store        bool   `json:"store"`
	Reasoning    string `json:"reasoning"`
}
type SteeringReport struct {
	steering.Evidence
	Fallback                   *steering.FallbackDiagnostic `json:"fallback,omitempty"`
	ProbeID                    string                       `json:"probe_id"`
	TargetSlotID               string                       `json:"target_slot_id"`
	ActualSlotID               *string                      `json:"actual_slot_id"`
	AccountLabel               string                       `json:"account_label,omitempty"`
	Model                      string                       `json:"model"`
	Profile                    string                       `json:"probe_profile"`
	Parameters                 SteeringParameters           `json:"parameters"`
	ConfigVersion              string                       `json:"account_config_version,omitempty"`
	FingerprintMode            string                       `json:"fingerprint_mode,omitempty"`
	ProxyConfigured            bool                         `json:"proxy_configured"`
	ClientVersion              string                       `json:"client_version,omitempty"`
	AdmissionState             string                       `json:"admission_state"`
	AdmissionReason            string                       `json:"admission_reason,omitempty"`
	Endpoint                   string                       `json:"upstream_endpoint,omitempty"`
	Transport                  string                       `json:"transport"`
	State                      string                       `json:"state"`
	CreatedAt                  string                       `json:"created_at"`
	StartedAt                  string                       `json:"started_at,omitempty"`
	FinishedAt                 string                       `json:"finished_at,omitempty"`
	ExpiresAt                  string                       `json:"expires_at,omitempty"`
	CredentialRefreshPerformed bool                         `json:"credential_refresh_performed"`
	CredentialRefreshResult    string                       `json:"credential_refresh_result,omitempty"`
	Historical                 bool                         `json:"historical"`
}
type StartSteeringProbeCommand struct{ SlotID, Model, IdempotencyKey string }
type GetSteeringProbeCommand struct{ SlotID, ProbeID string }
type SteeringProbeResult struct {
	Report    SteeringReport
	Status    int
	ErrorCode string
}

// SteeringAPIError contains only bounded application codes, never raw network
// errors or credentials. Admin can preserve conflict/capacity/not-found status.
type SteeringAPIError struct {
	Status int
	Code   string
}

func (e *SteeringAPIError) Error() string { return e.Code }

// Clone isolates nested evidence across worker, event and management readers.
func (r SteeringReport) Clone() SteeringReport {
	r.Evidence = r.Evidence.Clone()
	r.Fallback = r.Fallback.Clone()
	if r.ActualSlotID != nil {
		v := *r.ActualSlotID
		r.ActualSlotID = &v
	}
	return r
}
