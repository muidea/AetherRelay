package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

func TestSummaryLoggingKeepsFailuresAndMakesSuccessDebugOnly(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, tc := range []struct {
		name, message string
		status        int
		estimated     bool
		wantLevel     string
	}{
		{name: "success", status: 200, wantLevel: "DEBUG"},
		{name: "client_error", status: 400, wantLevel: "WARN"},
		{name: "upstream_failure", status: 502, message: "Codex upstream failed", wantLevel: "ERROR"},
		{name: "stream_failure_after_headers", status: 200, message: "upstream read failed", wantLevel: "WARN"},
		{name: "estimated_usage", status: 200, estimated: true, wantLevel: "WARN"},
		{name: "client_canceled", status: 200, message: "client canceled downstream request", wantLevel: "DEBUG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, minimum := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
				var out bytes.Buffer
				slog.SetDefault(slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: minimum})))
				(&Handler{}).printSummary(nil, "event-test", "codexoauth", "gpt-test", true, tc.status, time.Second, tokenUsage{Estimated: tc.estimated}, "", "", "", nil, 0, tc.message)
				if minimum == slog.LevelInfo && tc.wantLevel == "DEBUG" {
					if out.Len() != 0 {
						t.Fatalf("routine summary at INFO: %s", out.String())
					}
					continue
				}
				var record map[string]any
				if err := json.Unmarshal(out.Bytes(), &record); err != nil {
					t.Fatal(err)
				}
				if record["level"] != tc.wantLevel || record["event_id"] != "event-test" || record["status"] != float64(tc.status) {
					t.Fatalf("summary=%v", record)
				}
			}
		})
	}
}
