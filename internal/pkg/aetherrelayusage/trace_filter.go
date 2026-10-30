package usage

import "strings"

const failedResultSQL = `state='completed' AND outcome IS DISTINCT FROM 'success' AND outcome IS DISTINCT FROM 'process_interrupted'`
const unknownResultSQL = `(state IS DISTINCT FROM 'completed' OR outcome='process_interrupted')`
const recoveredResultSQL = `outcome='success' AND event_id IN (SELECT event_id FROM usage_execution_trace WHERE had_anomaly)`
const partialResultSQL = `(` + failedResultSQL + `) AND (outcome='upstream_truncated' OR event_id IN (SELECT event_id FROM usage_generation WHERE partial))`
const causeSource = `(SELECT u.event_id AS id, coalesce(nullif(t.primary_cause,''),nullif(d.upstream_error_code,''),nullif(u.failure_class,''),nullif(u.error_code,''),'unknown') AS cause FROM usage_events u LEFT JOIN usage_execution_trace t ON u.event_id=t.event_id LEFT JOIN usage_upstream_error d ON u.event_id=d.event_id)`

func appendTraceFilters(parts []string, args []any, f UsageFilter) ([]string, []any) {
	if f.MissingDimension != "" {
		column := map[string]string{"account": "coalesce((SELECT account_id FROM usage_account_selection a WHERE a.event_id=usage_events.event_id),'')", "model": "coalesce(model,'')", "provider": "coalesce(provider,'')"}[f.MissingDimension]
		parts = append(parts, column+"=''")
	}
	if f.AccountID != "" {
		parts = append(parts, `event_id IN (SELECT event_id FROM usage_account_selection WHERE account_id=?)`)
		args = append(args, f.AccountID)
	}
	switch f.Result {
	case "failed":
		parts = append(parts, `(`+failedResultSQL+`)`)
	case "partial":
		parts = append(parts, `(`+partialResultSQL+`)`)
	case "recovered":
		parts = append(parts, `(`+recoveredResultSQL+`)`)
	case "unknown":
		parts = append(parts, unknownResultSQL)
	case "success":
		parts = append(parts, `outcome='success'`)
	case "anomaly":
		parts = append(parts, `((`+failedResultSQL+`) OR (`+recoveredResultSQL+`))`)
	}
	if f.Cause != "" {
		parts = append(parts, `event_id IN (SELECT id FROM `+causeSource+` WHERE cause=?)`)
		args = append(args, f.Cause)
	}
	if f.Transport != "" {
		parts = append(parts, `coalesce((SELECT nullif(t.transport,'') FROM usage_execution_trace t WHERE t.event_id=usage_events.event_id),'unknown')=?`)
		args = append(args, f.Transport)
	}
	if f.Revision != "" {
		parts = append(parts, `coalesce((SELECT nullif(t.revision,'') FROM usage_execution_trace t WHERE t.event_id=usage_events.event_id),'unknown')=?`)
		args = append(args, f.Revision)
	}
	if f.Search != "" {
		parts = append(parts, `event_id IN (SELECT u.event_id FROM usage_events u LEFT JOIN usage_execution_trace t ON u.event_id=t.event_id LEFT JOIN usage_upstream_error d ON u.event_id=d.event_id LEFT JOIN usage_account_selection a ON u.event_id=a.event_id WHERE contains(lower(concat_ws(' ',u.event_id,u.api_key_id,u.provider,u.model,u.error_code,u.failure_class,t.request_id,t.primary_cause,a.account_label,a.account_id,d.upstream_error_code)),?))`)
		args = append(args, strings.ToLower(f.Search))
	}
	return parts, args
}
func matchTraceFilters(e *Event, f UsageFilter) bool {
	if f.MissingDimension != "" && map[string]string{"account": e.AccountID, "model": e.Model, "provider": e.Provider}[f.MissingDimension] != "" {
		return false
	}
	if f.AccountID != "" && e.AccountID != f.AccountID {
		return false
	}
	unknown := anomalyUnknown(*e)
	failed := !unknown && e.Outcome != "success"
	recovered := !unknown && e.Outcome == "success" && e.HadAnomaly
	switch f.Result {
	case "failed":
		if !failed {
			return false
		}
	case "partial":
		if !failed || !(e.GenerationPartial || e.Outcome == "upstream_truncated") {
			return false
		}
	case "recovered":
		if !recovered {
			return false
		}
	case "unknown":
		if !unknown {
			return false
		}
	case "success":
		if e.Outcome != "success" {
			return false
		}
	case "anomaly":
		if !failed && !recovered {
			return false
		}
	}
	if f.Cause != "" && anomalyCause(*e) != f.Cause {
		return false
	}
	transport := e.TraceTransport
	if transport == "" {
		transport = "unknown"
	}
	if f.Transport != "" && transport != f.Transport {
		return false
	}
	revision := e.TraceRevision
	if revision == "" {
		revision = "unknown"
	}
	if f.Revision != "" && revision != f.Revision {
		return false
	}
	text := strings.Join([]string{e.EventID, e.APIKeyID, e.Provider, e.Model, e.ErrorCode, e.FailureClass, e.TraceRequestID, e.PrimaryCause, e.AccountLabel, e.AccountID, e.UpstreamErrorCode}, " ")
	return f.Search == "" || strings.Contains(strings.ToLower(text), strings.ToLower(f.Search))
}
