package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	archive "aetherrelay/internal/pkg/aetherrelayarchive"
)

func TestCodexFinalBodyArchiveAndAttemptHistory(t *testing.T) {
	for _, full := range []bool{false, true} {
		for _, unredacted := range []bool{false, true} {
			t.Run(fmt.Sprintf("content=%t/plain=%t", full, unredacted), func(t *testing.T) {
				h, _ := newArchiveFidelityHandler(t, unredacted, codexResponsesExecutorStub{})
				recorder, err := archive.NewRecorderOptions(t.TempDir(), archive.RecorderOptions{MaxRounds: 10, FullContent: full})
				if err != nil {
					t.Fatal(err)
				}
				round, err := recorder.Start()
				if err != nil {
					t.Fatal(err)
				}
				defer round.Abort()
				r := httptest.NewRequest("POST", "/v1/messages", nil)
				body := []byte(`{"input":[],"client_metadata":{"session_id":"session-private","x-codex-installation-id":"installation-private","x-codex-window-id":"window-private","x-codex-turn-metadata":"embedded-private"}}`)
				attempt := codexArchiveTestAttempt()
				attempt.Request.At = time.Now()
				attempt.Request.Body = body
				attempt.Request.BodyBytes = len(body)
				observer := h.codexAttemptObserver(round, r, "codexoauth")
				observer(attempt, fmt.Errorf("overload"))
				attempt.Request.At = attempt.Request.At.Add(time.Second)
				observer(attempt, nil)
				// A handshake and final settlement refer to the same attempt.
				observer(attempt, codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("terminal failure")))
				if len(round.UpstreamAttempts) != 2 {
					t.Fatalf("duplicate attempts=%d", len(round.UpstreamAttempts))
				}
				for _, name := range []string{"upstream_request_001.json", "upstream_response_001.json", "upstream_request_002.json", "upstream_response_002.json"} {
					if _, err := os.Stat(filepath.Join(round.Dir, name)); err != nil {
						t.Fatal(err)
					}
				}
				for _, name := range []string{"upstream_request_body.json", "upstream_request_001.body.json", "upstream_request_002.body.json"} {
					data, err := os.ReadFile(filepath.Join(round.Dir, name))
					if !full {
						if !os.IsNotExist(err) {
							t.Fatalf("body recorded with content disabled: %s", name)
						}
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(data), "private") != unredacted {
						t.Fatalf("body redaction mismatch: %s", data)
					}
					if unredacted && string(data) != string(body) {
						t.Fatal("wire body changed in fidelity mode")
					}
				}
				first, _ := os.ReadFile(filepath.Join(round.Dir, "upstream_response_001.json"))
				last, _ := os.ReadFile(filepath.Join(round.Dir, "upstream_response_002.json"))
				if !strings.Contains(string(first), "overload") || !strings.Contains(string(last), "terminal failure") {
					t.Fatal("attempt failures overwritten")
				}
			})
		}
	}
}

func TestCodexArchiveRedactionPreservesBusinessNumbers(t *testing.T) {
	h, _ := newArchiveFidelityHandler(t, false, codexResponsesExecutorStub{})
	body := []byte(`{"input":[{"arguments":{"integer":9007199254740993,"session_id":"business-value"}}],"client_metadata":{"session_id":"private"}}`)
	result := string(h.codexArchiveBody(body))
	if !strings.Contains(result, "9007199254740993") || !strings.Contains(result, "business-value") || strings.Contains(result, "private") {
		t.Fatalf("archive corrupted business data: %s", result)
	}
}

func TestLocalFailureArchivesNoFictitiousUpstream(t *testing.T) {
	for _, kind := range []string{"conversion", "cooling"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			h, root := newArchiveFidelityHandler(t, false, codexResponsesExecutorStub{complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
				calls++
				if kind == "conversion" {
					t.Fatal("local rejection reached upstream")
				}
				failure := codexresponses.NewFailure(codexresponses.KindProviderUnavailable, 29, fmt.Errorf("account unavailable"))
				failure.UnavailableReason = "accounts_cooling"
				retryable := true
				failure.Retryable = &retryable
				return codexresponses.Result{}, failure
			}})
			raw := claudeToolContinuation
			if kind == "conversion" {
				raw = strings.ReplaceAll(raw, `"is_error":false`, `"is_error":"invalid"`)
			}
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(raw))
			r.Header.Set("Authorization", "Bearer test-client-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			dir := filepath.Join(root, "test-client", "000001")
			data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
			if err != nil {
				t.Fatal(err)
			}
			var meta archive.Metadata
			if err := json.Unmarshal(data, &meta); err != nil {
				t.Fatal(err)
			}
			if kind == "conversion" {
				if w.Code != 400 || calls != 0 || meta.ConversionErrorPath != "messages[2].content[0].is_error" {
					t.Fatalf("status=%d meta=%+v", w.Code, meta)
				}
			} else if w.Code != 503 || w.Header().Get("Retry-After") != "29" || meta.FailureClass != "accounts_cooling" || meta.Retryable == nil || !*meta.Retryable || meta.RetryAfterSeconds != 29 {
				t.Fatalf("status=%d meta=%+v", w.Code, meta)
			}
			for _, name := range []string{"upstream_request.json", "upstream_request_body.json", "upstream_request_001.json"} {
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("fake upstream archive %s", name)
				}
			}
		})
	}
}
