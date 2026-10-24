package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"aetherrelay/internal/modules/application/proxyapi/pkg/usagetarget"
	archive "aetherrelay/internal/pkg/aetherrelayarchive"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelaymetrics"
	"aetherrelay/internal/pkg/aetherrelayusage"
	"aetherrelay/internal/pkg/chatgptattempt"
)

type chatGPTTextExecutorStub struct {
	complete func(context.Context, chatgpttext.Request) (chatgpttext.Result, error)
	stream   func(context.Context, chatgpttext.Request, func(chatgpttext.Delta) error) (chatgpttext.Result, error)
}

func (s chatGPTTextExecutorStub) Complete(ctx context.Context, req chatgpttext.Request) (chatgpttext.Result, error) {
	if s.complete != nil {
		return s.complete(ctx, req)
	}
	return chatgpttext.Result{ConversationID: "conversation-1", ActualModel: "gpt-5-actual", Text: "hello"}, nil
}

func (s chatGPTTextExecutorStub) Stream(ctx context.Context, req chatgpttext.Request, emit func(chatgpttext.Delta) error) (chatgpttext.Result, error) {
	if s.stream != nil {
		return s.stream(ctx, req, emit)
	}
	if emit != nil {
		_ = emit(chatgpttext.Delta{Text: "hello", ActualModel: "gpt-5-actual"})
	}
	return chatgpttext.Result{ConversationID: "conversation-1", ActualModel: "gpt-5-actual", Text: "hello"}, nil
}

func newChatGPTWebHandler(t *testing.T, store usage.Store, exec chatgpttext.Executor) *Handler {
	t.Helper()
	cfg := mustHandlerConfig(config.Config{ChatGPTWeb: config.ChatGPTWebConfig{}})
	h := NewHandler(cfg, store, nil, metrics.NewRegistry()).WithChatGPTTextExecutor(exec)
	h.ReplaceEffectiveCatalog(effectivecatalog.Build(cfg, 1, 1, []effectivecatalog.PoolModel{{
		ID: "gpt-5",
	}}, "2026-07-26T00:00:00Z"))
	return h
}

