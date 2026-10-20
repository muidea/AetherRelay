package events

import "aetherrelay/internal/pkg/modeltrace"

const (
	TopicStartModelTraceProbe  = "aetherrelay.proxy.command.start_modeltrace_probe"
	TopicGetModelTraceProbe    = "aetherrelay.proxy.query.modeltrace_probe"
	TopicDrainModelTraceProbe  = "aetherrelay.proxy.command.drain_modeltrace_probe"
	TopicCancelModelTraceProbe = "aetherrelay.proxy.command.cancel_modeltrace_probe"
)

type StartModelTraceProbeCommand struct {
	SlotID, Model, Reasoning, IdempotencyKey, ProbeID string
	Challenges                                        []modeltrace.Challenge
}
type GetModelTraceProbeCommand struct{ SlotID, ProbeID string }
type CancelModelTraceProbeCommand struct{ SlotID, ProbeID string }
type ModelTraceSample struct {
	ID                     string           `json:"id"`
	Prompt                 string           `json:"prompt"`
	ExpectedCount          int              `json:"expected_count"`
	State                  string           `json:"state"`
	TextSource             string           `json:"text_source,omitempty"`
	HTTPStatus             int              `json:"http_status,omitempty"`
	UsageComplete          bool             `json:"usage_complete"`
	ErrorCode              string           `json:"error_code,omitempty"`
	StartedAt              string           `json:"started_at,omitempty"`
	FinishedAt             string           `json:"finished_at,omitempty"`
	SourceRequestSHA256    string           `json:"source_request_sha256,omitempty"`
	EffectiveRequestSHA256 string           `json:"effective_request_sha256,omitempty"`
	ResponseModelClaim     string           `json:"response_model_claim,omitempty"`
	OutputText             string           `json:"output_text,omitempty"`
	ContentSHA256          string           `json:"content_sha256,omitempty"`
	Usage                  *ModelTraceUsage `json:"usage,omitempty"`
}
type ModelTraceUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}
type ModelTraceReport struct {
	SchemaVersion              string             `json:"schema_version"`
	ProbeID                    string             `json:"probe_id"`
	TargetSlotID               string             `json:"target_slot_id"`
	ActualSlotID               string             `json:"actual_slot_id,omitempty"`
	Model                      string             `json:"model"`
	Reasoning                  string             `json:"reasoning"`
	Profile                    string             `json:"probe_profile"`
	Parser                     string             `json:"parser_policy"`
	Transport                  string             `json:"transport"`
	ConfigVersion              string             `json:"account_config_version,omitempty"`
	State                      string             `json:"state"`
	Done                       bool               `json:"done"`
	ErrorCode                  string             `json:"error_code,omitempty"`
	AdmissionReason            string             `json:"admission_reason,omitempty"`
	CredentialRefreshPerformed bool               `json:"credential_refresh_performed"`
	Historical                 bool               `json:"historical"`
	CreatedAt                  string             `json:"created_at"`
	FinishedAt                 string             `json:"finished_at,omitempty"`
	ExpiresAt                  string             `json:"expires_at,omitempty"`
	Samples                    []ModelTraceSample `json:"samples"`
}

func (r ModelTraceReport) Clone() ModelTraceReport {
	r.Samples = append([]ModelTraceSample(nil), r.Samples...)
	for i := range r.Samples {
		if r.Samples[i].Usage != nil {
			v := *r.Samples[i].Usage
			r.Samples[i].Usage = &v
		}
	}

	return r
}

type ModelTraceProbeResult struct {
	Report    ModelTraceReport
	Status    int
	ErrorCode string
}
type DrainModelTraceProbeCommand struct{ SlotID, ProbeID string }
