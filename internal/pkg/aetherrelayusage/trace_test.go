package usage

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/pkg/aetherrelaycodex"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	trace "aetherrelay/internal/pkg/aetherrelaytrace"
)

func traceStores(t *testing.T, run func(*testing.T, Store)) {
	t.Helper()
	for _, name := range []string{"memory", "duckdb"} {
		t.Run(name, func(t *testing.T) {
			var s Store
			if name == "memory" {
				s = NewMemoryStore()
			} else {
				var err error
				s, err = OpenDuckDB(config.UsageStoreConfig{Path: filepath.Join(t.TempDir(), "trace.duckdb"), MemoryLimit: "128MB", Threads: 1})
				if err != nil {
					t.Fatal(err)
				}
			}
			defer s.Close()
			run(t, s)
		})
	}
}
func traceFixture(n int, failed bool) trace.Trace {
	sent := true
	tr := trace.Trace{Version: 1, Context: trace.Context{RequestID: fmt.Sprintf("request-%d", n), Revision: "rev-one", Instance: "instance"}, Attempts: []trace.Attempt{{ID: 1, Provider: "codexoauth", AccountID: "slot", AccountLabel: "operator@example.com", Model: "model", Finished: true, Transport: "websocket", RequestSent: &sent, EventCount: 1348, WireBytes: 416424, ErrorBodyState: "not_collected"}}}
	if failed {
		tr.Attempts[0].Failed = true
		tr.Attempts[0].FailureClass = "upstream_overloaded"
		tr.Attempts[0].UpstreamError = aetherrelaycodex.UpstreamError{UpstreamErrorType: "service_unavailable_error", UpstreamErrorCode: "server_is_overloaded", UpstreamErrorMessage: "Our servers are currently overloaded. Please try again later."}
	}
	return tr
}
func TestExecutionTraceQueriesExportAndNoBackfill(t *testing.T) {
	traceStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		at := time.Date(2026, 10, 29, 18, 0, 0, 0, time.UTC)
		for i, outcome := range []string{"success", "upstream_truncated", "success", "error", OutcomeProcessInterrupted, "pending"} {
			id := fmt.Sprintf("event-%d", i)
			if err := s.Start(ctx, StartRecord{EventID: id, APIKeyID: "key", Provider: "codexoauth", Model: "model", StartedAt: at.Add(time.Duration(i) * time.Second)}); err != nil {
				t.Fatal(err)
			}
			if outcome == "pending" {
				continue
			}
			r := CompleteRecord{EventID: id, Provider: "codexoauth", AccountID: "slot", AccountLabel: "operator@example.com", HTTPStatus: 200, Outcome: outcome, CompletedAt: at.Add(time.Minute), Duration: time.Minute}
			if i < 3 {
				tr := traceFixture(i, i != 0)
				r.Trace = &tr
			}
			if i == 1 {
				r.GenerationPartial = true
				r.FirstOutputAt = at.Add(time.Second)
				r.GenerationDuration = time.Second
				r.UpstreamError = r.Trace.Attempts[0].UpstreamError
				r.FailureClass = "upstream_overloaded"
			}
			if err := s.Complete(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		f := UsageFilter{AllTime: true, Timezone: "Asia/Singapore"}
		d, err := s.Anomalies(ctx, f, "cause")
		if err != nil {
			t.Fatal(err)
		}
		want := AnomalyCounts{Completed: 4, Success: 2, Failed: 2, Partial: 1, Recovered: 1, Unknown: 2, Affected: 3, MissingTrace: 1, FailedAttempts: 2, Rate: .75}
		if !reflect.DeepEqual(d.Summary, want) {
			t.Fatalf("summary %+v want %+v", d.Summary, want)
		}
		if len(d.Daily) != 1 || d.Daily[0].Key != "2026-10-30" || d.Scope.Timezone != "Asia/Singapore" {
			t.Fatalf("daily %+v", d)
		}
		for _, tc := range []struct {
			name string
			f    UsageFilter
			want int
		}{{"recovered", UsageFilter{Result: "recovered"}, 1}, {"partial", UsageFilter{Result: "partial"}, 1}, {"failed", UsageFilter{Result: "failed"}, 2}, {"unknown", UsageFilter{Result: "unknown"}, 2}, {"anomaly", UsageFilter{Result: "anomaly"}, 3}, {"cause", UsageFilter{Cause: "upstream_overloaded"}, 2}, {"request search", UsageFilter{Search: "REQUEST-1"}, 1}, {"account", UsageFilter{AccountID: "slot"}, 5}, {"revision", UsageFilter{Revision: "rev-one"}, 3}, {"transport", UsageFilter{Transport: "websocket"}, 3}, {"unknown transport", UsageFilter{Transport: "unknown"}, 3}, {"wildcard literal", UsageFilter{Search: "%"}, 0}, {"SQL literal", UsageFilter{Search: "' OR 1=1 --"}, 0}} {
			t.Run(tc.name, func(t *testing.T) {
				tc.f.AllTime = true
				p, e := s.Events(ctx, EventFilter{UsageFilter: tc.f})
				if e != nil || len(p.Events) != tc.want {
					t.Fatalf("events %d want %d err %v", len(p.Events), tc.want, e)
				}
				n, e := s.Count(ctx, tc.f)
				if e != nil || n != int64(tc.want) {
					t.Fatalf("count %d %v", n, e)
				}
				_, e = s.Dashboard(ctx, tc.f)
				if e != nil {
					t.Fatal(e)
				}
			})
		}
		p, err := s.Events(ctx, EventFilter{UsageFilter: f, EventIDs: []string{"event-1"}})
		if err != nil || len(p.Events) != 1 {
			t.Fatal(p, err)
		}
		if p.Events[0].AttemptCount != 1 || p.Events[0].PrimaryCause != "upstream_overloaded" || !p.Events[0].HadAnomaly {
			t.Fatal(p.Events[0])
		}
		tr, err := s.ExecutionTrace(ctx, "event-1")
		if err != nil || tr == nil || tr.Attempts[0].EventCount != 1348 {
			t.Fatal(tr, err)
		}
		*tr.Attempts[0].RequestSent = false
		again, _ := s.ExecutionTrace(ctx, "event-1")
		if !*again.Attempts[0].RequestSent {
			t.Fatal("store snapshot mutated")
		}
		old, err := s.ExecutionTrace(ctx, "event-3")
		if err != nil || old != nil {
			t.Fatal(old, err)
		}
		var buf bytes.Buffer
		if err = s.ExportCSV(ctx, UsageFilter{AllTime: true, Cause: "upstream_overloaded"}, &buf); err != nil {
			t.Fatal(err)
		}
		rows, err := csv.NewReader(&buf).ReadAll()
		if err != nil || len(rows) != 3 {
			t.Fatal(rows, err)
		}
		for _, column := range []string{"trace_state", "attempt_count", "primary_cause", "trace_revision", "upstream_error_message"} {
			if !strings.Contains(strings.Join(rows[0], ","), column) {
				t.Fatal(column)
			}
		}
		if len(rows[0]) != len(rows[1]) {
			t.Fatal("CSV column mismatch")
		}
		for _, group := range []string{"account", "model", "revision", "transport", "provider"} {
			out, err := s.Anomalies(ctx, f, group)
			if err != nil || out.Summary != want {
				t.Fatalf("%s %+v %v", group, out, err)
			}
		}
	})
}
func TestTraceCheckpointSurvivesReopenAndCannotOverwriteSettled(t *testing.T) {
	cfg := config.UsageStoreConfig{Path: filepath.Join(t.TempDir(), "trace.duckdb"), MemoryLimit: "128MB", Threads: 1}
	s, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = s.Start(ctx, StartRecord{EventID: "inflight", APIKeyID: "key"}); err != nil {
		t.Fatal(err)
	}
	tr := traceFixture(1, true)
	if err = s.WriteTrace(ctx, "inflight", tr); err != nil {
		t.Fatal(err)
	}
	if err = s.WriteTrace(ctx, "missing", tr); err != ErrEventNotStarted {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.ExecutionTrace(ctx, "inflight")
	if err != nil || got == nil || got.Attempts[0].UpstreamErrorCode != "server_is_overloaded" {
		t.Fatal(got, err)
	}
	if err = s.WriteTrace(ctx, "inflight", trace.Trace{}); err != ErrEventNotStarted {
		t.Fatal(err)
	}
	d, err := s.Anomalies(ctx, UsageFilter{AllTime: true}, "cause")
	if err != nil || d.Summary.Unknown != 1 || d.Summary.Failed != 0 || d.Summary.Completed != 0 {
		t.Fatal(d, err)
	}
}
func TestTraceDeletionAndCompletionRollback(t *testing.T) {
	traceStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		if err := s.EnsureClientAPIKey(ctx, "key", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(ctx, StartRecord{EventID: "event", APIKeyID: "key"}); err != nil {
			t.Fatal(err)
		}
		tr := traceFixture(1, true)
		if err := s.WriteTrace(ctx, "event", tr); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteClientAPIKey(ctx, "key"); err != nil {
			t.Fatal(err)
		}
		got, err := s.ExecutionTrace(ctx, "event")
		if err != nil || got != nil {
			t.Fatal(got, err)
		}
	})
}
func TestTraceCompletionIsTransactional(t *testing.T) {
	ctx := context.Background()
	s, err := OpenDuckDB(config.UsageStoreConfig{Path: filepath.Join(t.TempDir(), "trace.duckdb"), MemoryLimit: "128MB", Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Start(ctx, StartRecord{EventID: "event", APIKeyID: "key"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`DROP TABLE usage_execution_trace`)
	if err != nil {
		t.Fatal(err)
	}
	tr := traceFixture(1, true)
	if err = s.Complete(ctx, CompleteRecord{EventID: "event", HTTPStatus: 200, Outcome: "success", Trace: &tr}); err == nil {
		t.Fatal("expected failure")
	}
	var state string
	if err = s.db.QueryRow(`SELECT state FROM usage_events WHERE event_id='event'`).Scan(&state); err != nil || state != StateStarted {
		t.Fatal(state, err)
	}
}

// A missing value is distinct from a real value literally named "unknown".
func TestExecutionTraceMissingDimensionDrilldown(t *testing.T) {
	traceStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		for i, v := range []string{"", "unknown", "present"} {
			id := fmt.Sprint(i)
			if err := s.Start(ctx, StartRecord{EventID: id, APIKeyID: "key", Provider: v, Model: v}); err != nil {
				t.Fatal(err)
			}
			if err := s.Complete(ctx, CompleteRecord{EventID: id, Provider: v, Model: v, AccountID: v, Outcome: "error", HTTPStatus: 502}); err != nil {
				t.Fatal(err)
			}
		}
		for _, group := range []string{"account", "model", "provider"} {
			d, err := s.Anomalies(ctx, UsageFilter{AllTime: true}, group)
			if err != nil || len(d.Groups) != 3 {
				t.Fatalf("%s %+v %v", group, d, err)
			}
			f := UsageFilter{AllTime: true, MissingDimension: group}
			p, err := s.Events(ctx, EventFilter{UsageFilter: f})
			if err != nil || len(p.Events) != 1 || p.Events[0].EventID != "0" {
				t.Fatalf("%s %+v %v", group, p, err)
			}
			n, err := s.Count(ctx, f)
			if err != nil || n != 1 {
				t.Fatal(n, err)
			}
			var b bytes.Buffer
			if err = s.ExportCSV(ctx, f, &b); err != nil {
				t.Fatal(err)
			}
			csvRows, err := csv.NewReader(&b).ReadAll()
			if err != nil || len(csvRows) != 2 || csvRows[1][0] != "0" {
				t.Fatal(csvRows, err)
			}
		}
	})
}

func TestTraceCheckpointInvalidatesDiagnosticDashboardFilters(t *testing.T) {
	traceStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		if err := s.Start(ctx, StartRecord{EventID: "pending", APIKeyID: "key"}); err != nil {
			t.Fatal(err)
		}
		f := UsageFilter{AllTime: true, Search: "checkpoint-request-id"}
		before, err := s.Dashboard(ctx, f)
		if err != nil || before.Summary.Requests != 0 {
			t.Fatal(before, err)
		}
		if err = s.WriteTrace(ctx, "pending", trace.Trace{Context: trace.Context{RequestID: f.Search}}); err != nil {
			t.Fatal(err)
		}
		after, err := s.Dashboard(ctx, f)
		if err != nil || after.Summary.Requests != 1 {
			t.Fatalf("stale diagnostic dashboard %+v %v", after, err)
		}
	})
}
