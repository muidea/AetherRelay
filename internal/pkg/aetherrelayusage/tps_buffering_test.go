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

func TestBufferingSurvivesWithoutOutputSample(t *testing.T) {
	for _, backend := range []string{"memory", "duckdb"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			cfg := testCfg(filepath.Join(t.TempDir(), "buffered.duckdb"))
			var s Store = NewMemoryStore()
			if backend == "duckdb" {
				var err error
				s, err = OpenDuckDB(cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
			defer func() { s.Close() }()
			at := time.Now().Add(-time.Minute)
			for _, id := range []string{"no-output", "no-usage", "known-zero"} {
				if err := s.Start(ctx, StartRecord{EventID: id, APIKeyID: "key", StartedAt: at}); err != nil {
					t.Fatal(err)
				}
				r := CompleteRecord{EventID: id, HTTPStatus: 200, Outcome: "conversion", Duration: time.Second, GenerationBuffered: true}
				if id == "no-usage" {
					r.FirstOutputAt, r.GenerationDuration = at, time.Second
				}
				if id == "known-zero" {
					r.OutputTokensKnown = true
				}
				if err := s.Complete(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			if backend == "duckdb" {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				var err error
				s, err = OpenDuckDB(cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
			page, err := s.Events(ctx, EventFilter{UsageFilter: UsageFilter{AllTime: true}})
			if err != nil || len(page.Events) != 3 {
				t.Fatal(page, err)
			}
			for _, e := range page.Events {
				if !e.GenerationBuffered || e.ObservedTPS != nil || e.FirstOutputAt != nil || e.GenerationDurationMS != 0 || e.OutputTokensKnown != (e.EventID == "known-zero") || e.EventID != "known-zero" && e.TPS != nil {
					t.Fatalf("buffering lost or output fabricated: %+v", e)
				}
			}
			d, err := s.Dashboard(ctx, UsageFilter{AllTime: true})
			if err != nil || d.Summary.ObservedTPS != nil || d.Summary.ObservedTPSSamples != 0 || d.Summary.ObservedTPSBufferedSamples != 0 {
				t.Fatalf("buffering-only record polluted timing aggregates: %+v, %v", d, err)
			}
			var out bytes.Buffer
			if err := s.ExportCSV(ctx, UsageFilter{AllTime: true}, &out); err != nil {
				t.Fatal(err)
			}
			rows, err := csv.NewReader(&out).ReadAll()
			if err != nil || len(rows) != 4 {
				t.Fatal(rows, err)
			}
			columns := map[string]int{}
			for i, key := range rows[0] {
				columns[key] = i
			}
			for _, row := range rows[1:] {
				if row[columns["generation_buffered"]] != "true" || row[columns["observed_tps"]] != "" || row[columns["first_output_at"]] != "" {
					t.Fatalf("CSV lost buffering or fabricated timing: %v", row)
				}
			}
		})
	}
}

func TestBufferingTableAdditionPreservesExistingTiming(t *testing.T) {
	ctx := context.Background()
	cfg := testCfg(filepath.Join(t.TempDir(), "existing.duckdb"))
	s, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute)
	if err := s.Start(ctx, StartRecord{EventID: "old", APIKeyID: "key", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, CompleteRecord{EventID: "old", HTTPStatus: 200, Outcome: "success", OutputTokens: 25, OutputTokensKnown: true, FirstOutputAt: at, GenerationDuration: time.Second}); err != nil {
		t.Fatal(err)
	}
	// The deployed ObservedTPS version has timings but no buffering table.
	if _, err := s.db.Exec(`DROP TABLE usage_generation_buffering`); err != nil {
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
	if err != nil || len(page.Events) != 1 {
		t.Fatal(page, err)
	}
	e := page.Events[0]
	if e.ObservedTPS == nil || *e.ObservedTPS != 25 || e.GenerationBuffered || e.OutputTokens != 25 {
		t.Fatal(e)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM usage_generation_buffering`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestBufferingWriteFailureRollsBackEntireCompletion(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	at := time.Now().Add(-time.Minute)
	if err := s.Start(ctx, StartRecord{EventID: "event", APIKeyID: "key", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE usage_generation_buffering`); err != nil {
		t.Fatal(err)
	}
	r := CompleteRecord{EventID: "event", HTTPStatus: 200, Outcome: "success", OutputTokens: 25, OutputTokensKnown: true, FirstOutputAt: at, GenerationDuration: time.Second, GenerationBuffered: true}
	if err := s.Complete(ctx, r); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatal(err)
	}
	var state string
	var samples int
	if err := s.db.QueryRow(`SELECT state FROM usage_events WHERE event_id='event'`).Scan(&state); err != nil || state != StateStarted {
		t.Fatal(state, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM usage_generation`).Scan(&samples); err != nil || samples != 0 {
		t.Fatal(samples, err)
	}
	if err := initializeSchema(ctx, s.db); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, r); err != nil {
		t.Fatal(err)
	}
	d, err := s.Dashboard(ctx, UsageFilter{AllTime: true})
	if err != nil || d.Summary.ObservedTPSBufferedSamples != 1 || d.Summary.ObservedTPS == nil || *d.Summary.ObservedTPS != 25 {
		t.Fatal(d, err)
	}
}
