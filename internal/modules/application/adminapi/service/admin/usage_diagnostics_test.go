package admin

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	config "aetherrelay/internal/pkg/aetherrelayconfig"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

type failedUsageQueryStore struct{ usage.Store }

func (failedUsageQueryStore) Events(context.Context, usage.EventFilter) (usage.EventPage, error) {
	return usage.EventPage{}, fmt.Errorf("secret-sql-value: %w", context.DeadlineExceeded)
}

func TestAdminUsageQueryLogsRealDriverClassAndQueryStage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.duckdb")
	store, err := usage.OpenDuckDB(config.UsageStoreConfig{Path: path, MemoryLimit: "256MB", Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE usage_generation`); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	h := NewHandlerWithUsage("", &fakeRuntime{}, store)
	r := httptest.NewRequest("GET", "/admin/api/usage/events?search=secret-search-value", nil)
	w := httptest.NewRecorder()
	h.usageEvents(w, r)
	if w.Code != 503 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, field := range []string{`"reason":"duckdb_catalog"`, `"query_operation":"events_usage_generation"`, `"query_stage":"query"`, `"duckdb_error_code":`, `"db_pool_in_use":`} {
		if !strings.Contains(logs.String(), field) {
			t.Fatalf("missing %s: %s", field, logs.String())
		}
	}
	if strings.Contains(logs.String(), "secret-") || strings.Contains(logs.String(), "Catalog Error") || strings.Contains(w.Body.String(), "usage_generation") {
		t.Fatal("query value or driver message leaked")
	}
}

func TestAdminUsageQueryFailureLogExcludesSearchAndDriverError(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	h := NewHandlerWithUsage("", &fakeRuntime{}, failedUsageQueryStore{usage.NewMemoryStore()})
	r := httptest.NewRequest("GET", "/admin/api/usage/events?search=secret-search-value", nil)
	w := httptest.NewRecorder()
	h.usageEvents(w, r)
	if w.Code != 503 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, field := range []string{`"endpoint":"/admin/api/usage/events"`, `"stage":"events"`, `"reason":"deadline_exceeded"`} {
		if !strings.Contains(logs.String(), field) {
			t.Fatalf("missing %s: %s", field, logs.String())
		}
	}
	if strings.Contains(logs.String(), "secret-") {
		t.Fatal("sensitive query or driver value leaked")
	}
}
