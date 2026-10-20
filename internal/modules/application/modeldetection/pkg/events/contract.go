package events

import (
	proxy "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	"aetherrelay/internal/pkg/modeltrace"
)

const (
	TopicStart  = "aetherrelay.modeldetection.command.start"
	TopicGet    = "aetherrelay.modeldetection.query.report"
	TopicCancel = "aetherrelay.modeldetection.command.cancel"
)

type StartCommand struct{ SlotID, Model, Reasoning, IdempotencyKey string }
type GetCommand struct{ SlotID, ProbeID string }
type CancelCommand struct{ SlotID, ProbeID string }

// Report owns the assessment; the embedded sampling receipt stays Proxy-owned.
type Report struct {
	proxy.ModelTraceReport
	ParsedCounts      []int               `json:"parsed_counts,omitempty"`
	Bank              modeltrace.Snapshot `json:"bank"`
	Result            *modeltrace.Result  `json:"result,omitempty"`
	PathValidation    string              `json:"path_fingerprint_validated"`
	QualityValidation string              `json:"quality_regression_validated"`
}

func (r Report) Clone() Report {
	r.ModelTraceReport = r.ModelTraceReport.Clone()
	r.ParsedCounts = append([]int(nil), r.ParsedCounts...)
	if r.Result != nil {
		v := *r.Result
		r.Result = &v
		if v.Analysis != nil {
			a := *v.Analysis
			r.Result.Analysis = &a
			a.Results = append([]modeltrace.Candidate(nil), a.Results...)
			a.Families = append([]modeltrace.Family(nil), a.Families...)
			a.Diagnostics = append([]modeltrace.Diagnostic(nil), a.Diagnostics...)
		}
	}
	return r
}

type Result struct {
	Report    Report
	Status    int
	ErrorCode string
}
type APIError struct {
	Status int
	Code   string
}

func (e *APIError) Error() string { return e.Code }
