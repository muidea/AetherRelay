package usage

import (
	"bytes"
	"context"
	"encoding/csv"
	"path/filepath"
	"testing"
	"time"

	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	codexmodel "aetherrelay/internal/pkg/aetherrelaycodexmodel"
)

func TestCodexModelPersistsWithAccountSnapshotAndCSV(t *testing.T) {
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
			if err := store.CreateClientAPIKey(ctx, ClientAPIKeyRecord{ID: "key", Hash: "test", Enabled: true, CreatedAt: at, ProviderAccess: clientaccess.All()}); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"historical", "substituted", "web"} {
				if err := store.Start(ctx, StartRecord{EventID: id, APIKeyID: "key", StartedAt: at, Model: "gpt-6.1-sol"}); err != nil {
					t.Fatal(err)
				}
				rec := CompleteRecord{EventID: id, Provider: "codexoauth", HTTPStatus: 200, Outcome: "success", AccountID: "local-slot", AccountLabel: "operator@example.com"}
				if id != "historical" {
					rec.Observation = codexmodel.New("gpt-6.1-sol", "gpt-6-luna")
				}
				if id == "web" {
					rec.Provider = "chatgptweb"
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
				if e.EventID == "substituted" {
					if !e.ModelSubstituted || e.UpstreamRequestModel != "gpt-6.1-sol" || e.ResponseModel != "gpt-6-luna" || e.AccountLabel != "operator@example.com" || e.AccountID != "local-slot" || e.Outcome != "success" {
						t.Fatal(e)
					}
				} else if e.ResponseModel != "" || e.ModelSubstituted {
					t.Fatal("invented historical/Web evidence", e)
				}
			}
			var output bytes.Buffer
			if err := store.ExportCSV(ctx, UsageFilter{AllTime: true}, &output); err != nil {
				t.Fatal(err)
			}
			rows, err := csv.NewReader(&output).ReadAll()
			if err != nil || len(rows) != 4 {
				t.Fatal(rows, err)
			}
			columns := map[string]int{}
			for i, name := range rows[0] {
				columns[name] = i
			}
			for _, row := range rows[1:] {
				if row[columns["event_id"]] == "substituted" {
					for field, want := range map[string]string{"response_model": "gpt-6-luna", "upstream_request_model": "gpt-6.1-sol", "model_substituted": "true", "account_label": "operator@example.com", "outcome": "success"} {
						if row[columns[field]] != want {
							t.Fatal(field, row)
						}
					}
				}
			}
			if err := store.DeleteClientAPIKey(ctx, "key"); err != nil {
				t.Fatal(err)
			}
			if db, ok := store.(*DuckDBStore); ok {
				var n int
				if err := db.db.QueryRow(`SELECT count(*) FROM usage_codex_model`).Scan(&n); err != nil || n != 0 {
					t.Fatal(n, err)
				}
			}
		})
	}
}

func TestCodexModelAddsTableWithoutBackfillingExistingEvents(t *testing.T) {
	cfg := testCfg(filepath.Join(t.TempDir(), "usage.duckdb"))
	store, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Start(ctx, StartRecord{EventID: "old", APIKeyID: "key", StartedAt: time.Now().UTC(), Provider: "codexoauth", Model: "gpt-6.1-sol"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, CompleteRecord{EventID: "old", Provider: "codexoauth", AccountID: "old-slot", AccountLabel: "old@example.com", Outcome: "success", HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE usage_codex_model`); err != nil {
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
	e := page.Events[0]
	if e.AccountLabel != "old@example.com" || e.Model != "gpt-6.1-sol" || e.ResponseModel != "" || e.UpstreamRequestModel != "" || e.ModelSubstituted {
		t.Fatal(e)
	}
}
