package usage

import (
	"context"
	"fmt"
	"testing"
	"time"

	codexmodel "aetherrelay/internal/pkg/aetherrelaycodexmodel"
)

func TestRetentionPreservesBoundaryInflightAndClientKeys(t *testing.T) {
	traceStores(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		cutoff := time.Date(2026, 7, 30, 5, 30, 0, 0, time.UTC)
		if err := store.EnsureClientAPIKey(ctx, "key", cutoff.AddDate(0, -1, 0)); err != nil {
			t.Fatal(err)
		}
		for i, name := range []string{"expired", "boundary", "recent", "inflight"} {
			at := cutoff.Add(time.Duration(i-1) * time.Microsecond)
			if name == "inflight" {
				at = cutoff.AddDate(0, -1, 0)
			}
			if err := store.Start(ctx, StartRecord{EventID: name, APIKeyID: "key", Provider: "codexoauth", Model: name, StartedAt: at}); err != nil {
				t.Fatal(err)
			}
			tr := traceFixture(i, true)
			if name == "inflight" {
				if err := store.WriteTrace(ctx, name, tr); err != nil {
					t.Fatal(err)
				}
				continue
			}
			r := CompleteRecord{EventID: name, Provider: "codexoauth", Model: name, CompletedAt: at.Add(time.Second), HTTPStatus: 502, Outcome: "upstream_failed", Trace: &tr, UpstreamError: tr.Attempts[0].UpstreamError, AccountID: "slot", OutputTokensKnown: true, GenerationBuffered: true, FirstOutputAt: at, GenerationDuration: time.Second, GenerationPartial: true, Observation: codexmodel.New("requested", "actual")}
			if err := store.Complete(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		// Populate real caches before sweeping.
		if db, ok := store.(*DuckDBStore); ok {
			db.cache = newDashboardCache(60, 4)
			db.optionsCache = newFilterOptionsCache(60, 4)
		}
		if _, err := store.Dashboard(ctx, UsageFilter{AllTime: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.FilterOptions(ctx, FilterOptionsQuery{AllTime: true}); err != nil {
			t.Fatal(err)
		}
		pruner := store.(interface {
			PruneBefore(context.Context, time.Time) (int64, error)
		})
		if n, err := pruner.PruneBefore(ctx, cutoff); err != nil || n != 1 {
			t.Fatalf("prune=%d %v", n, err)
		}
		if n, err := store.Count(ctx, UsageFilter{AllTime: true}); err != nil || n != 3 {
			t.Fatalf("remaining=%d %v", n, err)
		}
		if dashboard, err := store.Dashboard(ctx, UsageFilter{AllTime: true}); err != nil || dashboard.Summary.Requests != 3 {
			t.Fatalf("stale dashboard: %+v %v", dashboard.Summary, err)
		}
		if options, err := store.FilterOptions(ctx, FilterOptionsQuery{AllTime: true}); err != nil {
			t.Fatal(err)
		} else {
			for _, model := range options.Models {
				if model == "expired" {
					t.Fatal("stale filter options")
				}
			}
		}
		if tr, err := store.ExecutionTrace(ctx, "expired"); err != nil || tr != nil {
			t.Fatalf("expired trace=%+v %v", tr, err)
		}
		if tr, err := store.ExecutionTrace(ctx, "inflight"); err != nil || tr == nil {
			t.Fatalf("active trace=%+v %v", tr, err)
		}
		if n, err := pruner.PruneBefore(ctx, cutoff); err != nil || n != 0 {
			t.Fatalf("second prune=%d %v", n, err)
		}
		keys, err := store.ClientAPIKeyMetadata(ctx)
		if err != nil || keys["key"].CreatedAt.IsZero() {
			t.Fatalf("key metadata removed: %+v %v", keys, err)
		}
		if db, ok := store.(*DuckDBStore); ok {
			for _, table := range []string{"usage_execution_trace", "usage_upstream_error", "usage_codex_model", "usage_account_selection", "usage_output_observation", "usage_generation_buffering", "usage_generation"} {
				var count int
				if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE event_id='expired'`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("orphan %s=%d %v", table, count, err)
				}
				if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE event_id='recent'`).Scan(&count); err != nil || count != 1 {
					t.Fatalf("lost recent %s=%d %v", table, count, err)
				}
			}
		}
		if err := store.Complete(ctx, CompleteRecord{EventID: "inflight", CompletedAt: cutoff, Outcome: "success", HTTPStatus: 200}); err != nil {
			t.Fatal("inflight settlement broken", err)
		}
	})
}

func TestRetentionBatchesAndCanceledSweep(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cutoff := time.Now().UTC()
	// Generate a backlog in one fixture transaction; exercise multiple sweeps'
	// worth of transactions without thousands of fixture API round trips.
	_, err := s.db.ExecContext(ctx, `INSERT INTO usage_events(event_id,started_at,completed_at,usage_date,api_key_id,state,http_status,outcome) SELECT 'old-' || i, ?::TIMESTAMPTZ, ?::TIMESTAMPTZ, ?::DATE, 'key', 'completed', 200, 'success' FROM range(?) t(i)`, cutoff.AddDate(0, -4, 0), cutoff.AddDate(0, -4, 0), cutoff.Format("2006-01-02"), retentionBatchSize+1)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if n, err := s.PruneBefore(canceled, cutoff); err == nil || n != 0 {
		t.Fatalf("canceled sweep=%d %v", n, err)
	}
	if n, err := s.PruneBefore(ctx, cutoff); err != nil || n != retentionBatchSize+1 {
		t.Fatal(fmt.Sprintf("backlog=%d %v", n, err))
	}
}
