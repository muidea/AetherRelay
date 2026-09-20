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
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

func TestCodexConversionObservationWithoutArchive(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, details string
			read, write   int64
			known         bool
		}{
			{"missing", "", 0, 0, false},
			{"zero", `,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}`, 0, 0, true},
			{"hit", `,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":20}`, 40, 20, true},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				store := usage.NewMemoryStore()
				t.Cleanup(func() { _ = store.Close() })
				attempt := codexArchiveTestAttempt()
				attempt.Response.TransferEncoding = "chunked"
				result := `{"id":"resp_done","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":100,"output_tokens":5` + tc.details + `}}`
				keys := []string{}
				observe := func(req codexresponses.Request) {
					var body map[string]any
					if err := json.Unmarshal(req.Body, &body); err != nil {
						t.Fatal(err)
					}
					keys = append(keys, body["prompt_cache_key"].(string))
					if req.ObserveAttempt == nil {
						t.Fatal("statistics depend on archive")
					}
					failed := attempt
					failed.Request.At = attempt.Request.At.Add(-time.Second)
					failed.Response.Status = 429
					failed.Response.DurationMS = 500
					req.ObserveAttempt(failed, fmt.Errorf("retry"))
					req.ObserveAttempt(attempt, nil)
				}
				h := newCodexResponsesHandler(t, store, codexResponsesExecutorStub{
					complete: func(_ context.Context, req codexresponses.Request) (codexresponses.Result, error) {
						observe(req)
						return codexresponses.Result{Body: []byte(result), Attempt: attempt}, nil
					},
					stream: func(_ context.Context, req codexresponses.Request, start func(codexresponses.StreamStart) error, emit func([]byte) error) error {
						observe(req)
						if err := start(codexresponses.StreamStart{Attempt: attempt, FirstEventDuration: time.Second}); err != nil {
							return err
						}
						for _, event := range []string{
							`{"type":"response.created","response":{"id":"resp_done","model":"gpt-5.2-codex"}}`,
							`{"type":"response.output_text.delta","delta":"done"}`,
							`{"type":"response.output_text.done"}`,
						} {
							if err := emit([]byte("data: " + event + "\n\n")); err != nil {
								return err
							}
						}
						return emit([]byte(`data: {"type":"response.completed","response":` + result + "}\n\n"))
					},
				})
				for i := 0; i < 2; i++ {
					raw := strings.TrimSuffix(claudeToolContinuation, "}") + fmt.Sprintf(`,"stream":%t}`, stream)
					r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(raw))
					r.Header.Set("Authorization", "Bearer test-client-key")
					r.Header.Set("X-Claude-Code-Session-Id", "session-example")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if w.Code != 200 || !strings.Contains(w.Body.String(), `"stop_reason":"end_turn"`) {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
					if tc.known != strings.Contains(w.Body.String(), `"cache_read_input_tokens"`) {
						t.Fatalf("cache presence lost: %s", w.Body.String())
					}
					if tc.known && !strings.Contains(w.Body.String(), fmt.Sprintf(`"input_tokens":%d`, 100-tc.read-tc.write)) {
						t.Fatalf("cache double counted: %s", w.Body.String())
					}
				}
				if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
					t.Fatal("Claude cache identity changed across rounds")
				}
				for _, e := range usageEvents(t, store) {
					if e.Outcome != "success" || e.ConversionLevel != 2 || !e.ConversionDegraded || e.InputTokens != 100 || e.CachedInputTokens != tc.read || e.CacheCreationInputTokens != tc.write || e.CachedInputTokensKnown != tc.known || e.CacheCreationInputTokensKnown != tc.known {
						t.Fatalf("event=%+v", e)
					}
					if e.UpstreamStatus != 200 || e.UpstreamDurationMS != 42 || e.UpstreamContentType != "text/event-stream" || e.UpstreamContentLengthKnown || e.UpstreamTransferEncoding != "chunked" {
						t.Fatalf("observations=%+v", e)
					}
				}
			})
		}
	}
}

func TestFinalUnobservedAttemptClearsEarlierResponse(t *testing.T) {
	h := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{})
	round := archive.NewObservationRound()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	attempt := codexArchiveTestAttempt()
	h.archiveCodexUpstreamAttempt(round, r, "codexoauth", attempt, nil)
	attempt.Request.At = attempt.Request.At.Add(time.Second)
	attempt.Response = codexresponses.HTTPResponseObservation{}
	h.archiveCodexUpstreamAttempt(round, r, "codexoauth", attempt, fmt.Errorf("transport"))
	if round.UpstreamStatus != 0 || round.UpstreamContentLength != -1 || round.UpstreamDuration != 0 {
		t.Fatalf("stale observation: %+v", round)
	}
}

func TestClaudeCacheIdentityIsolation(t *testing.T) {
	key := func(client, model, session string) string {
		r := codexIdentityRequest(client, model, map[string]string{"X-Claude-Code-Session-Id": session})
		r.URL.Path = "/v1/messages"
		return codexPromptCacheHash(r, model, nil)
	}
	baseline := key("key-a", "model-a", "session-a")
	if baseline != key("key-a", "model-a", "session-a") {
		t.Fatal("session is not stable")
	}
	for _, other := range []string{key("key-b", "model-a", "session-a"), key("key-a", "model-b", "session-a"), key("key-a", "model-a", "session-b")} {
		if baseline == other {
			t.Fatal("cache namespace collision")
		}
	}
	for _, invalid := range []string{"", "bad session", strings.Repeat("x", 257), "中文"} {
		if key("key-a", "model-a", invalid) == key("key-a", "model-a", invalid) {
			t.Fatal("missing/invalid session merged requests")
		}
	}
}

func TestConvertedArchiveMatchesUsageEvent(t *testing.T) {
	attempt := codexArchiveTestAttempt()
	h, dir := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
		return codexresponses.Result{Attempt: attempt, Body: []byte(`{"id":"resp","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":0}}}`)}, nil
	}})
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(claudeToolContinuation))
	r.Header.Set("Authorization", "Bearer test-client-key")
	r.Header.Set("X-Request-Id", "client-correlation")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "test-client", "000001", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta archive.Metadata
	if err = json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	events := usageEvents(t, h.usageStore)
	if len(events) != 1 {
		t.Fatalf("events=%+v", events)
	}
	e := events[0]
	if meta.EventID != e.EventID || meta.EventID == meta.RequestID || meta.UpstreamStatus != e.UpstreamStatus || meta.UpstreamDurationMS != e.UpstreamDurationMS || meta.ConversionLevel != 2 || !meta.ConversionDegraded || !meta.CachedInputTokensKnown || meta.CacheCreationInputTokensKnown {
		t.Fatalf("metadata=%+v event=%+v", meta, e)
	}
}

func TestCacheCounterPresenceValidation(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		known bool
		count int
	}{
		{`{}`, false, 0},
		{`{"input_tokens_details":{"cached_tokens":0}}`, true, 0},
		{`{"input_tokens_details":{"cached_tokens":40}}`, true, 40},
		{`{"input_tokens_details":{"cached_tokens":null}}`, false, 0},
		{`{"input_tokens_details":{"cached_tokens":"40"}}`, false, 0},
		{`{"input_tokens_details":{"cached_tokens":-1}}`, false, 0},
		{`{"input_tokens_details":{"cached_tokens":1.5}}`, false, 0},
	} {
		u, ok := usageFromRaw([]byte(tc.raw))
		if !ok || u.CachedInputTokensKnown != tc.known || u.CachedInputTokens != tc.count {
			t.Fatalf("raw=%s usage=%+v", tc.raw, u)
		}
	}
}
