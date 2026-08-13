package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	aetherrelaymetrics "aetherrelay/internal/pkg/aetherrelaymetrics"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

func TestCodexNormalizationGoldenCorpus(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex_normalization_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus []struct {
		Name          string         `json:"name"`
		Compact       bool           `json:"compact"`
		Input         map[string]any `json:"input"`
		Expected      map[string]any `json:"expected"`
		ErrorContains string         `json:"error_contains"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range corpus {
		t.Run(testCase.Name, func(t *testing.T) {
			input, _ := json.Marshal(testCase.Input)
			normalized, _, _, normalizeErr := normalizeCodexRequest(input, testCase.Compact)
			if testCase.ErrorContains != "" {
				if normalizeErr == nil || !strings.Contains(normalizeErr.Error(), testCase.ErrorContains) {
					t.Fatalf("CP-DOD-001 err=%v", normalizeErr)
				}
				return
			}
			if normalizeErr != nil {
				t.Fatal(normalizeErr)
			}
			var got map[string]any
			if err := json.Unmarshal(normalized, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, testCase.Expected) {
				t.Fatalf("CP-DOD-001 got=%s want=%#v", normalized, testCase.Expected)
			}
		})
	}
}

type codexResponsesExecutorStub struct {
	complete func(context.Context, codexresponses.Request) (codexresponses.Result, error)
	stream   func(context.Context, codexresponses.Request, func(codexresponses.StreamStart) error, func([]byte) error) error
	wsOpen   func(context.Context, codexresponses.WebsocketOpenRequest) (codexresponses.WebsocketOpenResult, error)
	wsSend   func(context.Context, string, []byte) error
	wsPull   func(context.Context, string) ([]byte, bool, error)
	wsClose  func(context.Context, string)
}

func (s codexResponsesExecutorStub) CompleteCodexResponses(ctx context.Context, request codexresponses.Request) (codexresponses.Result, error) {
	if s.complete != nil {
		return s.complete(ctx, request)
	}
	return codexresponses.Result{Body: []byte(`{"object":"response","id":"resp_1","usage":{"input_tokens":3,"output_tokens":2}}`)}, nil
}

func (s codexResponsesExecutorStub) CompleteCodexCompact(ctx context.Context, request codexresponses.Request) (codexresponses.Result, error) {
	return s.CompleteCodexResponses(ctx, request)
}

func (s codexResponsesExecutorStub) StartCodexCompact(ctx context.Context, request codexresponses.Request) (<-chan codexresponses.Completion, error) {
	done := make(chan codexresponses.Completion, 1)
	go func() {
		result, err := s.CompleteCodexCompact(ctx, request)
		done <- codexresponses.Completion{Result: result, Err: err}
		close(done)
	}()
	return done, nil
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

func (s codexResponsesExecutorStub) OpenCodexWebsocket(ctx context.Context, request codexresponses.WebsocketOpenRequest) (codexresponses.WebsocketOpenResult, error) {
	if s.wsOpen != nil {
		return s.wsOpen(ctx, request)
	}
	return codexresponses.WebsocketOpenResult{SessionID: "ws-test"}, nil
}

func (s codexResponsesExecutorStub) SendCodexWebsocket(ctx context.Context, sessionID string, payload []byte) error {
	if s.wsSend != nil {
		return s.wsSend(ctx, sessionID, payload)
	}
	return nil
}
func (s codexResponsesExecutorStub) PullCodexWebsocket(ctx context.Context, sessionID string) ([]byte, bool, error) {
	if s.wsPull != nil {
		return s.wsPull(ctx, sessionID)
	}
	return []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`), false, nil
}
func (s codexResponsesExecutorStub) CloseCodexWebsocket(ctx context.Context, sessionID string) {
	if s.wsClose != nil {
		s.wsClose(ctx, sessionID)
	}
}

func newCodexResponsesHandler(t *testing.T, store usage.Store, executor codexresponses.Executor) *Handler {
	t.Helper()
	cfg := mustHandlerConfig(config.Config{CodexOAuth: config.CodexOAuthConfig{}})
	handler := NewHandler(cfg, store, nil, nil).WithCodexResponsesExecutor(executor)
	handler.ReplaceEffectiveCatalog(effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: "gpt-5.2-codex"}}}))
	return handler
}

