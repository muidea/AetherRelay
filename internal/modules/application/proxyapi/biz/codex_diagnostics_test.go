package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	transport "aetherrelay/internal/pkg/aetherrelaytransport"
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

func TestTransportReasonLogsWithoutArchiveOrPrivateText(t *testing.T) {
	previous := slog.Default()
	defer slog.SetDefault(previous)
	for _, reason := range []transport.Reason{transport.ConnectionReset, "private-proxy:private-password", ""} {
		var output bytes.Buffer
		slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
		failure := codexresponses.NewFailure(codexresponses.KindNetwork, 0, nil)
		failure.Attempt.Response.TransportReason = reason
		// No recorder or archive observer is needed for these WARN records.
		request := codexresponses.Request{Model: "gpt-test", AccountAttempt: 2, Diagnostics: codexresponses.Diagnostics{RequestID: "request-test"}}
		_, guard := newCodexStreamGuard(context.Background(), 0, 0, 0)
		defer guard.close()
		logCodexStreamAttempt(request, failure, "pull", guard)
		lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n"))
		if len(lines) != 2 {
			t.Fatalf("log records=%d", len(lines))
		}
		for _, line := range lines {
			var got map[string]any
			if err := json.Unmarshal(line, &got); err != nil {
				t.Fatal(err)
			}
			if got["transport_reason"] != string(reason.Safe()) || got["request_id"] != "request-test" || got["account_attempt"] != float64(2) || got["error_class"] != "network" {
				t.Fatalf("diagnostic=%v", got)
			}
		}
		if strings.Contains(output.String(), "private-") {
			t.Fatalf("unsafe log: %s", output.String())
		}
	}
}

func TestStreamFailureReasonPreservesResponseObservation(t *testing.T) {
	attempt := codexresponses.HTTPAttempt{Request: codexresponses.HTTPRequestObservation{At: time.Now(), URL: "https://example.test"}, Response: codexresponses.HTTPResponseObservation{Observed: true, Status: 200, DurationMS: 17}}
	// Invalid enum values are normalized before reaching the adapter or log.
	update := upevents.PullResult{Done: true, ErrorClass: upevents.ErrorNetwork, TransportReason: "private-host"}
	failure := codexStreamFailure(update, attempt)
	if failure.Kind != codexresponses.KindNetwork || failure.Attempt.Response.TransportReason != transport.Unknown || !failure.Attempt.Response.Observed || failure.Attempt.Response.Status != 200 || failure.Attempt.Response.DurationMS != 17 || attempt.Response.TransportReason != "" {
		t.Fatalf("failure=%+v", failure)
	}
}
