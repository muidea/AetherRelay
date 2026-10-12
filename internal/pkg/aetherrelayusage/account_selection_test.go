package usage

import (
	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	"bytes"
	"context"
	"encoding/csv"
	"path/filepath"
	"testing"
	"time"
)

// CP-OBS-013: detail and CSV preserve the account snapshot independently of
// the live account pool, across restart and alongside older unknown records.
func TestAccountSelectionPersistenceAndExport(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"memory", "duckdb"} {
		t.Run(kind, func(t *testing.T) {
			var store Store
			cfg := testCfg(filepath.Join(t.TempDir(), "usage.duckdb"))
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
			if err := store.CreateClientAPIKey(ctx, ClientAPIKeyRecord{ID: "key", Hash: "sha256:test", Enabled: true, CreatedAt: at, ProviderAccess: clientaccess.All()}); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"historical", "selected"} {
				if err := store.Start(ctx, StartRecord{EventID: id, APIKeyID: "key", StartedAt: at}); err != nil {
					t.Fatal(err)
				}
				rec := CompleteRecord{EventID: id, Provider: "codexoauth", HTTPStatus: 200, Outcome: "success", CompletedAt: at.Add(time.Second)}
				if id == "selected" {
					rec.AccountID, rec.AccountLabel = "local-slot", "operator@example.com"
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
			if err != nil || len(page.Events) != 2 {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			for _, e := range page.Events {
				if e.EventID == "selected" && (e.Provider != "codexoauth" || e.AccountID != "local-slot" || e.AccountLabel != "operator@example.com") {
					t.Fatalf("lost account snapshot: %+v", e)
				}
				if e.EventID == "historical" && (e.AccountID != "" || e.AccountLabel != "") {
					t.Fatalf("invented old selection: %+v", e)
				}
			}
			var output bytes.Buffer
			if err := store.ExportCSV(ctx, UsageFilter{AllTime: true}, &output); err != nil {
				t.Fatal(err)
			}
			rows, err := csv.NewReader(&output).ReadAll()
			if err != nil || len(rows) != 3 {
				t.Fatalf("csv rows=%v err=%v", rows, err)
			}
			columns := map[string]int{}
			for i, name := range rows[0] {
				columns[name] = i
			}
			for _, row := range rows[1:] {
				if row[columns["event_id"]] == "selected" && (row[columns["account_id"]] != "local-slot" || row[columns["account_label"]] != "operator@example.com") {
					t.Fatalf("CSV lost snapshot: %v", row)
				}
			}
			if err := store.DeleteClientAPIKey(ctx, "key"); err != nil {
				t.Fatal(err)
			}
			if db, ok := store.(*DuckDBStore); ok {
				var count int
				if err := db.db.QueryRow(`SELECT count(*) FROM usage_account_selection`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("orphan selections=%d err=%v", count, err)
				}
			}
		})
	}
}

// CP-OBS-013: upgrading a final-layout database only adds an observation table;
// existing event columns and rows are preserved without guessing account data.
func TestAccountSelectionAddsTableWithoutChangingExistingEvents(t *testing.T) {
	cfg := testCfg(filepath.Join(t.TempDir(), "existing.duckdb"))
	store, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Start(ctx, StartRecord{EventID: "old", APIKeyID: "key", Provider: "codexoauth"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, CompleteRecord{EventID: "old", HTTPStatus: 200, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE usage_account_selection`); err != nil {
		t.Fatal(err)
	}
	const layout = `SELECT string_agg(column_name || ':' || data_type, ',' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_name='usage_events'`
	var before string
	if err := store.db.QueryRow(layout).Scan(&before); err != nil {
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
	var after string
	if err := store.db.QueryRow(layout).Scan(&after); err != nil || before != after {
		t.Fatalf("event layout changed: err=%v", err)
	}
	page, err := store.Events(ctx, EventFilter{UsageFilter: UsageFilter{AllTime: true}})
	if err != nil || len(page.Events) != 1 || page.Events[0].Provider != "codexoauth" || page.Events[0].AccountID != "" {
		t.Fatalf("existing event changed: %+v err=%v", page, err)
	}
}
