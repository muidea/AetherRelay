package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-proxy/internal/modules/application/proxyapi/pkg/codexresponses"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	config "ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxyusage"
)

type codexResponsesExecutorStub struct {
	complete func(context.Context, codexresponses.Request) (codexresponses.Result, error)
	stream   func(context.Context, codexresponses.Request, func(codexresponses.StreamStart) error, func([]byte) error) error
}

func (s codexResponsesExecutorStub) CompleteCodexResponses(ctx context.Context, request codexresponses.Request) (codexresponses.Result, error) {
	if s.complete != nil {
		return s.complete(ctx, request)
	}
	return codexresponses.Result{Body: []byte(`{"object":"response","id":"resp_1","usage":{"input_tokens":3,"output_tokens":2}}`)}, nil
}

func (s codexResponsesExecutorStub) StreamCodexResponses(ctx context.Context, request codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
	if s.stream != nil {
		return s.stream(ctx, request, started, emit)
	}
	if started != nil {
		if err := started(codexresponses.StreamStart{}); err != nil {
			return err
		}
	}
	if emit != nil {
		return emit([]byte("data: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"id\":\"resp_1\"}}\n\n"))
	}
	return nil
}

func newCodexResponsesHandler(t *testing.T, store usage.Store, executor codexresponses.Executor) *Handler {
	t.Helper()
	cfg := mustHandlerConfig(config.Config{CodexOAuth: config.CodexOAuthConfig{Enabled: true}})
	handler := NewHandler(cfg, store, nil, nil).WithCodexResponsesExecutor(executor)
	handler.ReplaceEffectiveCatalog(effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: "gpt-5.2-codex"}}}))
	return handler
}

func TestCodexOAuthResponsesPreservesNativeRequestAndSettlesUsage(t *testing.T) {
	store := usage.NewMemoryStore()
	var received codexresponses.Request
	handler := newCodexResponsesHandler(t, store, codexResponsesExecutorStub{complete: func(_ context.Context, request codexresponses.Request) (codexresponses.Result, error) {
		received = request
		return codexresponses.Result{Body: []byte(`{"object":"response","id":"resp_2","usage":{"input_tokens":7,"output_tokens":4}}`)}, nil
	}})
	body := []byte(`{"model":"gpt-5.2-codex","input":"hello","tools":[{"type":"function","name":"lookup"}],"metadata":{"trace":"client"}}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !bytes.Equal(received.Body, body) || received.Model != "gpt-5.2-codex" {
		t.Fatalf("native request was rewritten: %+v body=%s", received, received.Body)
	}
	events := usageEvents(t, store)
	if len(events) != 1 {
		t.Fatalf("usage events=%+v", events)
	}
	event := events[0]
	if event.Outcome != "success" || event.Provider != effectivecatalog.CodexOAuthProviderID || event.UpstreamProtocol != effectivecatalog.CodexOAuthProviderID || event.UpstreamEndpoint != "codex_oauth_responses" || event.ConversionMode != TransportModeCodexOAuthResponses {
		t.Fatalf("Codex usage metadata=%+v", event)
	}
}

func TestCodexOAuthResponsesDoesNotServeChatCompletions(t *testing.T) {
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-5.2-codex","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestResponsesFallsBackFromDirectProviderToCodexOAuth(t *testing.T) {
	cfg := mustHandlerConfig(config.Config{
		CodexOAuth: config.CodexOAuthConfig{Enabled: true},
		Providers: map[string]config.Provider{
			"primary": {
				Name: "primary", Protocol: "openai", BaseURL: "https://primary.test", APIKey: "k", Models: []string{"gpt-shared"}, Priority: 200,
				EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions, config.EndpointCapabilityResponses},
			},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-shared": {ID: "gpt-shared", ContextWindowTokens: 128000, MaxOutputTokens: 16384, Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "primary"},
		},
	})
	store := usage.NewMemoryStore()
	handler := NewHandler(cfg, store, nil, nil).WithCodexResponsesExecutor(codexResponsesExecutorStub{})
	handler.ReplaceEffectiveCatalog(effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: "gpt-shared"}}}))
	attempts := 0
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if r.URL.Host != "primary.test" {
			return nil, fmt.Errorf("unexpected direct provider %q", r.URL.Host)
		}
		return testResponse(http.StatusBadGateway, "application/json", `{"error":"bad gateway"}`), nil
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-shared","input":"hello","tools":[{"type":"function","name":"lookup"}]}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"object":"response"`)) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if attempts != 1 {
		t.Fatalf("direct attempts=%d want 1", attempts)
	}
	events := usageEvents(t, store)
	if len(events) != 1 || events[0].Provider != effectivecatalog.CodexOAuthProviderID || events[0].Outcome != "success" {
		t.Fatalf("usage events=%+v", events)
	}
}

func TestResponsesFallsBackFromCodexOAuthToDirectProvider(t *testing.T) {
	cfg := mustHandlerConfig(config.Config{
		CodexOAuth: config.CodexOAuthConfig{Enabled: true},
		Providers: map[string]config.Provider{
			"backup": {
				Name: "backup", Protocol: "openai", BaseURL: "https://backup.test", APIKey: "k", Models: []string{"gpt-shared"}, Priority: 20,
				EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions, config.EndpointCapabilityResponses},
			},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-shared": {ID: "gpt-shared", ContextWindowTokens: 128000, MaxOutputTokens: 16384, Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "backup"},
		},
	})
	store := usage.NewMemoryStore()
	failure := codexresponses.NewFailure(codexresponses.KindUpstream, 0, fmt.Errorf("Codex upstream failed"))
	failure.HTTPStatus = http.StatusBadGateway
	handler := NewHandler(cfg, store, nil, nil).WithCodexResponsesExecutor(codexResponsesExecutorStub{complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
		return codexresponses.Result{}, failure
	}})
	handler.ReplaceEffectiveCatalog(effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: "gpt-shared"}}}))
	attempts := 0
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		return testResponse(http.StatusOK, "application/json", `{"object":"response","id":"resp_backup","output_text":"ok"}`), nil
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-shared","input":"hello"}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"resp_backup"`)) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if attempts != 1 {
		t.Fatalf("direct attempts=%d want 1", attempts)
	}
	events := usageEvents(t, store)
	if len(events) != 1 || events[0].Provider != "backup" || events[0].Outcome != "success" {
		t.Fatalf("usage events=%+v", events)
	}
}