func usageEvents(t *testing.T, store usage.Store) []usage.Event {
	t.Helper()
	page, err := store.Events(context.Background(), usage.EventFilter{PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	return page.Events
}

func TestChatGPTWebBuiltinChatDoesNotRequireStaticProvider(t *testing.T) {
	h := newChatGPTWebHandler(t, usage.NewMemoryStore(), chatGPTTextExecutorStub{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"hello"`) {
		t.Fatalf("response=%s", resp.Body.String())
	}
}

func TestChatGPTTextRequestAcceptsTextAndDataURLImageParts(t *testing.T) {
	request, err := chatGPTTextRequest("gpt-5", map[string]any{"messages": []any{map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": "what is this?"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLz4QAAAABJRU5ErkJggg=="}},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 1 || request.Messages[0].Content != "what is this?" || len(request.Messages[0].Images) != 1 || len(request.Messages[0].Images[0]) == 0 {
		t.Fatalf("request=%+v", request)
	}
	for _, body := range []map[string]any{
		{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/a.png"}}}}}},
		{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_audio"}}}}},
	} {
		if _, err := chatGPTTextRequest("gpt-5", body); err == nil {
			t.Fatalf("unsupported multimodal payload unexpectedly accepted: %#v", body)
		}
	}
}

func TestChatGPTWebPassesImagePartsToExecutor(t *testing.T) {
	var received chatgpttext.Request
	h := newChatGPTWebHandler(t, usage.NewMemoryStore(), chatGPTTextExecutorStub{
		complete: func(_ context.Context, request chatgpttext.Request) (chatgpttext.Result, error) {
			received = request
			return chatgpttext.Result{Text: "ok"}, nil
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLz4QAAAABJRU5ErkJggg=="}}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || len(received.Messages) != 1 || received.Messages[0].Content != "inspect" || len(received.Messages[0].Images) != 1 {
		t.Fatalf("status=%d request=%+v body=%s", resp.Code, received, resp.Body.String())
	}
}

func TestChatGPTWebCompatibilityRejectsToolsAndRecordsIgnoredControls(t *testing.T) {
	ignored, apiErr := chatGPTWebChatCompatibility(map[string]any{
		"model": "gpt-5", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"temperature": 0.2, "top_p": 0.9, "user": "client-1",
	})
	if apiErr != nil || strings.Join(ignored, ",") != "temperature,top_p,user" {
		t.Fatalf("ignored=%v apiErr=%+v", ignored, apiErr)
	}
	_, apiErr = chatGPTWebChatCompatibility(map[string]any{
		"model": "gpt-5", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{"type": "function"}},
	})
	if apiErr == nil || apiErr.Code != ErrorCodeConversionUnsupported || apiErr.Feature != "tools" {
		t.Fatalf("tools apiErr=%+v", apiErr)
	}
}

func TestChatGPTWebResponsesProjectsBoundedInputAndSSE(t *testing.T) {
	store := usage.NewMemoryStore()
	var received chatgpttext.Request
	h := newChatGPTWebHandler(t, store, chatGPTTextExecutorStub{
		complete: func(_ context.Context, request chatgpttext.Request) (chatgpttext.Result, error) {
			received = request
			return chatgpttext.Result{ConversationID: "conv-1", ActualModel: "gpt-5-actual", Text: "done"}, nil
		},
		stream: func(_ context.Context, request chatgpttext.Request, emit func(chatgpttext.Delta) error) (chatgpttext.Result, error) {
			received = request
			if err := emit(chatgpttext.Delta{Text: "hel", ActualModel: "gpt-5-actual"}); err != nil {
				return chatgpttext.Result{}, err
			}
			if err := emit(chatgpttext.Delta{Text: "lo"}); err != nil {
				return chatgpttext.Result{}, err
			}
			return chatgpttext.Result{ActualModel: "gpt-5-actual", Text: "hello"}, nil
		},
	})
	body := `{"model":"gpt-5","instructions":"be concise","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect"},{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLz4QAAAABJRU5ErkJggg=="}]}],"temperature":0.5}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"object":"response"`) || !strings.Contains(resp.Body.String(), `"text":"done"`) {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if len(received.Messages) != 2 || received.Messages[0].Role != "system" || received.Messages[1].Content != "inspect" || len(received.Messages[1].Images) != 1 {
		t.Fatalf("request=%+v", received)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_file","filename":"notes.md","file_data":"data:text/markdown;base64,IyBoZWxsbw=="}]}]}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp = httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || len(received.Messages) != 1 || len(received.Messages[0].Files) != 1 || received.Messages[0].Files[0].Name != "notes.md" || string(received.Messages[0].Files[0].Bytes) != "# hello" {
		t.Fatalf("status=%d request=%+v body=%s", resp.Code, received, resp.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","stream":true,"input":"say hello"}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp = httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	for _, eventType := range []string{"response.created", "response.output_text.delta", "response.output_text.done", "response.completed"} {
		if !strings.Contains(resp.Body.String(), eventType) {
			t.Fatalf("missing %s in %s", eventType, resp.Body.String())
		}
	}
	if events := usageEvents(t, store); len(events) != 3 {
		t.Fatalf("usage=%+v", events)
	} else {
		foundStream := false
		for _, event := range events {
			if event.Stream && event.Outcome == "success" && event.UpstreamEndpoint == "chatgptweb_responses" && event.ConversionMode == TransportModeChatGPTWebResponses {
				foundStream = true
			}
		}
		if !foundStream {
			t.Fatalf("responses stream usage=%+v", events)
		}
	}
}

func TestChatGPTWebResponsesRejectsStatefulAndToolSemantics(t *testing.T) {
	for _, feature := range []string{"tools", "previous_response_id", "background"} {
		body := map[string]any{"model": "gpt-5", "input": "hello", feature: true}
		if feature == "tools" {
			body[feature] = []any{map[string]any{"type": "function"}}
		}
		if _, _, apiErr := chatGPTResponsesRequest("gpt-5", body); apiErr == nil || apiErr.Code != ErrorCodeConversionUnsupported || apiErr.Feature != feature {
			t.Fatalf("feature=%s apiErr=%+v", feature, apiErr)
		}
	}
}

func TestChatGPTWebResponsesRejectsUndeclaredReasoningBeforeUpstream(t *testing.T) {
	h := newChatGPTWebHandler(t, usage.NewMemoryStore(), chatGPTTextExecutorStub{})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hello","reasoning":{"effort":"low"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), `"feature":"reasoning"`) {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestChatGPTWebTextSuccessSettlesUsage(t *testing.T) {
	store := usage.NewMemoryStore()
	h := newChatGPTWebHandler(t, store, chatGPTTextExecutorStub{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hello world"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	events := usageEvents(t, store)
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	ev := events[0]
	if ev.State != usage.StateCompleted || ev.Outcome != "success" || ev.Provider != "chatgptweb" {
		t.Fatalf("event=%+v", ev)
	}
	if ev.Model != "gpt-5-actual" {
		t.Fatalf("expected actual model, got %q", ev.Model)
	}
	if !ev.Estimated || ev.InputTokens <= 0 || ev.OutputTokens <= 0 {
		t.Fatalf("expected estimated positive tokens: %+v", ev)
	}
	if ev.ErrorCode != "" || ev.HTTPStatus != http.StatusOK {
		t.Fatalf("event=%+v", ev)
	}
	if ev.UpstreamProtocol != "chatgptweb" {
		t.Fatalf("upstream_protocol=%q", ev.UpstreamProtocol)
	}
}

func TestChatGPTWebTextUpstreamFailureSettlesUsage(t *testing.T) {
	store := usage.NewMemoryStore()
	exec := chatGPTTextExecutorStub{
		complete: func(context.Context, chatgpttext.Request) (chatgpttext.Result, error) {
			return chatgpttext.Result{ActualModel: "gpt-5-actual", Text: "partial"}, chatgptfail.New(chatgptfail.KindRateLimit, errors.New("rate limited"))
		},
	}
	h := newChatGPTWebHandler(t, store, exec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	events := usageEvents(t, store)
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	ev := events[0]
	if ev.Outcome != "upstream_failed" || ev.ErrorCode != "rate_limit" {
		t.Fatalf("event=%+v", ev)
	}
	if ev.Provider != "chatgptweb" || ev.Model != "gpt-5-actual" {
		t.Fatalf("event=%+v", ev)
	}
	if ev.ErrorCode == "proxy_internal_error" {
		t.Fatal("must not use proxy_internal_error for classified upstream failure")
	}
	statsJSON, err := h.metricsRegistry.StatsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var stats metrics.StatsJSON
	if err := json.Unmarshal(statsJSON, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Errors.ByStatusCode["-1"] != 1 {
		t.Fatalf("ChatGPT Web non-stream upstream failure must be counted: %+v", stats.Errors)
	}
}

func TestChatGPTWebTextStreamSuccessSettlesUsage(t *testing.T) {
	store := usage.NewMemoryStore()
	h := newChatGPTWebHandler(t, store, chatGPTTextExecutorStub{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "data:") || !strings.Contains(resp.Body.String(), "[DONE]") {
		t.Fatalf("body=%s", resp.Body.String())
	}
	events := usageEvents(t, store)
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	ev := events[0]
	if !ev.Stream || ev.Outcome != "success" || !ev.Estimated || ev.Provider != "chatgptweb" {
		t.Fatalf("event=%+v", ev)
	}
}

func TestChatGPTWebTextClientCancelSettlesUsage(t *testing.T) {
	store := usage.NewMemoryStore()
	exec := chatGPTTextExecutorStub{
		stream: func(ctx context.Context, _ chatgpttext.Request, emit func(chatgpttext.Delta) error) (chatgpttext.Result, error) {
			if emit != nil {
				_ = emit(chatgpttext.Delta{Text: "hel"})
			}
			return chatgpttext.Result{Text: "hel", ActualModel: "gpt-5-actual"}, chatgptfail.New(chatgptfail.KindClientCanceled, context.Canceled)
		},
	}
	h := newChatGPTWebHandler(t, store, exec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	// Stream already started: HTTP stays 200, outcome is client_canceled.
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	events := usageEvents(t, store)
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	ev := events[0]
	if ev.Outcome != "client_canceled" || ev.ErrorCode != "client_canceled" {
		t.Fatalf("event=%+v", ev)
	}
	if ev.Provider != "chatgptweb" || !ev.Estimated {
		t.Fatalf("event=%+v", ev)
	}
}

func TestUpdateConfigKeepsBuiltinCatalogUntilDiscoveryRefresh(t *testing.T) {
	cfg := mustHandlerConfig(config.Config{ChatGPTWeb: config.ChatGPTWebConfig{}})
	h := NewHandler(cfg, usage.NewMemoryStore(), nil, nil)
	h.ReplaceEffectiveCatalog(effectivecatalog.Build(cfg, 7, 1, []effectivecatalog.PoolModel{{
		ID: "gpt-5",
	}}, "2026-07-26T00:00:00Z"))

	updated := cfg
	updated.RequestTimeout++
	if err := h.UpdateConfig(updated); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	route, ok := h.EffectiveCatalog().Lookup("gpt-5")
	if !ok || !route.Builtin {
		t.Fatalf("builtin route lost after config update: %+v ok=%v", route, ok)
	}
}

func TestChatGPTWebTextClientWriteSettlesUsage(t *testing.T) {
	store := usage.NewMemoryStore()
	exec := chatGPTTextExecutorStub{
		stream: func(ctx context.Context, _ chatgpttext.Request, emit func(chatgpttext.Delta) error) (chatgpttext.Result, error) {
			if emit != nil {
				_ = emit(chatgpttext.Delta{Text: "hel", ActualModel: "gpt-5-actual"})
			}
			return chatgpttext.Result{Text: "hel", ActualModel: "gpt-5-actual"}, chatgptfail.New(chatgptfail.KindClientWrite, errors.New("broken pipe"))
		},
	}
	h := newChatGPTWebHandler(t, store, exec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	events := usageEvents(t, store)
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	ev := events[0]
	if ev.Outcome != "client_write" || ev.ErrorCode != "client_write" {
		t.Fatalf("event=%+v", ev)
	}
	if ev.Provider != "chatgptweb" || !ev.Estimated {
		t.Fatalf("event=%+v", ev)
	}
}

// CP-OBS-016: pending status refresh and local rejection diagnostics.
func TestChatGPTWebStructuredOutputRejectionPersistsDiagnostics(t *testing.T) {
	for _, durable := range []bool{false, true} {
		name := "memory without archive"
		if durable {
			name = "duckdb with archive"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			var store usage.Store = usage.NewMemoryStore()
			storeConfig := config.UsageStoreConfig{Path: filepath.Join(root, "usage.duckdb"), MemoryLimit: "256MB", Threads: 1}
			if durable {
				var err error
				store, err = usage.OpenDuckDB(storeConfig)
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _ = store.Close() })
			cfg := mustHandlerConfig(config.Config{InteractionDir: filepath.Join(root, "interactions"), ArchiveInteractions: durable, ArchiveFullContent: durable})
			var recorder *archive.Recorder
			if durable {
				var err error
				recorder, err = archive.NewRecorderOptions(cfg.InteractionDir, archive.RecorderOptions{MaxRounds: 10, ScopeByAPIKey: true})
				if err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			h := NewHandler(cfg, store, recorder, nil).WithChatGPTTextExecutor(chatGPTTextExecutorStub{complete: func(context.Context, chatgpttext.Request) (chatgpttext.Result, error) {
				calls++
				return chatgpttext.Result{}, nil
			}})
			if durable {
				withClientKey(h, "test-client", "test-client-key")
			}
			h.ReplaceEffectiveCatalog(effectivecatalog.Build(cfg, 1, 1, []effectivecatalog.PoolModel{{ID: "auto"}}, time.Now().UTC().Format(time.RFC3339)))
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"return JSON"}],"response_format":{"type":"json_object"},"temperature":0.2}`))
			req.Header.Set("Authorization", "Bearer test-client-key")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			var response APIErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if rec.Code != 400 || calls != 0 || response.Error.Feature != "response_format" || response.Error.FailureClass != ErrorCodeConversionUnsupported || strings.Join(response.Error.UnsupportedFeatures, ",") != "response_format" {
				t.Fatalf("status=%d calls=%d response=%s", rec.Code, calls, rec.Body.String())
			}
			if durable {
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				var err error
				store, err = usage.OpenDuckDB(storeConfig)
				if err != nil {
					t.Fatal(err)
				}
			}
			events := usageEvents(t, store)
			if len(events) != 1 {
				t.Fatalf("events=%+v", events)
			}
			e := events[0]
			if e.State != usage.StateCompleted || e.HTTPStatus != 400 || e.ErrorCode != ErrorCodeConversionUnsupported || e.FailureClass != response.Error.FailureClass || strings.Join(e.UnsupportedFeatures, ",") != "response_format" || e.UpstreamStatus != 0 || e.AccountID != "" || e.TotalTokens != 0 {
				t.Fatalf("incomplete rejection diagnostic: %+v", e)
			}
			if durable {
				paths, err := filepath.Glob(filepath.Join(cfg.InteractionDir, "*", "*", "metadata.json"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("metadata=%v err=%v", paths, err)
				}
				raw, err := os.ReadFile(paths[0])
				if err != nil {
					t.Fatal(err)
				}
				var meta struct {
					FailureClass        string   `json:"failure_class"`
					UnsupportedFeatures []string `json:"unsupported_features"`
				}
				if err = json.Unmarshal(raw, &meta); err != nil {
					t.Fatal(err)
				}
				if meta.FailureClass != e.FailureClass || strings.Join(meta.UnsupportedFeatures, ",") != "response_format" {
					t.Fatalf("archive diagnostics=%s", raw)
				}
			}
		})
	}
}

// CP-OBS-017: one usage settlement, final account/status, and separate archived attempts.
func TestChatGPTWebAttemptDiagnosticsMatchUsageAndArchive(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("recovered=%t/stream=%t", recovered, streaming), func(t *testing.T) {
				store := usage.NewMemoryStore()
				defer store.Close()
				cfg := mustHandlerConfig(config.Config{InteractionDir: t.TempDir(), ArchiveInteractions: true, ArchiveFullContent: true})
				recorder, err := archive.NewRecorderOptions(cfg.InteractionDir, archive.RecorderOptions{MaxRounds: 10, ScopeByAPIKey: true})
				if err != nil {
					t.Fatal(err)
				}
				registry := metrics.NewRegistry()
				calls := 0
				execute := func(ctx context.Context) (chatgpttext.Result, error) {
					calls++
					usagetarget.Record(ctx, "chatgptweb", "first", "first@example.com")
					usagetarget.RecordTextAttempt(ctx, chatgptattempt.Observation{Index: 1, AccountID: "first", AccountLabel: "first@example.com", Operation: "bootstrap", UpstreamStatus: 403, GenerationNotSent: true, FailureClass: "upstream"})
					if !recovered {
						return chatgpttext.Result{}, &chatgptfail.Failure{Kind: chatgptfail.KindUpstream, Operation: "bootstrap", UpstreamStatus: 403, GenerationNotSent: true, Cause: errors.New("denied")}
					}
					usagetarget.Record(ctx, "chatgptweb", "second", "second@example.com")
					usagetarget.RecordTextAttempt(ctx, chatgptattempt.Observation{Index: 2, AccountID: "second", AccountLabel: "second@example.com", UpstreamStatus: 200, Success: true})
					return chatgpttext.Result{Text: "hello", ActualModel: "auto"}, nil
				}
				h := NewHandler(cfg, store, recorder, registry).WithChatGPTTextExecutor(chatGPTTextExecutorStub{
					complete: func(ctx context.Context, request chatgpttext.Request) (chatgpttext.Result, error) {
						return execute(ctx)
					},
					stream: func(ctx context.Context, request chatgpttext.Request, emit func(chatgpttext.Delta) error) (chatgpttext.Result, error) {
						result, err := execute(ctx)
						if err == nil {
							err = emit(chatgpttext.Delta{Text: result.Text, ActualModel: result.ActualModel})
						}
						return result, err
					},
				})
				h.ReplaceEffectiveCatalog(effectivecatalog.Build(cfg, 1, 1, []effectivecatalog.PoolModel{{ID: "auto"}}, time.Now().UTC().Format(time.RFC3339)))
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"auto","messages":[{"role":"user","content":"hi"}],"stream":%t}`, streaming)))
				req.Header.Set("Authorization", "Bearer test-client-key")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				status, upstreamStatus, account, attempts := 502, 403, "first", 1
				if recovered {
					status, upstreamStatus, account, attempts = 200, 200, "second", 2
				}
				events := usageEvents(t, store)
				if calls != 1 || rec.Code != status || len(events) != 1 {
					t.Fatalf("calls=%d status=%d body=%s events=%+v", calls, rec.Code, rec.Body.String(), events)
				}
				e := events[0]
				if e.State != usage.StateCompleted || e.AccountID != account || e.AccountLabel != account+"@example.com" || e.UpstreamStatus != upstreamStatus {
					t.Fatalf("usage=%+v", e)
				}
				if recovered && (e.Outcome != "success" || e.FailureClass != "") {
					t.Fatalf("old failure leaked into success: %+v", e)
				}
				paths, err := filepath.Glob(filepath.Join(cfg.InteractionDir, "*", "*", "metadata.json"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("paths=%v err=%v", paths, err)
				}
				raw, err := os.ReadFile(paths[0])
				if err != nil {
					t.Fatal(err)
				}
				var meta archive.Metadata
				if err = json.Unmarshal(raw, &meta); err != nil {
					t.Fatal(err)
				}
				if meta.AccountID != e.AccountID || meta.UpstreamStatus != e.UpstreamStatus || len(meta.TextAttempts) != attempts {
					t.Fatalf("archive mismatch: %s", raw)
				}
				if meta.TextAttempts[0].UpstreamStatus != 403 || meta.TextAttempts[0].AccountID != "first" {
					t.Fatalf("first attempt lost: %s", raw)
				}
				stats := registry.Snapshot()
				if stats.Cache.ByProvider["chatgptweb"].Miss != 0 {
					t.Fatalf("estimated cache counted as miss: %+v", stats.Cache)
				}
			})
		}
	}
}
