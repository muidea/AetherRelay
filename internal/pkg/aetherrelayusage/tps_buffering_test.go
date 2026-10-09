package usage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

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
	// The deployed TPS version has timings but no buffering table.
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
	if e.TPS == nil || *e.TPS != 25 || e.GenerationBuffered || e.OutputTokens != 25 {
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
	if err != nil || d.Summary.TPSBufferedSamples != 1 || d.Summary.TPS == nil || *d.Summary.TPS != 25 {
		t.Fatal(d, err)
	}
}
