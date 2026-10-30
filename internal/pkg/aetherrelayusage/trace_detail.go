package usage

import (
	"time"

	trace "aetherrelay/internal/pkg/aetherrelaytrace"
)

type EvidenceField struct {
	Field  string `json:"field"`
	State  string `json:"state"`
	Source string `json:"source,omitempty"`
	Detail string `json:"detail,omitempty"`
}
type TimelineEntry struct {
	At         *time.Time `json:"at,omitempty"`
	AttemptID  int        `json:"attempt_id,omitempty"`
	Kind       string     `json:"kind"`
	Detail     string     `json:"detail,omitempty"`
	DurationMS int64      `json:"duration_ms,omitempty"`
}
type TraceDetail struct {
	Event    Event           `json:"event"`
	Trace    *trace.Trace    `json:"trace"`
	Evidence []EvidenceField `json:"evidence"`
	Timeline []TimelineEntry `json:"timeline"`
}

func BuildTraceDetail(e Event, t *trace.Trace) TraceDetail {
	d := TraceDetail{Event: e, Trace: t, Evidence: []EvidenceField{}, Timeline: []TimelineEntry{{At: &e.StartedAt, Kind: "request_received"}}}
	state := "saved"
	if t == nil {
		state = "not_collected"
	}
	d.Evidence = append(d.Evidence, EvidenceField{Field: "execution_trace", State: state, Source: "usage_store"})
	if t != nil {
		if t.SaveFailed {
			d.Evidence = append(d.Evidence, EvidenceField{Field: "checkpoint_persistence", State: "saved_after_checkpoint_failure", Source: "usage_store"})
		}
		state = t.Summary().TraceState
		d.Evidence[0].State = state
		revision := "saved"
		if t.Context.Revision == "" {
			revision = "not_collected"
		}
		d.Evidence = append(d.Evidence, EvidenceField{Field: "request_start_context", State: "saved", Source: "usage_store"}, EvidenceField{Field: "request_revision", State: revision, Source: "request_start"})
		for index, a := range t.Attempts {
			id := a.ID
			at := a.StartedAt
			if !at.IsZero() {
				d.Timeline = append(d.Timeline, TimelineEntry{At: &at, AttemptID: id, Kind: "execution_started"})
			}
			for _, h := range a.Handshakes {
				d.Timeline = append(d.Timeline, TimelineEntry{AttemptID: id, Kind: "handshake", DurationMS: h.DurationMS})
			}
			if len(a.Handshakes) == 0 && a.UpstreamStatus > 0 {
				d.Timeline = append(d.Timeline, TimelineEntry{AttemptID: id, Kind: "upstream_headers", DurationMS: a.HeadersDurationMS})
			}
			if a.RequestSent != nil {
				detail := "false"
				if *a.RequestSent {
					detail = "true"
				}
				d.Timeline = append(d.Timeline, TimelineEntry{AttemptID: id, Kind: "request_sent", Detail: detail})
			}
			if a.FirstOutputAt != nil {
				d.Timeline = append(d.Timeline, TimelineEntry{At: a.FirstOutputAt, AttemptID: id, Kind: "first_upstream_output"})
			}
			if index == len(t.Attempts)-1 && e.FirstOutputAt != nil {
				d.Timeline = append(d.Timeline, TimelineEntry{At: e.FirstOutputAt, Kind: "first_client_output", AttemptID: id})
			}
			if a.Failed || a.TerminalEvent != "" {
				d.Timeline = append(d.Timeline, TimelineEntry{At: a.LastEventAt, AttemptID: id, Kind: "upstream_terminal", Detail: a.TerminalEvent})
			}
			errorState := "not_provided"
			if a.UpstreamErrorCode != "" || a.UpstreamErrorType != "" || a.UpstreamErrorMessage != "" {
				errorState = "saved"
			}
			if a.ErrorFieldsTruncated {
				errorState = "truncated"
			}
			d.Evidence = append(d.Evidence, EvidenceField{Field: "upstream_error", State: errorState, Source: "structured_observation", Detail: itoaTrace(id)}, EvidenceField{Field: "raw_error_body", State: a.ErrorBodyState, Source: "interaction_archive", Detail: itoaTrace(id)})
			closeState := "not_observed"
			if a.WebsocketCloseCode > 0 {
				closeState = "saved"
			}
			d.Evidence = append(d.Evidence, EvidenceField{Field: "websocket_close", State: closeState, Source: "transport_observation", Detail: itoaTrace(id)})
		}
	}
	tokens := "not_observed"
	if e.OutputTokensKnown {
		tokens = "saved"
	}
	d.Evidence = append(d.Evidence, EvidenceField{Field: "output_usage", State: tokens, Source: "usage_store"})
	if e.FirstOutputAt != nil && (t == nil || len(t.Attempts) == 0) {
		d.Timeline = append(d.Timeline, TimelineEntry{At: e.FirstOutputAt, Kind: "first_client_output"})
	}
	if e.CompletedAt != nil {
		d.Timeline = append(d.Timeline, TimelineEntry{At: e.CompletedAt, Kind: "request_settled", Detail: e.Outcome})
	}
	return d
}
func itoaTrace(id int) string { // decimal without adding unbounded provenance text
	if id < 0 {
		return ""
	}
	if id == 0 {
		return "0"
	}
	b := [20]byte{}
	i := len(b)
	for id > 0 {
		i--
		b[i] = byte('0' + id%10)
		id /= 10
	}
	return string(b[i:])
}
