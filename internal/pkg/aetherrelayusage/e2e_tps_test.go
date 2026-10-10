package usage

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func forTPSStores(t *testing.T, run func(*testing.T, Store)) {
	t.Helper()
	for _, backend := range []string{"memory", "duckdb"} {
		t.Run(backend, func(t *testing.T) {
			var s Store = NewMemoryStore()
			if backend == "duckdb" {
				s = openTestStore(t)
			}
			defer s.Close()
			run(t, s)
		})
	}
}

func TestE2ETPSX600NineSamples(t *testing.T) {
	// Raw output-stage nanoseconds and request milliseconds from x600, 2026-10-10 11:43 CST.
	samples := []struct{ round, tokens, ms, ns int64 }{
		{41520, 66, 2339, 248226855}, {41519, 583, 15359, 5881162668},
		{41518, 89, 4112, 1273312492}, {41517, 536, 16296, 93580012},
		{41516, 119, 4660, 1986635536}, {41515, 9, 3199, 329256112},
		{41514, 15, 3341, 500689910}, {41513, 11, 4008, 885110933},
		{41512, 123, 4764, 1433625806},
	}
	forTPSStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		at := time.Now().UTC().Add(-time.Hour)
		wants := map[string]float64{}
		var tokens, ms int64
		for _, sample := range samples {
			id := fmt.Sprint(sample.round)
			wants[id] = float64(sample.tokens) / (float64(sample.ms) / 1000)
			if err := s.Start(ctx, StartRecord{EventID: id, APIKeyID: "claude-owner", RoundID: sample.round, StartedAt: at}); err != nil {
				t.Fatal(err)
			}
			r := CompleteRecord{EventID: id, CompletedAt: at.Add(time.Duration(sample.ms) * time.Millisecond), OutputTokens: sample.tokens, OutputTokensKnown: true, Duration: time.Duration(sample.ms) * time.Millisecond, FirstOutputAt: at.Add(time.Second), GenerationDuration: time.Duration(sample.ns), GenerationBuffered: sample.round != 41520, HTTPStatus: 200, Outcome: "success", Stream: sample.round != 41520}
			if err := s.Complete(ctx, r); err != nil {
				t.Fatal(err)
			}
			tokens += sample.tokens
			ms += sample.ms
		}
		// Simulate pre-upgrade rows: no new observation rows, but the same original usage and timings.
		if db, ok := s.(*DuckDBStore); ok {
			if _, err := db.db.Exec(`DELETE FROM usage_output_observation`); err != nil {
				t.Fatal(err)
			}
		}
		page, err := s.Events(ctx, EventFilter{UsageFilter: UsageFilter{AllTime: true}})
		if err != nil || len(page.Events) != 9 {
			t.Fatal(page, err)
		}
		for _, e := range page.Events {
			if e.TPS == nil || math.Abs(*e.TPS-wants[e.EventID]) > 1e-10 || !e.OutputTokensKnown {
				t.Fatalf("E2E sample %+v", e)
			}
			if e.ObservedTPS == nil || math.Abs(*e.ObservedTPS-float64(e.OutputTokens)/(float64(e.GenerationDurationNS)/1e9)) > 1e-9 {
				t.Fatalf("observed sample %+v", e)
			}
			if e.EventID == "41517" && (fmt.Sprintf("%.2f", *e.TPS) != "32.89" || fmt.Sprintf("%.2f", *e.ObservedTPS) != "5727.72" || !e.GenerationBuffered) {
				t.Fatal(e)
			}
		}
		dash, err := s.Dashboard(ctx, UsageFilter{AllTime: true})
		if err != nil {
			t.Fatal(err)
		}
		all, err := s.AllTimeByKey(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, stats := range []TPSStats{dash.Summary.TPSStats, dash.ByAPIKey[0].TPSStats, dash.Daily[0].TPSStats, all["claude-owner"].TPSStats} {
			if stats.TPS == nil || *stats.TPS != float64(tokens)/(float64(ms)/1000) || stats.TPSSamples != 9 || stats.TPSDurationMS != ms || stats.TPSOutputTokens != tokens || stats.ObservedTPSBufferedSamples != 8 {
				t.Fatalf("aggregates %+v", stats)
			}
		}
		var out bytes.Buffer
		if err := s.ExportCSV(ctx, UsageFilter{AllTime: true}, &out); err != nil {
			t.Fatal(err)
		}
		rows, err := csv.NewReader(&out).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		columns := map[string]int{}
		for i, col := range rows[0] {
			columns[col] = i
		}
		for _, row := range rows[1:] {
			rate, err := strconv.ParseFloat(row[columns["tps"]], 64)
			if err != nil || rate != wants[row[0]] || row[columns["observed_tps"]] == "" || row[columns["output_tokens_known"]] != "true" {
				t.Fatal(row, err)
			}
		}
	})
}

