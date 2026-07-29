package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptfail"
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgpttext"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetrics"
	"ai-proxy/internal/pkg/aiproxyusage"
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
	cfg := mustHandlerConfig(config.Config{ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true}})
	h := NewHandler(cfg, store, nil, metrics.NewRegistry()).WithChatGPTTextExecutor(exec)
	h.ReplaceEffectiveCatalog(effectivecatalog.Build(cfg, 1, 1, []effectivecatalog.PoolModel{{
		ID: "gpt-5", Operations: []string{config.ModelOperationChatCompletions},
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
	cfg := mustHandlerConfig(config.Config{ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true}})
	h := NewHandler(cfg, usage.NewMemoryStore(), nil, nil)
	h.ReplaceEffectiveCatalog(effectivecatalog.Build(cfg, 7, 1, []effectivecatalog.PoolModel{{
		ID: "gpt-5", Operations: []string{config.ModelOperationChatCompletions},
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
