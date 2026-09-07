package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

func TestCacheWriteUsageParsing(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		read, write  int
	}{
		{"responses", `"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":20}`, 40, 20},
		{"explicit zero", `"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":0}`, 40, 0},
		{"missing write is not cache miss", `"input_tokens_details":{"cached_tokens":40}`, 40, 0},
		{"missing details", `"unused":null`, 0, 0},
		{"invalid write", `"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":"20"}`, 40, 0},
		{"null write", `"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":null}`, 40, 0},
		{"anthropic compatibility", `"cache_read_input_tokens":40,"cache_creation_input_tokens":20`, 40, 20},
		{"legacy details", `"input_tokens_details":{"cache_read_tokens":40,"cache_creation_tokens":20}`, 40, 20},
		{"canonical write wins", `"cache_creation_input_tokens":10,"input_tokens_details":{"cache_creation_tokens":15,"cache_write_tokens":20}`, 0, 20},
		{"canonical zero wins", `"cache_creation_input_tokens":10,"input_tokens_details":{"cache_creation_tokens":15,"cache_write_tokens":0}`, 0, 0},
		{"invalid canonical keeps alias", `"input_tokens_details":{"cache_creation_tokens":20,"cache_write_tokens":null}`, 0, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"input_tokens":100,"output_tokens":5,"total_tokens":105,` + tc.fields + `}`
			parsed, ok := usageFromRaw(json.RawMessage(raw))
			if !ok || !parsed.Known || parsed.Estimated || parsed.PromptTokens != 100 || parsed.CompletionTokens != 5 || parsed.TotalTokens != 105 || parsed.CachedInputTokens != tc.read || parsed.CacheCreationInputTokens != tc.write {
				t.Fatalf("parsed=%+v ok=%v", parsed, ok)
			}
			for _, terminal := range []string{"completed", "incomplete", "failed"} {
				t.Run(terminal, func(t *testing.T) {
					acc := newResponsesStreamAccumulator("model")
					line := []byte(fmt.Sprintf(`data: {"type":"response.%s","response":{"usage":%s}}`, terminal, raw))
					// Terminal snapshots are absolute counters, not deltas.
					acc.TrackSSELine(line)
					acc.TrackSSELine(line)
					got := acc.FinalizeUsage(nil)
					if !got.Known || got.Estimated || got.PromptTokens != parsed.PromptTokens || got.CompletionTokens != parsed.CompletionTokens || got.CachedInputTokens != parsed.CachedInputTokens || got.CacheCreationInputTokens != parsed.CacheCreationInputTokens || got.CacheHitRate() != parsed.CacheHitRate() {
						t.Fatalf("stream=%+v buffered=%+v", got, parsed)
					}
				})
			}
		})
	}
}

func TestCodexCacheWriteUsageSettlement(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		stream     bool
	}{
		{"responses buffered", "/v1/responses", false},
		{"responses SSE", "/v1/responses", true},
		{"compact buffered", "/v1/responses/compact", false},
		{"compact SSE", "/v1/responses/compact", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const responseJSON = `{"object":"response","id":"resp_cache","status":"completed","output":[{"type":"compaction","encrypted_content":"test"}],"usage":{"input_tokens":100,"output_tokens":5,"total_tokens":105,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":20}}}`
			store := usage.NewMemoryStore()
			t.Cleanup(func() { _ = store.Close() })
			handler := newCodexResponsesHandler(t, store, codexResponsesExecutorStub{
				complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
					return codexresponses.Result{Body: []byte(responseJSON)}, nil
				},
				stream: func(_ context.Context, _ codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
					if err := started(codexresponses.StreamStart{}); err != nil {
						return err
					}
					return emit([]byte(`data: {"type":"response.completed","response":` + responseJSON + "}\n\n"))
				},
			})
			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.2-codex","input":"hello","stream":%t}`, tc.stream)))
			request.Header.Set("Authorization", "Bearer test-client-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"cache_write_tokens":20`) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			events := usageEvents(t, store)
			if len(events) != 1 || events[0].Outcome != "success" || events[0].Estimated || events[0].InputTokens != 100 || events[0].CachedInputTokens != 40 || events[0].CacheCreationInputTokens != 20 || events[0].CacheHitRate != 0.4 {
				t.Fatalf("events=%+v", events)
			}
			dashboard, err := store.Dashboard(context.Background(), usage.UsageFilter{AllTime: true})
			if err != nil {
				t.Fatal(err)
			}
			if dashboard.Summary.CachedInputTokens != 40 || dashboard.Summary.CacheCreationInputTokens != 20 || dashboard.Summary.CacheHitRate != 0.4 {
				t.Fatalf("summary=%+v", dashboard.Summary)
			}
		})
	}
}
