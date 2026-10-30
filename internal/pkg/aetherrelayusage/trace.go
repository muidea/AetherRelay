package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	trace "aetherrelay/internal/pkg/aetherrelaytrace"
)

func saveExecutionTrace(ctx context.Context, tx *sql.Tx, id string, t trace.Trace) error {
	t = trace.Bounded(t)
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if len(b) > trace.MaxTraceBytes {
		return fmt.Errorf("execution trace exceeds size limit")
	}
	s := t.Summary()
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_execution_trace (event_id,request_id,revision,instance,attempt_count,failed_attempts,had_anomaly,primary_cause,transport,evidence_state,payload) VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(event_id) DO UPDATE SET request_id=excluded.request_id,revision=excluded.revision,instance=excluded.instance,attempt_count=excluded.attempt_count,failed_attempts=excluded.failed_attempts,had_anomaly=excluded.had_anomaly,primary_cause=excluded.primary_cause,transport=excluded.transport,evidence_state=excluded.evidence_state,payload=excluded.payload`, id, t.Context.RequestID, t.Context.Revision, t.Context.Instance, s.AttemptCount, s.FailedAttempts, s.HadAnomaly, s.PrimaryCause, s.TraceTransport, s.TraceState, string(b))
	return err
}

// WriteTrace checkpoints bounded evidence before request completion. It cannot
// create orphan records or overwrite evidence after final settlement.
func (s *DuckDBStore) WriteTrace(ctx context.Context, id string, t trace.Trace) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	s.write.Lock()
	defer s.write.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStoreUnavailable
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM usage_events WHERE event_id=?`, id).Scan(&state); err != nil || state != StateStarted {
		return ErrEventNotStarted
	}
	if err = saveExecutionTrace(ctx, tx, id, t); err != nil {
		return fmt.Errorf("%w: trace: %v", ErrStoreUnavailable, err)
	}
	if err = tx.Commit(); err != nil {
		return ErrStoreUnavailable
	}
	s.cache.clear() // request IDs and diagnostic filters can change before settlement
	return nil
}
func (s *DuckDBStore) ExecutionTrace(ctx context.Context, id string) (*trace.Trace, error) {
	if s.closed.Load() {
		return nil, ErrStoreClosed
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM usage_execution_trace WHERE event_id=?`, id).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, ErrStoreUnavailable
	}
	var t trace.Trace
	if json.Unmarshal([]byte(payload), &t) != nil {
		return nil, ErrStoreUnavailable
	}
	return &t, nil
}
func (s *MemoryStore) WriteTrace(_ context.Context, id string, t trace.Trace) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.events[id]
	if e == nil || e.State != StateStarted {
		return ErrEventNotStarted
	}
	t = trace.Bounded(t)
	s.traces[id] = t
	e.Summary = t.Summary()
	return nil
}
func (s *MemoryStore) ExecutionTrace(_ context.Context, id string) (*trace.Trace, error) {
	if s.closed.Load() {
		return nil, ErrStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.traces[id]
	if !ok {
		return nil, nil
	}
	t = trace.Clone(t)
	return &t, nil
}

type AnomalyCounts struct {
	Completed      int64   `json:"completed"`
	Success        int64   `json:"success"`
	Failed         int64   `json:"failed"`
	Partial        int64   `json:"partial"`
	Recovered      int64   `json:"recovered"`
	Unknown        int64   `json:"unknown"`
	Affected       int64   `json:"affected"`
	MissingTrace   int64   `json:"missing_trace"`
	FailedAttempts int64   `json:"failed_attempts"`
	Rate           float64 `json:"rate"`
}
type AnomalyGroup struct {
	Key string `json:"key"`
	AnomalyCounts
}
type AnomalyDashboard struct {
	Scope     ScopeInfo      `json:"scope"`
	GroupBy   string         `json:"group_by"`
	Summary   AnomalyCounts  `json:"summary"`
	Groups    []AnomalyGroup `json:"groups"`
	Daily     []AnomalyGroup `json:"daily"`
	Truncated bool           `json:"truncated"`
}

func validateGroup(v string) error {
	switch v {
	case "cause", "account", "model", "revision", "transport", "provider":
		return nil
	}
	return fmt.Errorf("invalid group_by")
}
func finishAnomaly(c *AnomalyCounts) {
	c.Affected = c.Failed + c.Recovered
	if c.Completed > 0 {
		c.Rate = float64(c.Affected) / float64(c.Completed)
	}
}
func anomalyCause(e Event) string {
	if e.PrimaryCause != "" {
		return e.PrimaryCause
	}
	if e.UpstreamErrorCode != "" {
		return e.UpstreamErrorCode
	}
	if e.FailureClass != "" {
		return e.FailureClass
	}
	if e.ErrorCode != "" {
		return e.ErrorCode
	}
	return "unknown"
}
func anomalyUnknown(e Event) bool {
	return e.State != StateCompleted || e.Outcome == OutcomeProcessInterrupted
}
func addAnomaly(c *AnomalyCounts, e Event) {
	if anomalyUnknown(e) {
		c.Unknown++
		return
	}
	c.Completed++
	if e.Outcome == "success" {
		c.Success++
		if e.HadAnomaly {
			c.Recovered++
		}
	} else {
		c.Failed++
		if e.GenerationPartial || e.Outcome == "upstream_truncated" {
			c.Partial++
		}
	}
	if e.TraceState == "" || e.TraceState == "not_collected" {
		c.MissingTrace++
	}
	c.FailedAttempts += int64(e.FailedAttempts)
	finishAnomaly(c)
}
func rangeForTrace(f UsageFilter) (time.Time, time.Time) {
	from, to := f.From.UTC(), f.To.UTC()
	if f.AllTime {
		if from.IsZero() {
			from = time.Unix(0, 0).UTC()
		}
		if to.IsZero() {
			to = time.Now().UTC().Add(24 * time.Hour)
		}
	}
	return from, to
}
func traceLocation(f UsageFilter) *time.Location {
	if f.Timezone == "Asia/Singapore" {
		return time.FixedZone("Asia/Singapore", 8*3600)
	}
	return time.UTC
}
func (s *MemoryStore) Anomalies(_ context.Context, f UsageFilter, group string) (AnomalyDashboard, error) {
	if s.closed.Load() {
		return AnomalyDashboard{}, ErrStoreClosed
	}
	if err := ValidateUsageFilter(&f); err != nil {
		return AnomalyDashboard{}, err
	}
	if err := validateGroup(group); err != nil {
		return AnomalyDashboard{}, err
	}
	from, to := rangeForTrace(f)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := AnomalyDashboard{Scope: ScopeInfo{From: from, To: to, Timezone: traceLocation(f).String()}, GroupBy: group, Groups: []AnomalyGroup{}, Daily: []AnomalyGroup{}}
	groups := map[string]*AnomalyCounts{}
	daily := map[string]*AnomalyCounts{}
	for _, e := range s.events {
		if !matchEvent(e, f, from, to) {
			continue
		}
		addAnomaly(&out.Summary, *e)
		day := e.StartedAt.In(traceLocation(f)).Format("2006-01-02")
		if daily[day] == nil {
			daily[day] = &AnomalyCounts{}
		}
		addAnomaly(daily[day], *e)
		key := anomalyCause(*e)
		switch group {
		case "account":
			key = e.AccountID
		case "model":
			key = e.Model
		case "revision":
			key = s.traces[e.EventID].Context.Revision
		case "transport":
			key = e.TraceTransport
		case "provider":
			key = e.Provider
		}
		if key == "" && (group == "revision" || group == "transport") {
			key = "unknown"
		}
		if groups[key] == nil {
			groups[key] = &AnomalyCounts{}
		}
		addAnomaly(groups[key], *e)
	}
	for k, v := range groups {
		if v.Affected > 0 || v.Unknown > 0 {
			out.Groups = append(out.Groups, AnomalyGroup{Key: k, AnomalyCounts: *v})
		}
	}
	for k, v := range daily {
		out.Daily = append(out.Daily, AnomalyGroup{Key: k, AnomalyCounts: *v})
	}
	sort.Slice(out.Groups, func(i, j int) bool {
		a, b := out.Groups[i], out.Groups[j]
		if a.Affected != b.Affected {
			return a.Affected > b.Affected
		}
		return a.Key < b.Key
	})
	if len(out.Groups) > 200 {
		out.Groups = out.Groups[:200]
		out.Truncated = true
	}
	sort.Slice(out.Daily, func(i, j int) bool { return out.Daily[i].Key < out.Daily[j].Key })
	return out, nil
}

const anomalyEventSource = `(SELECT usage_events.*, coalesce(t.evidence_state,'not_collected') AS trace_state, coalesce(t.attempt_count,0) AS attempt_count, coalesce(t.failed_attempts,0) AS failed_attempts, coalesce(t.had_anomaly,false) AS had_anomaly, coalesce(t.primary_cause,'') AS primary_cause, coalesce(t.transport,'') AS trace_transport, coalesce(t.revision,'') AS trace_revision, coalesce(t.request_id,'') AS trace_request_id FROM ` + targetEvents + ` LEFT JOIN usage_execution_trace t ON usage_events.event_id=t.event_id) AS usage_events`
const anomalyAggregates = `count(*) FILTER(WHERE state='completed' AND outcome IS DISTINCT FROM 'process_interrupted'), count(*) FILTER(WHERE state='completed' AND outcome='success'), count(*) FILTER(WHERE state='completed' AND outcome IS DISTINCT FROM 'success' AND outcome IS DISTINCT FROM 'process_interrupted'), count(*) FILTER(WHERE state='completed' AND outcome IS DISTINCT FROM 'success' AND outcome IS DISTINCT FROM 'process_interrupted' AND (coalesce(generation_partial,false) OR outcome='upstream_truncated')), count(*) FILTER(WHERE state='completed' AND outcome='success' AND had_anomaly), count(*) FILTER(WHERE state IS DISTINCT FROM 'completed' OR outcome='process_interrupted'), count(*) FILTER(WHERE state='completed' AND outcome IS DISTINCT FROM 'process_interrupted' AND trace_state='not_collected'), coalesce(sum(failed_attempts) FILTER(WHERE state='completed' AND outcome IS DISTINCT FROM 'process_interrupted'),0)`

func scanAnomaly(row interface{ Scan(...any) error }, prefix []any) (AnomalyCounts, error) {
	var c AnomalyCounts
	dest := append(prefix, &c.Completed, &c.Success, &c.Failed, &c.Partial, &c.Recovered, &c.Unknown, &c.MissingTrace, &c.FailedAttempts)
	err := row.Scan(dest...)
	finishAnomaly(&c)
	return c, err
}
func (s *DuckDBStore) Anomalies(ctx context.Context, f UsageFilter, group string) (AnomalyDashboard, error) {
	if s.closed.Load() {
		return AnomalyDashboard{}, ErrStoreClosed
	}
	if err := ValidateUsageFilter(&f); err != nil {
		return AnomalyDashboard{}, err
	}
	if err := validateGroup(group); err != nil {
		return AnomalyDashboard{}, err
	}
	from, to := rangeForTrace(f)
	where, args := buildFilterWhere(f, from, to)
	out := AnomalyDashboard{Scope: ScopeInfo{From: from, To: to, Timezone: traceLocation(f).String()}, GroupBy: group, Groups: []AnomalyGroup{}, Daily: []AnomalyGroup{}}
	// A single read transaction gives summary, groups and timeline one snapshot.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("%w: anomaly query: %v", ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()
	out.Summary, err = scanAnomaly(tx.QueryRowContext(ctx, `SELECT `+anomalyAggregates+` FROM `+anomalyEventSource+` WHERE `+where, args...), nil)
	if err != nil {
		return out, fmt.Errorf("%w: anomaly summary: %v", ErrStoreUnavailable, err)
	}
	column := map[string]string{"cause": "coalesce(nullif(primary_cause,''),nullif(upstream_error_code,''),nullif(failure_class,''),nullif(error_code,''),'unknown')", "account": "coalesce(account_id,'')", "model": "coalesce(model,'')", "revision": "coalesce(nullif(trace_revision,''),'unknown')", "transport": "coalesce(nullif(trace_transport,''),'unknown')", "provider": "coalesce(provider,'')"}[group]
	rows, err := tx.QueryContext(ctx, `SELECT `+column+` AS group_key, `+anomalyAggregates+` FROM `+anomalyEventSource+` WHERE `+where+` GROUP BY group_key HAVING count(*) FILTER(WHERE state='completed' AND ((outcome IS DISTINCT FROM 'success' AND outcome IS DISTINCT FROM 'process_interrupted') OR (outcome='success' AND had_anomaly)))>0 OR count(*) FILTER(WHERE state IS DISTINCT FROM 'completed' OR outcome='process_interrupted')>0 ORDER BY (count(*) FILTER(WHERE state='completed' AND ((outcome IS DISTINCT FROM 'success' AND outcome IS DISTINCT FROM 'process_interrupted') OR (outcome='success' AND had_anomaly)))) DESC, group_key LIMIT 201`, args...)
	if err != nil {
		return out, fmt.Errorf("%w: anomaly groups: %v", ErrStoreUnavailable, err)
	}
	for rows.Next() {
		var key string
		c, e := scanAnomaly(rows, []any{&key})
		if e != nil {
			rows.Close()
			return out, fmt.Errorf("%w: anomaly scan: %v", ErrStoreUnavailable, e)
		}
		out.Groups = append(out.Groups, AnomalyGroup{Key: key, AnomalyCounts: c})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, fmt.Errorf("%w: anomaly query: %v", ErrStoreUnavailable, err)
	}
	if len(out.Groups) > 200 {
		out.Groups = out.Groups[:200]
		out.Truncated = true
	}
	day := `cast(timezone('UTC',started_at) AS DATE)`
	if f.Timezone == "Asia/Singapore" {
		day = `cast(timezone('UTC',started_at)+INTERVAL '8 hours' AS DATE)`
	}
	rows, err = tx.QueryContext(ctx, `SELECT cast(`+day+` AS VARCHAR) AS day, `+anomalyAggregates+` FROM `+anomalyEventSource+` WHERE `+where+` GROUP BY day ORDER BY day`, args...)
	if err != nil {
		return out, fmt.Errorf("%w: anomaly query: %v", ErrStoreUnavailable, err)
	}
	for rows.Next() {
		var key string
		c, e := scanAnomaly(rows, []any{&key})
		if e != nil {
			rows.Close()
			return out, fmt.Errorf("%w: anomaly scan: %v", ErrStoreUnavailable, e)
		}
		out.Daily = append(out.Daily, AnomalyGroup{Key: key, AnomalyCounts: c})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, fmt.Errorf("%w: anomaly query: %v", ErrStoreUnavailable, err)
	}
	if err = tx.Commit(); err != nil {
		return out, fmt.Errorf("%w: anomaly query: %v", ErrStoreUnavailable, err)
	}
	return out, nil
}
