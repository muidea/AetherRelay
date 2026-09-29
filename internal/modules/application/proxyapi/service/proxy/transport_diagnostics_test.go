package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	archive "aetherrelay/internal/pkg/aetherrelayarchive"
	transport "aetherrelay/internal/pkg/aetherrelaytransport"
)

func TestTransportReasonArchiveUpdatesSameAttempt(t *testing.T) {
	for _, observed := range []bool{false, true} {
		for _, reason := range []transport.Reason{transport.ConnectionReset, "private-proxy:private-password"} {
			h, _ := newArchiveFidelityHandler(t, false, codexResponsesExecutorStub{})
			recorder, err := archive.NewRecorderOptions(t.TempDir(), archive.RecorderOptions{MaxRounds: 10})
			if err != nil {
				t.Fatal(err)
			}
			round, err := recorder.Start()
			if err != nil {
				t.Fatal(err)
			}
			defer round.Abort()
			r := httptest.NewRequest("POST", "/v1/messages", nil)
			attempt := codexArchiveTestAttempt()
			attempt.Response.Observed = observed
			if !observed {
				attempt.Response.Status = 0
				attempt.Response.Headers = nil
			}
			h.archiveCodexUpstreamAttempt(round, r, "codexoauth", attempt, nil)
			failure := codexresponses.NewFailure(codexresponses.KindNetwork, 0, nil)
			attempt.Response.TransportReason = reason
			failure.Attempt = attempt
			h.archiveCodexUpstreamAttempt(round, r, "codexoauth", attempt, failure)
			// Duplicate settlement and a late handshake must retain the cause.
			h.archiveCodexUpstreamAttempt(round, r, "codexoauth", attempt, failure)
			attempt.Response.TransportReason = ""
			h.archiveCodexUpstreamAttempt(round, r, "codexoauth", attempt, nil)
			if len(round.UpstreamAttempts) != 1 {
				t.Fatal("terminal update created another attempt")
			}
			for _, name := range []string{"upstream_response.json", "upstream_response_001.json"} {
				data, err := os.ReadFile(filepath.Join(round.Dir, name))
				if err != nil {
					t.Fatal(err)
				}
				var got upstreamResponseDebugInfo
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				if got.TransportReason != reason.Safe() || got.FailureClass != "network" || got.Status != attempt.Response.Status || got.Attempt != 1 || strings.Contains(string(data), "private-") {
					t.Fatalf("archive=%s", data)
				}
			}
		}
	}
}

func TestTransportReasonDoesNotChangeClientError(t *testing.T) {
	h, _ := newArchiveFidelityHandler(t, false, codexResponsesExecutorStub{})
	var baseline string
	for _, reason := range []transport.Reason{"", transport.ConnectionReset, "private-proxy:private-password"} {
		failure := codexresponses.NewFailure(codexresponses.KindNetwork, 0, nil)
		failure.Attempt.Response.TransportReason = reason
		response := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/responses", nil)
		h.writeCodexResponsesError(response, r, nil, time.Now(), "codexoauth", "gpt-test", false, failure)
		if response.Code != 502 {
			t.Fatalf("status=%d", response.Code)
		}
		if baseline == "" {
			baseline = response.Body.String()
		}
		if response.Body.String() != baseline || strings.Contains(response.Body.String(), "transport_reason") || strings.Contains(response.Body.String(), "private-") {
			t.Fatalf("client envelope changed: %s", response.Body.String())
		}
	}
}
