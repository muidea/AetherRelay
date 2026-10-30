package usage

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	clientaccess "aetherrelay/internal/pkg/aetherrelayclientaccess"
	"aetherrelay/internal/pkg/aetherrelaycodex"
)

func TestUpstreamErrorObservationPersistsExportsAndDeletes(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"memory", "duckdb"} {
		t.Run(kind, func(t *testing.T) {
			cfg := testCfg(filepath.Join(t.TempDir(), "usage.duckdb"))
			var store Store
			if kind == "memory" {
				store = NewMemoryStore()
			} else {
				var err error
				store, err = OpenDuckDB(cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
			defer func() { _ = store.Close() }()
			at := time.Now().UTC().Add(-time.Minute)
			if err := store.CreateClientAPIKey(ctx, ClientAPIKeyRecord{ID: "key", Hash: "test", Enabled: true, CreatedAt: at, ProviderAccess: clientaccess.All()}); err != nil {
				t.Fatal(err)
			}
			diagnostic := aetherrelaycodex.UpstreamError{UpstreamErrorType: "service_unavailable_error", UpstreamErrorCode: "server_is_overloaded", UpstreamErrorMessage: "Our servers are currently overloaded. Please try again later."}
			no := false
			for _, id := range []string{"overload", "network", "historical"} {
				if err := store.Start(ctx, StartRecord{EventID: id, APIKeyID: "key", StartedAt: at}); err != nil {
					t.Fatal(err)
				}
				rec := CompleteRecord{EventID: id, Provider: "codexoauth", HTTPStatus: 200, Outcome: "upstream_truncated", ErrorCode: "upstream", FailureClass: "upstream_overloaded", Retryable: &no, UpstreamStatus: 101, FirstOutputAt: at.Add(time.Second), GenerationDuration: time.Second, GenerationPartial: true}
				if id == "overload" {
					rec.UpstreamError = diagnostic
				} else if id == "network" {
					rec.ErrorCode = "network"
					rec.FailureClass = ""
				}
				if err := store.Complete(ctx, rec); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "duckdb" {
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				var err error
				store, err = OpenDuckDB(cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
			page, err := store.Events(ctx, EventFilter{UsageFilter: UsageFilter{AllTime: true}, PageSize: 10})
			if err != nil || len(page.Events) != 3 {
				t.Fatal(page, err)
			}
			for _, e := range page.Events {
				if e.EventID == "overload" {
					if e.UpstreamError != diagnostic || e.ErrorCode != "upstream" || e.FailureClass != "upstream_overloaded" || e.HTTPStatus != 200 || e.UpstreamStatus != 101 || e.Retryable == nil || *e.Retryable || !e.GenerationPartial || e.OutputTokensKnown || e.CachedInputTokensKnown {
						t.Fatalf("lost failure or invented usage: %+v", e)
					}
					raw, _ := json.Marshal(e)
					if !bytes.Contains(raw, []byte(`"upstream_error_code":"server_is_overloaded"`)) {
						t.Fatalf("missing API fields: %s", raw)
					}
				} else if e.UpstreamError != (aetherrelaycodex.UpstreamError{}) {
					t.Fatalf("invented failure evidence: %+v", e)
				}
			}
			var out bytes.Buffer
			if err := store.ExportCSV(ctx, UsageFilter{AllTime: true}, &out); err != nil {
				t.Fatal(err)
			}
			rows, err := csv.NewReader(&out).ReadAll()
			if err != nil || len(rows) != 4 {
				t.Fatal(rows, err)
			}
			columns := map[string]int{}
			for i, name := range rows[0] {
				columns[name] = i
			}
			for _, row := range rows[1:] {
				code, message := "", ""
				if row[columns["event_id"]] == "overload" {
					code, message = diagnostic.UpstreamErrorCode, diagnostic.UpstreamErrorMessage
				}
				if row[columns["upstream_error_code"]] != code || row[columns["upstream_error_message"]] != message {
					t.Fatal(row)
				}
			}
			if err := store.DeleteClientAPIKey(ctx, "key"); err != nil {
				t.Fatal(err)
			}
			if db, ok := store.(*DuckDBStore); ok {
				var n int
				if err := db.db.QueryRow(`SELECT count(*) FROM usage_upstream_error`).Scan(&n); err != nil || n != 0 {
					t.Fatal(n, err)
				}
			}
		})
	}
}

func TestUpstreamErrorObservationAddsTableWithoutHistoryRewrite(t *testing.T) {
	ctx := context.Background()
	cfg := testCfg(filepath.Join(t.TempDir(), "usage.duckdb"))
	store, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Start(ctx, StartRecord{EventID: "old", APIKeyID: "key", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, CompleteRecord{EventID: "old", HTTPStatus: 200, Outcome: "upstream_truncated", ErrorCode: "upstream"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE usage_upstream_error`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	page, err := store.Events(ctx, EventFilter{UsageFilter: UsageFilter{AllTime: true}, PageSize: 10})
	if err != nil || len(page.Events) != 1 {
		t.Fatal(page, err)
	}
	if e := page.Events[0]; e.ErrorCode != "upstream" || e.Outcome != "upstream_truncated" || e.UpstreamError != (aetherrelaycodex.UpstreamError{}) {
		t.Fatalf("history rewritten: %+v", e)
	}
}

func TestUpstreamErrorObservationWriteFailureRollsBackCompletion(t *testing.T) {
	ctx := context.Background()
	store, err := OpenDuckDB(testCfg(filepath.Join(t.TempDir(), "usage.duckdb")))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Start(ctx, StartRecord{EventID: "rollback", APIKeyID: "key", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE usage_upstream_error`); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, CompleteRecord{EventID: "rollback", HTTPStatus: 502, Outcome: "upstream_failed", UpstreamError: aetherrelaycodex.UpstreamError{UpstreamErrorCode: "server_is_overloaded"}}); err == nil {
		t.Fatal("missing table did not fail transaction")
	}
	var state string
	if err := store.db.QueryRow(`SELECT state FROM usage_events WHERE event_id='rollback'`).Scan(&state); err != nil || state != StateStarted {
		t.Fatal(state, err)
	}
}
