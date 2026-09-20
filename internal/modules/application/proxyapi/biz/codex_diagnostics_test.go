package biz

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
)

func TestAttemptDiagnosticKeepsObservedHTTPStatus(t *testing.T) {
	previous := slog.Default()
	defer slog.SetDefault(previous)
	for _, observed := range []bool{true, false} {
		var output bytes.Buffer
		slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
		failure := codexresponses.NewFailure(codexresponses.KindFirstEventTimeout, 5, nil)
		failure.Attempt.Response = codexresponses.HTTPResponseObservation{Observed: observed, Status: 200}
		logCodexAttempt(codexresponses.Request{Model: "test"}, failure)
		var got map[string]any
		if err := json.Unmarshal(output.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		want := float64(0)
		if observed {
			want = 200
		}
		if got["upstream_status"] != want || got["error_class"] != "first_event_timeout" || failure.HTTPStatus != 0 {
			t.Fatalf("diagnostic=%v failure=%+v", got, failure)
		}
	}
}