func TestE2ETPSEligibilityAndFilters(t *testing.T) {
	forTPSStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		at := time.Now().UTC().Add(-time.Hour)
		records := []CompleteRecord{
			{EventID: "unary", OutputTokens: 100, OutputTokensKnown: true, Duration: 10 * time.Second, Outcome: "success"},
			{EventID: "zero", OutputTokensKnown: true, Duration: 10 * time.Second, Outcome: "success"},
			{EventID: "partial", OutputTokens: 50, OutputTokensKnown: true, Duration: 5 * time.Second, Outcome: "client_canceled", Estimated: true},
			{EventID: "missing", Duration: time.Second, Outcome: "request_timeout"},
			{EventID: "no-duration", OutputTokens: 100, OutputTokensKnown: true, Outcome: "success"},
			{EventID: "negative-duration", OutputTokens: 100, OutputTokensKnown: true, Duration: -time.Second, Outcome: "success"},
			{EventID: "submillisecond", OutputTokens: 100, OutputTokensKnown: true, Duration: time.Nanosecond, Outcome: "success"},
		}
		for _, r := range records {
			if err := s.Start(ctx, StartRecord{EventID: r.EventID, APIKeyID: "key", StartedAt: at}); err != nil {
				t.Fatal(err)
			}
			r.HTTPStatus = 200
			r.CompletedAt = at.Add(time.Minute)
			if err := s.Complete(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Start(ctx, StartRecord{EventID: "running", APIKeyID: "key", StartedAt: at}); err != nil {
			t.Fatal(err)
		}
		page, err := s.Events(ctx, EventFilter{UsageFilter: UsageFilter{AllTime: true}})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Events {
			switch e.EventID {
			case "unary", "partial":
				if e.TPS == nil || *e.TPS != 10 || e.ObservedTPS != nil {
					t.Fatal(e)
				}
			case "zero":
				if e.TPS == nil || *e.TPS != 0 || !e.OutputTokensKnown {
					t.Fatal(e)
				}
			default:
				if e.TPS != nil {
					t.Fatal(e)
				}
			}
		}
		d, err := s.Dashboard(ctx, UsageFilter{AllTime: true})
		if err != nil {
			t.Fatal(err)
		}
		if d.Summary.TPS == nil || *d.Summary.TPS != 6 || d.Summary.TPSSamples != 3 || d.Summary.TPSPartialSamples != 1 || d.Summary.TPSEstimatedSamples != 1 || d.Summary.ObservedTPS != nil {
			t.Fatal(d)
		}
		estimated := true
		d, err = s.Dashboard(ctx, UsageFilter{AllTime: true, Estimated: &estimated})
		if err != nil || d.Summary.TPS == nil || *d.Summary.TPS != 10 || d.Summary.TPSSamples != 1 {
			t.Fatal(d, err)
		}
		// Completed records cannot be overwritten by retry/duplicate settlement.
		if err := s.Complete(ctx, CompleteRecord{EventID: "unary", HTTPStatus: 200, Outcome: "success", OutputTokens: 9999, Duration: time.Second}); !errors.Is(err, ErrEventNotStarted) {
			t.Fatal(err)
		}
	})
}

func TestE2ETPSKnownZeroSurvivesReopenAndLegacyUnknown(t *testing.T) {
	ctx := context.Background()
	cfg := testCfg(filepath.Join(t.TempDir(), "usage.duckdb"))
	s, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	for _, id := range []string{"zero", "unknown"} {
		if err := s.Start(ctx, StartRecord{EventID: id, APIKeyID: "key", StartedAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := s.Complete(ctx, CompleteRecord{EventID: id, HTTPStatus: 200, Outcome: "success", Duration: time.Second, OutputTokensKnown: id == "zero"}); err != nil {
			t.Fatal(err)
		}
	}
	// A legacy zero with no timing/usage-presence evidence remains unavailable.
	if _, err := s.db.Exec(`DELETE FROM usage_output_observation WHERE event_id='unknown'`); err != nil {
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
	page, err := s.Events(ctx, EventFilter{UsageFilter: UsageFilter{AllTime: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range page.Events {
		if e.EventID == "zero" {
			if e.TPS == nil || *e.TPS != 0 {
				t.Fatal(e)
			}
		} else if e.TPS != nil || e.OutputTokensKnown {
			t.Fatal(e)
		}
	}
}

func TestOutputObservationWriteFailureRollsBackCompletion(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	at := time.Now().UTC()
	if err := s.Start(ctx, StartRecord{EventID: "event", APIKeyID: "key", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE usage_output_observation`); err != nil {
		t.Fatal(err)
	}
	err := s.Complete(ctx, CompleteRecord{EventID: "event", HTTPStatus: 200, Outcome: "success", OutputTokensKnown: true, Duration: time.Second})
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatal(err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM usage_events WHERE event_id='event'`).Scan(&state); err != nil || state != StateStarted {
		t.Fatal(state, err)
	}
}

func TestOutputRateRejectsInvalidInputs(t *testing.T) {
	for _, seconds := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1), math.SmallestNonzeroFloat64} {
		if rate := outputRate(1, seconds, true); rate != nil {
			t.Fatal(rate)
		}
	}
	if outputRate(-1, 1, true) != nil || outputRate(1, 1, false) != nil {
		t.Fatal("invalid usage accepted")
	}
}

func TestE2ETPSConcurrentRequestsRemainIndependent(t *testing.T) {
	forTPSStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		at := time.Now().UTC().Add(-time.Hour)
		var wg sync.WaitGroup
		for i := 1; i <= 12; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				id := strconv.Itoa(i)
				if err := s.Start(ctx, StartRecord{EventID: id, APIKeyID: id, StartedAt: at}); err != nil {
					t.Error(err)
					return
				}
				if err := s.Complete(ctx, CompleteRecord{EventID: id, HTTPStatus: 200, Outcome: "success", OutputTokens: int64(i * i), OutputTokensKnown: true, Duration: time.Duration(i) * time.Second}); err != nil {
					t.Error(err)
				}
			}(i)
		}
		wg.Wait()
		page, err := s.Events(ctx, EventFilter{UsageFilter: UsageFilter{AllTime: true}})
		if err != nil || len(page.Events) != 12 {
			t.Fatal(page, err)
		}
		for _, e := range page.Events {
			i, _ := strconv.Atoi(e.EventID)
			if e.TPS == nil || *e.TPS != float64(i) || e.APIKeyID != e.EventID {
				t.Fatal(e)
			}
		}
	})
}
