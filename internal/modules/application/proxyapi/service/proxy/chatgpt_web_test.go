package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgpttext"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxyusage"
)

type chatGPTTextExecutorStub struct{}

func (chatGPTTextExecutorStub) Complete(context.Context, chatgpttext.Request) (chatgpttext.Result, error) {
	return chatgpttext.Result{ConversationID: "conversation-1", Text: "hello"}, nil
}

func (chatGPTTextExecutorStub) Stream(context.Context, chatgpttext.Request, func(chatgpttext.Delta) error) (chatgpttext.Result, error) {
	return chatgpttext.Result{}, nil
}

func TestChatGPTWebBuiltinChatDoesNotRequireStaticProvider(t *testing.T) {
	cfg := mustHandlerConfig(config.Config{ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true}})
	h := NewHandler(cfg, usage.NewMemoryStore(), nil, nil).WithChatGPTTextExecutor(chatGPTTextExecutorStub{})
	h.ReplaceEffectiveCatalog(effectivecatalog.Build(cfg, 1, 1, []effectivecatalog.PoolModel{{
		ID: "gpt-5", Operations: []string{config.ModelOperationChatCompletions},
	}}, "2026-07-26T00:00:00Z"))

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
