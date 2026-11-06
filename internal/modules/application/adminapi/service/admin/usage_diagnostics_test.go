package admin

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

type failedUsageQueryStore struct{ usage.Store }

func (failedUsageQueryStore) Events(context.Context, usage.EventFilter) (usage.EventPage, error) {
	return usage.EventPage{}, fmt.Errorf("secret-sql-value: %w", context.DeadlineExceeded)
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
