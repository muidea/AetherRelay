package usage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestObservationPresenceAcrossStores(t *testing.T) {
	for _, backend := range []string{"memory", "duckdb"} {
		t.Run(backend, func(t *testing.T) {
			var store Store = NewMemoryStore()
			if backend == "duckdb" {
				store = openTestStore(t)
			}
			t.Cleanup(func() { _ = store.Close() })
			ctx := context.Background()
			for i, known := range []bool{false, true} {
				id := fmt.Sprint(i)
				at := time.Now()
				if err := store.Start(ctx, StartRecord{EventID: id, APIKeyID: id, StartedAt: at}); err != nil {
					t.Fatal(err)
				}
				length := int64(-1)
				if known {
					length = 0
				}
				if err := store.Complete(ctx, CompleteRecord{EventID: id, CompletedAt: at.Add(time.Second), InputTokens: 100, HTTPStatus: 200, Outcome: "success", CachedInputTokensKnown: known, CacheCreationInputTokensKnown: known, UpstreamStatus: 200, UpstreamContentLength: length}); err != nil {
					t.Fatal(err)
				}
				page, err := store.Events(ctx, EventFilter{UsageFilter: UsageFilter{AllTime: true, APIKeyID: id}})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Events) != 1 {
					t.Fatalf("events=%+v", page)
				}
				e := page.Events[0]
				if e.CachedInputTokensKnown != known || e.CacheCreationInputTokensKnown != known || e.UpstreamContentLengthKnown != known || e.UpstreamContentLength != 0 {
					t.Fatalf("event=%+v", e)
				}
				dash, err := store.Dashboard(ctx, UsageFilter{AllTime: true, APIKeyID: id})
				if err != nil {
					t.Fatal(err)
				}
				if dash.Summary.CachedInputTokensKnown != known || dash.Summary.CacheCreationInputTokensKnown != known {
					t.Fatalf("summary=%+v", dash.Summary)
				}
				var exported bytes.Buffer
				if err = store.ExportCSV(ctx, UsageFilter{AllTime: true, APIKeyID: id}, &exported); err != nil {
					t.Fatal(err)
				}
				rows, err := csv.NewReader(&exported).ReadAll()
				if err != nil || len(rows) != 2 {
					t.Fatalf("CSV=%v err=%v", rows, err)
				}
				last := len(rows[0]) - 1
				if rows[0][last] != "cache_creation_input_tokens_known" || rows[1][last] != strconv.FormatBool(known) || rows[1][last-1] != strconv.FormatBool(known) {
					t.Fatalf("CSV presence lost: %v", rows)
				}
			}
			dash, err := store.Dashboard(ctx, UsageFilter{AllTime: true})
			if err != nil {
				t.Fatal(err)
			}
			if dash.Summary.CachedInputTokensKnown || dash.Summary.CacheCreationInputTokensKnown {
				t.Fatal("partial observations presented as complete")
			}
		})
	}
}

func TestCacheAggregationExcludesFailedObservationsButPreservesDetails(t *testing.T) {
	for _, backend := range []string{"memory", "duckdb"} {
		t.Run(backend, func(t *testing.T) {
			var store Store = NewMemoryStore()
			if backend == "duckdb" {
				store = openTestStore(t)
			}
			t.Cleanup(func() { _ = store.Close() })

			ctx := context.Background()
			at := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
			for _, rec := range []struct {
				id, outcome             string
				input, cached, creation int64
				httpStatus              int
				known                   bool
			}{
				{id: "success", outcome: "success", input: 100, cached: 80, creation: 0, httpStatus: 200, known: true},
				{id: "timeout", outcome: "first_event_timeout", input: 25, cached: 0, creation: 0, httpStatus: 504, known: false},
			} {
				if err := store.Start(ctx, StartRecord{EventID: rec.id, APIKeyID: "key", StartedAt: at}); err != nil {
					t.Fatal(err)
				}
				if err := store.Complete(ctx, CompleteRecord{
					EventID: rec.id, CompletedAt: at.Add(time.Second), Outcome: rec.outcome,
					HTTPStatus:  rec.httpStatus,
					InputTokens: rec.input, CachedInputTokens: rec.cached, CacheCreationInputTokens: rec.creation,
					CachedInputTokensKnown: rec.known, CacheCreationInputTokensKnown: rec.known,
				}); err != nil {
					t.Fatal(err)
				}
			}

			dash, err := store.Dashboard(ctx, UsageFilter{AllTime: true})
			if err != nil {
				t.Fatal(err)
			}
			if dash.Summary.Requests != 2 || dash.Summary.FailedRequests != 1 || dash.Summary.InputTokens != 125 {
				t.Fatalf("request accounting changed: %+v", dash.Summary)
			}
			if dash.Summary.CacheInputTokens != 100 || dash.Summary.CachedInputTokens != 80 || dash.Summary.CacheCreationInputTokens != 0 || !dash.Summary.CachedInputTokensKnown || !dash.Summary.CacheCreationInputTokensKnown || dash.Summary.CacheHitRate != 0.8 {
				t.Fatalf("failed observation polluted cache aggregation: %+v", dash.Summary)
			}

			page, err := store.Events(ctx, EventFilter{UsageFilter: UsageFilter{AllTime: true}, PageSize: 10})
			if err != nil {
				t.Fatal(err)
			}
			var failed *Event
			for i := range page.Events {
				if page.Events[i].EventID == "timeout" {
					failed = &page.Events[i]
					break
				}
			}
			if failed == nil || failed.InputTokens != 25 || failed.CachedInputTokens != 0 || failed.CacheCreationInputTokens != 0 || failed.CachedInputTokensKnown || failed.CacheCreationInputTokensKnown {
				t.Fatalf("failed event detail was not preserved: %+v", page.Events)
			}
		})
	}
}

func TestObservationColumnsPreserveExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.duckdb")
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The base schema is the immediately preceding layout, including its indexes.
	if err = createSchema(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO usage_events(event_id,api_key_id,started_at,completed_at,usage_date,input_tokens,total_tokens,http_status,outcome,state) VALUES ('old','key',now(),now(),current_date,100,100,200,'success','completed')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenDuckDB(testCfg(path))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.Events(ctx, EventFilter{UsageFilter: UsageFilter{AllTime: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].InputTokens != 100 || page.Events[0].CachedInputTokensKnown || page.Events[0].CacheCreationInputTokensKnown {
		t.Fatalf("history changed: %+v", page)
	}
}
