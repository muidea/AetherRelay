package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

func TestCodexModelNotFoundEnvelopeAndDiagnostics(t *testing.T) {
	// CP-FAIL-018 / CP-OBS-006: archive-independent HTTP/SSE/compact behavior.
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", path, stream), func(t *testing.T) {
				failure := codexresponses.NewFailure(codexresponses.KindModelNotFound, 0, fmt.Errorf("unavailable"))
				failure.HTTPStatus = 404
				failure.UpstreamCode = "model_not_found"
				failure.UpstreamType = "invalid_request_error"
				failure.UpstreamMessage = "The selected model is unavailable"
				var received codexresponses.Request
				h := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{
					complete: func(_ context.Context, request codexresponses.Request) (codexresponses.Result, error) {
						received = request
						return codexresponses.Result{}, failure
					},
					stream: func(_ context.Context, request codexresponses.Request, _ func(codexresponses.StreamStart) error, _ func([]byte) error) error {
						received = request
						return failure
					},
				})
				metadata := `{"request_kind":"compaction","compaction":{"reason":"comp_hash_changed","phase":"pre_turn"},"session_id":"secret-session"}`
				body, _ := json.Marshal(map[string]any{"model": "gpt-5.2-codex", "input": []any{}, "stream": stream, "client_metadata": map[string]any{"x-codex-turn-metadata": metadata}})
				r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
				r.Header.Set("Authorization", "Bearer test-client-key")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				wantStatus := http.StatusNotFound
				if path == "/v1/responses/compact" && stream {
					wantStatus = http.StatusOK
				} // CP-COMPACT-003: heartbeat already committed.
				if w.Code != wantStatus || !bytes.Contains(w.Body.Bytes(), []byte(`"code":"model_not_found"`)) || (wantStatus == 404 && !bytes.Contains(w.Body.Bytes(), []byte(failure.UpstreamMessage))) {
					t.Fatalf("%d %s", w.Code, w.Body.String())
				}
				if wantStatus == http.StatusOK && !bytes.Contains(w.Body.Bytes(), []byte("event: response.failed")) {
					t.Fatal("missing compact failure terminal")
				}
				if received.Diagnostics.RequestKind != "compaction" || received.Diagnostics.CompactionReason != "comp_hash_changed" || received.Diagnostics.RequestID == "" {
					t.Fatalf("trace=%+v", received.Diagnostics)
				}
				if bytes.Contains(received.Body, []byte("secret-session")) {
					t.Fatal("metadata leaked upstream")
				}
			})
		}
	}
}
