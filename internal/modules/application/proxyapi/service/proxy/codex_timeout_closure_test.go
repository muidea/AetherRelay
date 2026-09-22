package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	archive "aetherrelay/internal/pkg/aetherrelayarchive"
)

func TestCodexTimeoutSettlementRetainsPhaseAndHTTPFacts(t *testing.T) {
	for _, kind := range []codexresponses.ErrorKind{codexresponses.KindFirstEventTimeout, codexresponses.KindIdleTimeout} {
		t.Run(string(kind), func(t *testing.T) {
			attempt := codexArchiveTestAttempt()
			h, root := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{stream: func(_ context.Context, req codexresponses.Request, start func(codexresponses.StreamStart) error, emit func([]byte) error) error {
				req.ObserveAttempt(attempt, nil)
				if kind == codexresponses.KindIdleTimeout {
					if err := start(codexresponses.StreamStart{Attempt: attempt, FirstEventDuration: time.Second}); err != nil {
						return err
					}
					if err := emit([]byte(`data: {"type":"response.created","response":{"id":"test","model":"gpt-5.2-codex"}}`)); err != nil {
						return err
					}
				}
				failure := codexresponses.NewFailure(kind, 5, fmt.Errorf("stream timeout"))
				failure.Attempt = attempt
				req.ObserveAttempt(attempt, failure)
				return failure
			}})
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.2-codex","stream":true,"messages":[{"role":"user","content":"test"}]}`))
			r.Header.Set("Authorization", "Bearer test-client-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			status := 504
			if kind == codexresponses.KindIdleTimeout {
				status = 200
			}
			if w.Code != status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if kind == codexresponses.KindIdleTimeout && (!strings.Contains(w.Body.String(), "event: error") || strings.Contains(w.Body.String(), "event: message_stop")) {
				t.Fatalf("missing error terminal: %s", w.Body.String())
			}
			if kind == codexresponses.KindFirstEventTimeout && streamFailFromCodex(codexresponses.NewFailure(kind, 5, nil)).CountUpstream {
				t.Fatal("request first-event timeout must not trip provider circuit")
			}
			events := usageEvents(t, h.usageStore)
			if len(events) != 1 {
				t.Fatalf("events=%v", events)
			}
			e := events[0]
			if e.Outcome != string(kind) || e.ErrorCode != string(kind) || e.UpstreamStatus != 200 || e.HTTPStatus != status {
				t.Fatalf("event=%+v", e)
			}
			raw, err := os.ReadFile(filepath.Join(root, "test-client", "000001", "metadata.json"))
			if err != nil {
				t.Fatal(err)
			}
			var meta archive.Metadata
			if err = json.Unmarshal(raw, &meta); err != nil {
				t.Fatal(err)
			}
			if meta.ErrorCode != string(kind) || meta.Outcome != string(kind) || meta.UpstreamStatus != 200 || meta.EventID != e.EventID {
				t.Fatalf("metadata=%+v", meta)
			}
		})
	}
}

func TestCodexAttemptArchiveIdempotentAndFinalErrorUpdated(t *testing.T) {
	h, root := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{})
	recorder, err := archive.NewRecorderOptions(root, archive.RecorderOptions{FullContent: true, ScopeByAPIKey: true})
	if err != nil {
		t.Fatal(err)
	}
	h.interactionRecorder = recorder
	round, err := h.startRound("test-client")
	if err != nil {
		t.Fatal(err)
	}
	defer round.Abort()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	attempt := codexArchiveTestAttempt()
	attempt.Request.Body = []byte(`{"model":"test","input":[]}`)
	h.archiveCodexUpstreamAttempt(round, r, "codexoauth", attempt, nil)
	dir := filepath.Join(root, "test-client", "000001")
	old := time.Unix(100, 0)
	for _, name := range []string{"upstream_request_001.body.json", "upstream_request_body.json", "upstream_request_001.json", "upstream_request.json", "upstream_response_001.json", "upstream_response.json"} {
		if err = os.Chtimes(filepath.Join(dir, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	h.archiveCodexUpstreamAttempt(round, r, "codexoauth", attempt, nil)
	for _, name := range []string{"upstream_request_001.body.json", "upstream_request_body.json", "upstream_request_001.json", "upstream_request.json", "upstream_response_001.json", "upstream_response.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.ModTime().Equal(old) {
			t.Fatalf("duplicate rewrite: %s %v", name, err)
		}
	}
	failure := codexresponses.NewFailure(codexresponses.KindFirstEventTimeout, 5, fmt.Errorf("timeout"))
	h.archiveCodexUpstreamAttempt(round, r, "codexoauth", attempt, failure)
	h.archiveCodexUpstreamAttempt(round, r, "codexoauth", attempt, nil)
	raw, err := os.ReadFile(filepath.Join(dir, "upstream_response_001.json"))
	if err != nil {
		t.Fatal(err)
	}
	var response upstreamResponseDebugInfo
	if err = json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != 200 || response.FailureClass != "first_event_timeout" {
		t.Fatalf("response=%+v", response)
	}
	info, _ := os.Stat(filepath.Join(dir, "upstream_request.json"))
	if !info.ModTime().Equal(old) {
		t.Fatal("terminal callback rewrote request")
	}
	next := attempt
	next.Request.At = next.Request.At.Add(time.Second)
	next.Response.Status = 201
	h.archiveCodexUpstreamAttempt(round, r, "codexoauth", next, nil)
	h.archiveCodexUpstreamAttempt(round, r, "codexoauth", attempt, failure)
	if round.UpstreamStatus != 201 || len(round.UpstreamAttempts) != 2 {
		t.Fatal("late callback replaced final attempt")
	}
}
