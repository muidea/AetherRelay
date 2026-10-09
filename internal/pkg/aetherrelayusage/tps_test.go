package usage

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTPSWeightedAggregationAndEligibility(t *testing.T) {
	for _, backend := range []string{"memory", "duckdb"} {
		t.Run(backend, func(t *testing.T) {
			var s Store = NewMemoryStore()
			if backend == "duckdb" {
				s = openTestStore(t)
			}
			defer s.Close()
			ctx := context.Background()
			at := time.Now().Add(-time.Hour)
			records := []CompleteRecord{
				{EventID: "a", OutputTokens: 100, OutputTokensKnown: true, FirstOutputAt: at.Add(time.Second), GenerationDuration: 10 * time.Second},
				{EventID: "b", OutputTokens: 900, OutputTokensKnown: true, FirstOutputAt: at.Add(time.Second), GenerationDuration: 30 * time.Second, Estimated: true, GenerationPartial: true, GenerationBuffered: true},
				{EventID: "legacy", OutputTokens: 5000, OutputTokensKnown: true},
				{EventID: "missing-usage", GenerationBuffered: true, FirstOutputAt: at.Add(time.Second), GenerationDuration: time.Second},
				{EventID: "zero-duration", OutputTokens: 500, OutputTokensKnown: true, FirstOutputAt: at.Add(time.Second)},
			}
			for _, r := range records {
				if err := s.Start(ctx, StartRecord{EventID: r.EventID, APIKeyID: "key-a", StartedAt: at}); err != nil {
					t.Fatal(err)
				}
				r.HTTPStatus = 200
				r.Outcome = "success"
				r.CompletedAt = at.Add(time.Minute)
				if err := s.Complete(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			d, err := s.Dashboard(ctx, UsageFilter{AllTime: true})
			if err != nil {
				t.Fatal(err)
			}
			check := func(stats TPSStats) {
				t.Helper()
				if stats.TPS == nil || *stats.TPS != 25 || stats.TPSSamples != 2 || stats.TPSOutputTokens != 1000 || stats.TPSGenerationDurationMS != 40000 || stats.TPSEstimatedSamples != 1 || stats.TPSPartialSamples != 1 || stats.TPSBufferedSamples != 1 {
					t.Fatalf("stats=%+v", stats)
				}
			}
			check(d.Summary.TPSStats)
			if len(d.ByAPIKey) != 1 {
				t.Fatal(d)
			}
			check(d.ByAPIKey[0].TPSStats)
			if len(d.Daily) != 1 {
				t.Fatal(d.Daily)
			}
			check(d.Daily[0].TPSStats)
			if d.Summary.OutputTokens != 6500 || d.Summary.Requests != 5 {
				t.Fatal("ordinary usage totals changed", d.Summary)
			}
			byKey, err := s.AllTimeByKey(ctx)
			if err != nil {
				t.Fatal(err)
			}
			check(byKey["key-a"].TPSStats)
			estimated := true
			filtered, err := s.Dashboard(ctx, UsageFilter{AllTime: true, Estimated: &estimated})
			if err != nil {
				t.Fatal(err)
			}
			if filtered.Summary.TPS == nil || *filtered.Summary.TPS != 30 || filtered.Summary.TPSSamples != 1 || filtered.Summary.TPSBufferedSamples != 1 {
				t.Fatal(filtered)
			}
			empty, err := s.Dashboard(ctx, UsageFilter{AllTime: true, APIKeyID: "absent"})
			if err != nil || empty.Summary.TPS != nil || empty.Summary.TPSSamples != 0 {
				t.Fatal(empty, err)
			}
			page, err := s.Events(ctx, EventFilter{UsageFilter: UsageFilter{AllTime: true}})
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range page.Events {
				switch e.EventID {
				case "a":
					if e.TPS == nil || *e.TPS != 10 || e.FirstOutputAt == nil || e.GenerationDurationMS != 10000 || e.GenerationBuffered {
						t.Fatal(e)
					}
				case "b":
					if e.TPS == nil || *e.TPS != 30 || !e.GenerationPartial || !e.GenerationBuffered {
						t.Fatal(e)
					}
				default:
					if e.TPS != nil || e.GenerationBuffered {
						t.Fatal("unknown sample assigned TPS", e)
					}
				}
			}
			var out bytes.Buffer
			if err := s.ExportCSV(ctx, UsageFilter{AllTime: true}, &out); err != nil {
				t.Fatal(err)
			}
			rows, err := csv.NewReader(&out).ReadAll()
			if err != nil || len(rows) != 6 {
				t.Fatal(rows, err)
			}
			column, bufferedColumn := -1, -1
			for i, name := range rows[0] {
				if name == "generation_buffered" {
					bufferedColumn = i
				}
				if name == "tps" {
					column = i
				}
			}
			if column < 0 || bufferedColumn < 0 {
				t.Fatal(rows)
			}
			for _, row := range rows[1:] {
				if row[0] == "a" && (row[column] != "10" || row[bufferedColumn] != "false") {
					t.Fatal(row)
				}
				if row[0] == "b" && row[bufferedColumn] != "true" {
					t.Fatal(row)
				}
				if row[0] == "legacy" && (row[column] != "" || row[bufferedColumn] != "") {
					t.Fatal(row)
				}
			}
			r := records[0]
			r.HTTPStatus = 200
			r.Outcome = "success"
			if err := s.Complete(ctx, r); !errors.Is(err, ErrEventNotStarted) {
				t.Fatal(err)
			}

		})
	}
}
func TestGenerationSurvivesExistingDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	cfg := testCfg(filepath.Join(t.TempDir(), "existing.duckdb"))
	s, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute)
	if err := s.Start(ctx, StartRecord{EventID: "historic", APIKeyID: "key", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, CompleteRecord{EventID: "historic", HTTPStatus: 200, Outcome: "success", OutputTokens: 100}); err != nil {
		t.Fatal(err)
	}
	// Simulate the previous final version: populated base tables, no timing table.
	if _, err := s.db.Exec(`DROP TABLE usage_generation`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(ctx, StartRecord{EventID: "new", APIKeyID: "key", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	r := CompleteRecord{EventID: "new", HTTPStatus: 200, Outcome: "success", OutputTokens: 25, OutputTokensKnown: true, FirstOutputAt: at, GenerationDuration: time.Second, GenerationBuffered: true}
	if err := s.Complete(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, err := s.Dashboard(ctx, UsageFilter{AllTime: true})
	if err != nil || d.Summary.Requests != 2 || d.Summary.OutputTokens != 125 || d.Summary.TPS == nil || *d.Summary.TPS != 25 || d.Summary.TPSSamples != 1 || d.Summary.TPSBufferedSamples != 1 {
		t.Fatal(d, err)
	}
	if err := s.EnsureClientAPIKey(ctx, "key", at); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteClientAPIKey(ctx, "key"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM usage_generation) + (SELECT count(*) FROM usage_generation_buffering)`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestTPSKnownZeroSubmillisecondAndKeyFiltering(t *testing.T) {
	for _, backend := range []string{"memory", "duckdb"} {
		t.Run(backend, func(t *testing.T) {
			var s Store = NewMemoryStore()
			if backend == "duckdb" {
				s = openTestStore(t)
			}
			defer s.Close()
			ctx := context.Background()
			at := time.Now().Add(-time.Minute)
			for _, r := range []CompleteRecord{{EventID: "zero", OutputTokensKnown: true, GenerationDuration: time.Second}, {EventID: "fast", OutputTokens: 1, OutputTokensKnown: true, GenerationDuration: 250 * time.Microsecond}} {
				if err := s.Start(ctx, StartRecord{EventID: r.EventID, APIKeyID: r.EventID, StartedAt: at}); err != nil {
					t.Fatal(err)
				}
				r.HTTPStatus = 200
				r.Outcome = "success"
				r.FirstOutputAt = at
				if err := s.Complete(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			for key, want := range map[string]float64{"zero": 0, "fast": 4000} {
				d, err := s.Dashboard(ctx, UsageFilter{AllTime: true, APIKeyID: key})
				if err != nil || d.Summary.TPS == nil || *d.Summary.TPS != want || d.Summary.TPSSamples != 1 || len(d.ByAPIKey) != 1 || d.ByAPIKey[0].APIKeyID != key {
					t.Fatal(d, err)
				}
				if len(d.Daily) != 1 || d.Daily[0].TPS == nil || *d.Daily[0].TPS != want || d.Daily[0].TPSSamples != 1 {
					t.Fatal(d.Daily)
				}
			}
		})
	}
}
func TestGenerationCompletionRollsBackAtomically(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().Add(-time.Minute)
	if err := s.Start(ctx, StartRecord{EventID: "event", APIKeyID: "key", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO usage_generation VALUES ('event', ?, 1000000000, false)`, at); err != nil {
		t.Fatal(err)
	}
	rec := CompleteRecord{EventID: "event", HTTPStatus: 200, Outcome: "success", OutputTokens: 10, OutputTokensKnown: true, FirstOutputAt: at, GenerationDuration: time.Second}
	if err := s.Complete(ctx, rec); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatal(err)
	}
	var state string
	var tokens int64
	if err := s.db.QueryRow(`SELECT state, output_tokens FROM usage_events WHERE event_id='event'`).Scan(&state, &tokens); err != nil || state != StateStarted || tokens != 0 {
		t.Fatal(state, tokens, err)
	}
}
