package proxy

import (
	"bytes"
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
	"aetherrelay/internal/pkg/aetherrelaycodex"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

func TestCodexOverloadDiagnosticsReachUsageAndArchive(t *testing.T) {
	for _, afterOutput := range []bool{false, true} {
		for _, archived := range []bool{false, true} {
			t.Run(fmt.Sprintf("after-output=%v/archive=%v", afterOutput, archived), func(t *testing.T) {
				failure := codexresponses.NewFailure(codexresponses.KindUpstream, 0, fmt.Errorf("Codex upstream failed"))
				failure.UpstreamType = "service_unavailable_error"
				failure.UpstreamCode = "server_is_overloaded"
				failure.UpstreamMessage = "Our servers are currently overloaded. Please try again later."
				failure.Attempt = codexArchiveTestAttempt()
				failure.Attempt.Response.Status = 101
				if afterOutput {
					no := false
					failure.Retryable = &no
					failure.Attempt.Response.FirstOutputAt = time.Now().Add(-time.Second)
					failure.Attempt.Response.GenerationDuration = time.Second
					failure.Attempt.Response.GenerationPartial = true
				}
				calls := 0
				executor := codexResponsesExecutorStub{stream: func(_ context.Context, request codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
					calls++
					if afterOutput {
						if err := started(codexresponses.StreamStart{Attempt: failure.Attempt}); err != nil {
							return err
						}
						if err := emit([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")); err != nil {
							return err
						}
					}
					if request.ObserveAttempt != nil {
						request.ObserveAttempt(failure.Attempt, failure)
					}
					return failure
				}}
				var handler *Handler
				var dir string
				if archived {
					handler, dir = newArchivedCodexResponsesHandler(t, executor)
				} else {
					handler = newCodexResponsesHandler(t, usage.NewMemoryStore(), executor)
				}
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.2-codex","stream":true,"input":"hello"}`))
				request.Header.Set("Authorization", "Bearer test-client-key")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				wantStatus, wantOutcome := 502, "upstream_failed"
				if afterOutput {
					wantStatus, wantOutcome = 200, "upstream_truncated"
				}
				if response.Code != wantStatus || calls != 1 || strings.Contains(response.Body.String(), "server_is_overloaded") {
					t.Fatalf("status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
				}
				events := usageEvents(t, handler.usageStore)
				if len(events) != 1 {
					t.Fatal(events)
				}
				e := events[0]
				diagnostic := aetherrelaycodex.UpstreamError{UpstreamErrorType: failure.UpstreamType, UpstreamErrorCode: failure.UpstreamCode, UpstreamErrorMessage: failure.UpstreamMessage}
				if e.UpstreamError != diagnostic || e.FailureClass != "upstream_overloaded" || e.ErrorCode != "upstream" || e.Outcome != wantOutcome || e.OutputTokensKnown || e.Estimated || e.CachedInputTokensKnown {
					t.Fatalf("incorrect settlement: %+v", e)
				}
				if afterOutput && (e.Retryable == nil || *e.Retryable || !e.GenerationPartial || !strings.Contains(response.Body.String(), "partial")) {
					t.Fatalf("unsafe post-output failure: %+v", e)
				}
				if archived {
					paths, err := filepath.Glob(filepath.Join(dir, "*", "*", "metadata.json"))
					if err != nil || len(paths) != 1 {
						t.Fatal(paths, err)
					}
					raw, err := os.ReadFile(paths[0])
					if err != nil {
						t.Fatal(err)
					}
					var meta map[string]any
					if err := json.Unmarshal(raw, &meta); err != nil {
						t.Fatal(err)
					}

					rawAttempt, err := os.ReadFile(filepath.Join(filepath.Dir(paths[0]), "upstream_response.json"))
					if err != nil {
						t.Fatal(err)
					}
					var attemptMeta map[string]any
					if err := json.Unmarshal(rawAttempt, &attemptMeta); err != nil {
						t.Fatal(err)
					}
					if attemptMeta["upstream_error_code"] != e.UpstreamErrorCode || attemptMeta["failure_class"] != e.FailureClass {
						t.Fatalf("upstream metadata differs: %s", rawAttempt)
					}
					if meta["upstream_error_code"] != e.UpstreamErrorCode || meta["upstream_error_type"] != e.UpstreamErrorType || meta["upstream_error_message"] != e.UpstreamErrorMessage || meta["failure_class"] != e.FailureClass || meta["outcome"] != e.Outcome {
						t.Fatalf("metadata differs: %s", raw)
					}
				}
			})
		}
	}
}

func TestCodexFailureDiagnosticsDoNotChangeOtherClassification(t *testing.T) {
	for _, tc := range []struct {
		kind               codexresponses.ErrorKind
		code, reason, want string
	}{
		{codexresponses.KindUpstream, "server_is_overloaded", "", "upstream_overloaded"},
		{codexresponses.KindUpstream, "server_is_overloaded_extra", "", ""},
		{codexresponses.KindNetwork, "", "", ""},
		{codexresponses.KindRateLimit, "usage_limit_reached", "", ""},
		{codexresponses.KindInvalidRequest, "invalid_request", "", ""},
		{codexresponses.KindProviderUnavailable, "server_is_overloaded", "accounts_cooling", "accounts_cooling"},
	} {
		f := codexresponses.NewFailure(tc.kind, 0, fmt.Errorf("failed"))
		f.UpstreamCode = tc.code
		f.UnavailableReason = tc.reason
		got := streamFailFromCodex(f)
		if got.FailureClass != tc.want || got.ErrorCode != string(tc.kind) || got.UpstreamErrorCode != tc.code || f.Retryable != nil {
			t.Fatalf("kind=%s failure=%+v", tc.kind, got)
		}
	}
}
