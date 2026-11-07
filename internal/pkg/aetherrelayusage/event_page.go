package usage

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

func eventPageIDs(ids []string) (string, []any) {
	if len(ids) == 0 {
		return "false", nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return "event_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")", args
}

// Load bounded side records one table at a time. Even page-restricted joins can
// exhaust a small budget by allocating multiple hash tables simultaneously.
// The caller keeps base rows and all details on the same read snapshot.
func (s *DuckDBStore) loadEventPageDetails(ctx context.Context, tx *sql.Tx, ids []string, events []Event) error {
	byID := make(map[string]*Event, len(events))
	for i := range events {
		byID[events[i].EventID] = &events[i]
	}
	where, args := eventPageIDs(ids)
	read := func(table, columns string, scan func(*sql.Rows) error) error {
		rows, err := tx.QueryContext(ctx, "SELECT event_id, "+columns+" FROM "+table+" WHERE "+where, args...)
		if err != nil {
			return s.queryFailure("events_"+table, "query", err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return s.queryFailure("events_"+table, "scan", err)
			}
		}
		if err := rows.Err(); err != nil {
			return s.queryFailure("events_"+table, "rows", err)
		}
		return nil
	}
	if err := read("usage_generation", "first_output_at,generation_duration_ns,partial", func(r *sql.Rows) error {
		var id string
		var at time.Time
		var ns int64
		var partial bool
		if err := r.Scan(&id, &at, &ns, &partial); err != nil {
			return err
		}
		e := byID[id]
		e.FirstOutputAt = &at
		e.GenerationDurationNS = ns
		e.GenerationPartial = partial
		e.OutputTokensKnown = true
		return nil
	}); err != nil {
		return err
	}
	if err := read("usage_generation_buffering", "true", func(r *sql.Rows) error {
		var id string
		var buffered bool
		if err := r.Scan(&id, &buffered); err != nil {
			return err
		}
		byID[id].GenerationBuffered = buffered
		return nil
	}); err != nil {
		return err
	}
	if err := read("usage_output_observation", "known", func(r *sql.Rows) error {
		var id string
		var known bool
		if err := r.Scan(&id, &known); err != nil {
			return err
		}
		byID[id].OutputTokensKnown = known
		return nil
	}); err != nil {
		return err
	}
	if err := read("usage_account_selection", "account_id,account_label", func(r *sql.Rows) error {
		var id, account, label string
		if err := r.Scan(&id, &account, &label); err != nil {
			return err
		}
		e := byID[id]
		e.AccountID = account
		e.AccountLabel = label
		return nil
	}); err != nil {
		return err
	}
	if err := read("usage_codex_model", "upstream_request_model,response_model,model_substituted", func(r *sql.Rows) error {
		var id, request, response string
		var substituted bool
		if err := r.Scan(&id, &request, &response, &substituted); err != nil {
			return err
		}
		e := byID[id]
		e.UpstreamRequestModel = request
		e.ResponseModel = response
		e.ModelSubstituted = substituted
		return nil
	}); err != nil {
		return err
	}
	if err := read("usage_upstream_error", "upstream_error_type,upstream_error_code,upstream_error_param,upstream_error_message", func(r *sql.Rows) error {
		var id, kind, code, param, message string
		if err := r.Scan(&id, &kind, &code, &param, &message); err != nil {
			return err
		}
		e := byID[id]
		e.UpstreamErrorType = kind
		e.UpstreamErrorCode = code
		e.UpstreamErrorParam = param
		e.UpstreamErrorMessage = message
		return nil
	}); err != nil {
		return err
	}
	if err := read("usage_execution_trace", "evidence_state,attempt_count,failed_attempts,had_anomaly,primary_cause,transport,revision,request_id", func(r *sql.Rows) error {
		var id, state, cause, transport, revision, request string
		var attempts, failed int
		var anomaly bool
		if err := r.Scan(&id, &state, &attempts, &failed, &anomaly, &cause, &transport, &revision, &request); err != nil {
			return err
		}
		e := byID[id]
		e.TraceState = state
		e.AttemptCount = attempts
		e.FailedAttempts = failed
		e.HadAnomaly = anomaly
		e.PrimaryCause = cause
		e.TraceTransport = transport
		e.TraceRevision = revision
		e.TraceRequestID = request
		return nil
	}); err != nil {
		return err
	}
	for i := range events {
		e := &events[i]
		if e.FirstOutputAt != nil {
			known := e.OutputTokensKnown
			e.GenerationSample = makeGenerationSample(e.FirstOutputAt.UTC(), e.GenerationDurationNS, e.OutputTokens, e.GenerationPartial, e.GenerationBuffered, known)
			e.OutputTokensKnown = known
		}
		setE2ETPS(e)
	}
	return nil
}