func TestCodexOAuthResponsesNormalizesRequestAndSettlesUsage(t *testing.T) {
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
	var normalized map[string]any
	if err := json.Unmarshal(received.Body, &normalized); err != nil {
		t.Fatal(err)
	}
	if received.Model != "gpt-5.2-codex" || normalized["stream"] != true || normalized["store"] != false || normalized["metadata"] != nil {
		t.Fatalf("CP-REQ normalized request=%+v body=%s", received, received.Body)
	}
	if input, ok := normalized["input"].([]any); !ok || len(input) != 1 {
		t.Fatalf("CP-REQ-002 input=%#v", normalized["input"])
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

func TestCodexOAuthInvalidRequestReturnsClientError(t *testing.T) {
	failure := codexresponses.NewFailure(codexresponses.KindInvalidRequest, 0, fmt.Errorf("Codex upstream rejected the request"))
	failure.HTTPStatus = http.StatusBadRequest
	registry := aetherrelaymetrics.NewRegistry()
	cfg := mustHandlerConfig(config.Config{CodexOAuth: config.CodexOAuthConfig{}})
	handler := NewHandler(cfg, usage.NewMemoryStore(), nil, registry).WithCodexResponsesExecutor(codexResponsesExecutorStub{complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
		return codexresponses.Result{}, failure
	}})
	handler.ReplaceEffectiveCatalog(effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: "gpt-5.2-codex"}}}))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.2-codex","input":"hello"}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"invalid_request"`)) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if health := registry.ProviderHealthSnapshot(); len(health) != 0 {
		t.Fatalf("invalid request changed Provider health: %#v", health)
	}
}

func TestCodexOAuthDropsCompatibleMaxOutputTokensBeforeUpstream(t *testing.T) {
	var received codexresponses.Request
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{complete: func(_ context.Context, request codexresponses.Request) (codexresponses.Result, error) {
		received = request
		return codexresponses.Result{Body: []byte(`{"object":"response","id":"resp_1"}`)}, nil
	}})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.2-codex","input":"hello","max_output_tokens":64}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if bytes.Contains(received.Body, []byte("max_output_tokens")) {
		t.Fatalf("CP-REQ-017 field reached upstream: %s", received.Body)
	}
}

func TestCodexRequestRejectsUnsupportedStateAndCapabilityBeforeUpstream(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "previous response", body: `{"model":"gpt-5.2-codex","previous_response_id":"resp_external","input":"hello"}`},
		{name: "parallel type", body: `{"model":"gpt-5.2-codex","parallel_tool_calls":"yes","input":"hello"}`},
		{name: "parallel enabled", body: `{"model":"gpt-5.2-codex","parallel_tool_calls":true,"input":"hello"}`},
		{name: "client metadata", body: `{"model":"gpt-5.2-codex","client_metadata":{"tenant":"secret"},"input":"hello"}`},
		{name: "image input", body: `{"model":"gpt-5.2-codex","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`},
		{name: "computer tool", body: `{"model":"gpt-5.2-codex","input":"hello","tools":[{"type":"computer"}]}`},
		{name: "invalid item id", body: `{"model":"gpt-5.2-codex","input":[{"id":"bad id","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`},
		{name: "orphan tool output", body: `{"model":"gpt-5.2-codex","input":[{"type":"function_call_output","call_id":"call_missing","output":"done"}]}`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			called := false
			handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
				called = true
				return codexresponses.Result{}, nil
			}})
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(testCase.body))
			request.Header.Set("Authorization", "Bearer test-client-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || called {
				t.Fatalf("CP-REQ-014/016/019/021/022 status=%d called=%v body=%s", response.Code, called, response.Body.String())
			}
		})
	}
}

