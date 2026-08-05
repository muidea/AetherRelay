package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptsearch"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetrics"
	"ai-proxy/internal/pkg/aiproxyusage"
)

type chatGPTSearchExecutorStub struct {
	search func(context.Context, chatgptsearch.Request) (chatgptsearch.Result, error)
}

func (s chatGPTSearchExecutorStub) Search(ctx context.Context, request chatgptsearch.Request) (chatgptsearch.Result, error) {
	if s.search != nil {
		return s.search(ctx, request)
	}
	return chatgptsearch.Result{ConversationID: "search-1", ActualModel: "gpt-5-search", Text: "Answer", Sources: []chatgptsearch.Source{{Title: "Example", URL: "https://example.test"}}}, nil
}

func TestChatGPTWebSearchChatProjectsSourcesAndBufferedStream(t *testing.T) {
	var received chatgptsearch.Request
	h := newChatGPTWebHandler(t, usage.NewMemoryStore(), chatGPTTextExecutorStub{}).WithChatGPTSearchExecutor(chatGPTSearchExecutorStub{search: func(_ context.Context, request chatgptsearch.Request) (chatgptsearch.Result, error) {
		received = request
		return chatgptsearch.Result{ConversationID: "search-1", ActualModel: "gpt-5-search", Text: "Answer", Sources: []chatgptsearch.Source{{Title: "Example", URL: "https://example.test"}}}, nil
	}})
	for _, stream := range []bool{false, true} {
		body := `{"model":"gpt-5","messages":[{"role":"system","content":"concise"},{"role":"user","content":"latest news"}],"tools":[{"type":"web_search"}]}`
		if stream {
			body = strings.TrimSuffix(body, "}") + `,"stream":true}`
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-client-key")
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "https://example.test") || (stream && !strings.Contains(resp.Body.String(), "[DONE]")) {
			t.Fatalf("stream=%v status=%d body=%s", stream, resp.Code, resp.Body.String())
		}
	}
	if received.Model != "gpt-5" || received.Query != "latest news" {
		t.Fatalf("request=%+v", received)
	}
}

func TestChatGPTWebSearchRejectsMixedToolsAndMultimodalQuery(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-5","messages":[{"role":"user","content":"find"}],"tools":[{"type":"web_search"},{"type":"function","function":{"name":"x"}}]}`,
		`{"model":"gpt-5","messages":[{"role":"user","content":[{"type":"text","text":"find"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}],"tools":[{"type":"web_search"}]}`,
	} {
		h := newChatGPTWebHandler(t, usage.NewMemoryStore(), chatGPTTextExecutorStub{}).WithChatGPTSearchExecutor(chatGPTSearchExecutorStub{})
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-client-key")
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), ErrorCodeConversionUnsupported) {
			t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
		}
	}
}

func TestChatGPTWebSearchResponsesEmitsSearchLifecycle(t *testing.T) {
	h := newChatGPTWebHandler(t, usage.NewMemoryStore(), chatGPTTextExecutorStub{}).WithChatGPTSearchExecutor(chatGPTSearchExecutorStub{})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","stream":true,"input":"latest news","tools":[{"type":"web_search_preview"}]}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	for _, eventName := range []string{"response.created", "response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed", "response.completed"} {
		if !strings.Contains(resp.Body.String(), eventName) {
			t.Fatalf("missing %s: %s", eventName, resp.Body.String())
		}
	}
}

func TestChatGPTWebSearchEndpointForcesBuiltinProviderDespiteModelConflict(t *testing.T) {
	var received chatgptsearch.Request
	cfg := mustHandlerConfig(config.Config{
		ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true},
		Providers: map[string]config.Provider{
			"preferred-static": {
				Name: "preferred-static", Protocol: "openai", Models: []string{"gpt-5"}, Priority: 1000,
				Endpoints: []string{config.ProviderEndpointChatCompletions},
			},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"gpt-5": {ID: "gpt-5", ContextWindowTokens: 128000, MaxOutputTokens: 16384},
		},
	})
	h := NewHandler(cfg, usage.NewMemoryStore(), nil, metrics.NewRegistry()).WithChatGPTSearchExecutor(chatGPTSearchExecutorStub{search: func(_ context.Context, request chatgptsearch.Request) (chatgptsearch.Result, error) {
		received = request
		return chatgptsearch.Result{ConversationID: "search-1", ActualModel: "gpt-5-search", Text: "Answer", Sources: []chatgptsearch.Source{{Title: "Example", URL: "https://example.test", Snippet: "source excerpt"}}}, nil
	}})
	h.ReplaceEffectiveCatalog(effectivecatalog.Build(cfg, 1, 1, []effectivecatalog.PoolModel{{ID: "gpt-5"}}, "2026-08-03T00:00:00Z"))

	req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"model":"gpt-5","query":"latest news"}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"object":"search.result"`) || !strings.Contains(resp.Body.String(), `"snippet":"source excerpt"`) {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if received.Model != "gpt-5" || received.Query != "latest news" {
		t.Fatalf("search request=%+v", received)
	}
	events := usageEvents(t, h.usageStore)
	if len(events) != 1 || events[0].Provider != effectivecatalog.BuiltinProviderID || events[0].ClientEndpoint != "/v1/search" || events[0].UpstreamEndpoint != "chatgptweb_search" {
		t.Fatalf("usage events=%+v", events)
	}
}

func TestChatGPTWebSearchEndpointRejectsUnsupportedFields(t *testing.T) {
	h := newChatGPTWebHandler(t, usage.NewMemoryStore(), chatGPTTextExecutorStub{}).WithChatGPTSearchExecutor(chatGPTSearchExecutorStub{})
	req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"model":"gpt-5","query":"latest news","stream":true}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), ErrorCodeConversionUnsupported) || !strings.Contains(resp.Body.String(), `"feature":"stream"`) {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestChatGPTWebSearchEndpointReportsUnavailableAccountPool(t *testing.T) {
	cfg := mustHandlerConfig(config.Config{ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true}})
	h := NewHandler(cfg, usage.NewMemoryStore(), nil, metrics.NewRegistry()).WithChatGPTSearchExecutor(chatGPTSearchExecutorStub{})
	h.ReplaceEffectiveCatalog(effectivecatalog.Build(cfg, 1, 0, []effectivecatalog.PoolModel{{ID: "gpt-5"}}, "2026-08-03T00:00:00Z"))
	req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"model":"gpt-5","query":"latest news"}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusServiceUnavailable || !strings.Contains(resp.Body.String(), ErrorCodeProviderUnavailable) || !strings.Contains(resp.Body.String(), "no available chatgpt web accounts") {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}