func TestCodexRequestNormalizesLegacyFunctionsAndToolContinuation(t *testing.T) {
	raw := []byte(`{"model":"gpt-test","functions":[{"name":"lookup","description":"lookup","parameters":{"type":"object"}}],"function_call":{"name":"lookup"},"input":[{"type":"function_call","call_id":"call_lookup","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_lookup","output":"done"}]}`)
	normalized, body, _, err := normalizeCodexRequest(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := body["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	choice, _ := body["tool_choice"].(map[string]any)
	if len(tools) != 1 || tool["name"] != "lookup" || tool["function"] != nil || choice["name"] != "lookup" || bytes.Contains(normalized, []byte(`"functions"`)) {
		t.Fatalf("CP-REQ-010/021 normalized=%s", normalized)
	}
	if !bytes.Contains(normalized, []byte(`"call_id":"fc_lookup"`)) {
		t.Fatalf("CP-REQ-012 normalized=%s", normalized)
	}
}

func TestCodexInternalHeadersAreAuditedByNameOnly(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("X-Codex-Turn-Metadata", `{"secret":"value"}`)
	request.Header.Set("X-Codex-Beta-Features", "unverified-feature")
	request.Header.Set("Cookie", "not-a-codex-contract-header")
	got := codexIgnoredHeaderNames(request)
	want := []string{"X-Codex-Turn-Metadata", "X-Codex-Beta-Features"}
	if !reflect.DeepEqual(got, want) || strings.Contains(strings.Join(got, ","), "secret") || strings.Contains(strings.Join(got, ","), "unverified") {
		t.Fatalf("CP-HDR-011..015 ignored=%v", got)
	}
}

func TestCodexCompactNormalizesUnaryAndBridgesSSE(t *testing.T) {
	var received codexresponses.Request
	executor := codexResponsesExecutorStub{complete: func(_ context.Context, request codexresponses.Request) (codexresponses.Result, error) {
		received = request
		return codexresponses.Result{Body: []byte(`{"id":"resp_compact","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"safe"}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`)}, nil
	}}
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), executor)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewBufferString(`{"model":"gpt-5.2-codex","stream":true,"store":true,"tool_choice":"auto","input":[{"type":"compaction_trigger"}]}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("CP-COMPACT status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	for _, forbidden := range []string{`"stream"`, `"store"`, `"tool_choice"`} {
		if bytes.Contains(received.Body, []byte(forbidden)) {
			t.Fatalf("CP-COMPACT-001 %s reached upstream: %s", forbidden, received.Body)
		}
	}
	if !bytes.Contains(received.Body, []byte(`"instructions":""`)) || !strings.Contains(response.Body.String(), "response.output_item.done") || !strings.Contains(response.Body.String(), "response.completed") {
		t.Fatalf("CP-COMPACT normalized=%s response=%s", received.Body, response.Body.String())
	}
}

func TestCodexCompactStreamHeartbeatsAndFailsInBand(t *testing.T) {
	previous := codexCompactHeartbeatInterval
	codexCompactHeartbeatInterval = time.Millisecond
	defer func() { codexCompactHeartbeatInterval = previous }()
	executor := codexResponsesExecutorStub{complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
		time.Sleep(5 * time.Millisecond)
		return codexresponses.Result{}, codexresponses.NewFailure(codexresponses.KindUpstream, http.StatusBadGateway, fmt.Errorf("upstream failed"))
	}}
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), executor)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewBufferString(`{"model":"gpt-5.2-codex","stream":true,"input":[{"type":"compaction_trigger"}]}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Count(response.Body.String(), ": aetherrelay compact pending") < 2 || !strings.Contains(response.Body.String(), "event: response.failed") || strings.Contains(response.Body.String(), `"type":"error"`) {
		t.Fatalf("CP-COMPACT-003 status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestChatCompletionsRoutesToCodexResponsesWithTools(t *testing.T) {
	var received codexresponses.Request
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{complete: func(_ context.Context, request codexresponses.Request) (codexresponses.Result, error) {
		received = request
		return codexresponses.Result{Body: []byte(`{"id":"resp_chat","model":"gpt-5.2-codex","status":"completed","output":[{"type":"function_call","call_id":"fc_lookup","name":"lookup","arguments":"{\"q\":\"x\"}"}],"usage":{"input_tokens":4,"output_tokens":2}}`)}, nil
	}})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-5.2-codex","messages":[{"role":"developer","content":"be brief"},{"role":"user","content":"lookup x"}],"tools":[{"type":"function","function":{"name":"lookup","description":"lookup","parameters":{"type":"object"}}}]}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"finish_reason":"tool_calls"`) || !strings.Contains(response.Body.String(), `"tool_calls"`) {
		t.Fatalf("CP-EP-007 status=%d body=%s", response.Code, response.Body.String())
	}
	var upstream map[string]any
	if err := json.Unmarshal(received.Body, &upstream); err != nil {
		t.Fatal(err)
	}
	tools, _ := upstream["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	if upstream["instructions"] != "be brief" || tool["name"] != "lookup" || tool["function"] != nil {
		t.Fatalf("CP-EP-007 upstream=%s", received.Body)
	}
}

func TestChatCompletionsStreamsCodexToolCallAndDone(t *testing.T) {
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{stream: func(_ context.Context, _ codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
		if err := started(codexresponses.StreamStart{}); err != nil {
			return err
		}
		for _, line := range []string{
			`data: {"type":"response.created","response":{"id":"resp_stream","model":"gpt-5.2-codex"}}` + "\n\n",
			`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"fc_lookup","name":"lookup"}}` + "\n\n",
			`data: {"type":"response.function_call_arguments.delta","delta":"{\"q\":"}` + "\n\n",
			`data: {"type":"response.function_call_arguments.delta","delta":"\"x\"}"}` + "\n\n",
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}` + "\n\n",
		} {
			if err := emit([]byte(line)); err != nil {
				return err
			}
		}
		return nil
	}})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-5.2-codex","stream":true,"messages":[{"role":"user","content":"lookup x"}]}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"finish_reason":"tool_calls"`) || !strings.Contains(response.Body.String(), `data: [DONE]`) || !strings.Contains(response.Body.String(), `"arguments":"{\"q\":"`) {
		t.Fatalf("CP-EP-007 status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAnthropicMessagesRoutesToCodexResponsesWithTools(t *testing.T) {
	var received codexresponses.Request
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{complete: func(_ context.Context, request codexresponses.Request) (codexresponses.Result, error) {
		received = request
		return codexresponses.Result{Body: []byte(`{"id":"resp_tool","object":"response","model":"gpt-5.2-codex","status":"completed","output":[{"type":"function_call","call_id":"fc_lookup","name":"lookup","arguments":"{\"q\":\"x\"}"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`)}, nil
	}})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"gpt-5.2-codex","max_tokens":64,"system":"be brief","messages":[{"role":"user","content":"lookup x"}],"tools":[{"name":"lookup","description":"lookup","input_schema":{"type":"object"}}]}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"type":"tool_use"`) || !strings.Contains(response.Body.String(), `"stop_reason":"tool_use"`) {
		t.Fatalf("CP-EP-008 status=%d body=%s", response.Code, response.Body.String())
	}
	var upstream map[string]any
	if err := json.Unmarshal(received.Body, &upstream); err != nil {
		t.Fatal(err)
	}
	if upstream["instructions"] != "be brief" || upstream["store"] != false || upstream["max_output_tokens"] != nil || upstream["tools"] == nil {
		t.Fatalf("CP-EP-008 upstream=%s", received.Body)
	}
}

func TestAnthropicMessagesStreamsFromCodexResponses(t *testing.T) {
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{stream: func(_ context.Context, request codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
		if err := started(codexresponses.StreamStart{}); err != nil {
			return err
		}
		for _, line := range []string{
			`data: {"type":"response.created","response":{"id":"resp_stream","model":"gpt-5.2-codex"}}` + "\n\n",
			`data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n",
			`data: {"type":"response.completed","response":{"id":"resp_stream","model":"gpt-5.2-codex","status":"completed","usage":{"input_tokens":2,"output_tokens":1}}}` + "\n\n",
		} {
			if err := emit([]byte(line)); err != nil {
				return err
			}
		}
		return nil
	}})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"gpt-5.2-codex","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(response.Body.String(), "content_block_delta") || !strings.Contains(response.Body.String(), "message_stop") {
		t.Fatalf("CP-EP-008 status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestAnthropicMessagesStreamsCodexToolUse(t *testing.T) {
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{stream: func(_ context.Context, _ codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
		if err := started(codexresponses.StreamStart{}); err != nil {
			return err
		}
		for _, line := range []string{
			`data: {"type":"response.created","response":{"id":"resp_tool_stream","model":"gpt-5.2-codex"}}` + "\n\n",
			`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"fc_lookup","name":"lookup"}}` + "\n\n",
			`data: {"type":"response.function_call_arguments.delta","delta":"{\"q\":\"x\"}"}` + "\n\n",
			`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"fc_lookup","name":"lookup","arguments":"{\"q\":\"x\"}"}}` + "\n\n",
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":2,"output_tokens":2}}}` + "\n\n",
		} {
			if err := emit([]byte(line)); err != nil {
				return err
			}
		}
		return nil
	}})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"gpt-5.2-codex","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"lookup x"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"type":"tool_use"`) || !strings.Contains(response.Body.String(), `"type":"input_json_delta"`) || !strings.Contains(response.Body.String(), `"stop_reason":"tool_use"`) || !strings.Contains(response.Body.String(), `event: message_stop`) {
		t.Fatalf("CP-EP-008 tool stream status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestResponsesFallsBackFromDirectProviderToCodexOAuth(t *testing.T) {
	cfg := mustHandlerConfig(config.Config{
		CodexOAuth: config.CodexOAuthConfig{},
		Providers: map[string]config.Provider{
			"primary": {
				Name: "primary", Protocol: "openai", BaseURL: "https://primary.test", APIKey: "k", Models: []string{"gpt-shared"}, Priority: 200,
				Endpoints: []string{config.ProviderEndpointChatCompletions, config.ProviderEndpointResponses},
			},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"gpt-shared": {ID: "gpt-shared", ContextWindowTokens: 128000, MaxOutputTokens: 16384},
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
		CodexOAuth: config.CodexOAuthConfig{},
		Providers: map[string]config.Provider{
			"backup": {
				Name: "backup", Protocol: "openai", BaseURL: "https://backup.test", APIKey: "k", Models: []string{"gpt-shared"}, Priority: 20,
				Endpoints: []string{config.ProviderEndpointChatCompletions, config.ProviderEndpointResponses},
			},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"gpt-shared": {ID: "gpt-shared", ContextWindowTokens: 128000, MaxOutputTokens: 16384},
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
