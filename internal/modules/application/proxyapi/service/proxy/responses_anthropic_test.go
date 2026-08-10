package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	archive "aetherrelay/internal/pkg/aetherrelayarchive"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	metrics "aetherrelay/internal/pkg/aetherrelaymetrics"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

type contextBlockingReadCloser struct{ ctx context.Context }

func (b contextBlockingReadCloser) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (contextBlockingReadCloser) Close() error { return nil }

type failingConversionResponseWriter struct {
	header http.Header
	status int
}

func (w *failingConversionResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingConversionResponseWriter) WriteHeader(status int) { w.status = status }
func (w *failingConversionResponseWriter) Write([]byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return 0, io.ErrClosedPipe
}

func TestResponsesAnthropicLevel1TextConversion(t *testing.T) {
	body, err := buildAnthropicFromResponses(map[string]any{"input": "hello", "max_output_tokens": float64(32)}, "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if request["max_tokens"] != float64(32) {
		t.Fatalf("max_tokens = %#v", request["max_tokens"])
	}
	messages := request["messages"].([]any)
	if messages[0].(map[string]any)["content"] != "hello" {
		t.Fatalf("messages = %#v", messages)
	}

	response, usage, err := convertAnthropicToResponses([]byte(`{"id":"msg_1","model":"claude-test","content":[{"type":"text","text":"world"}],"usage":{"input_tokens":2,"output_tokens":3}}`), "claude-test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response), `"object":"response"`) || !strings.Contains(string(response), "world") {
		t.Fatalf("response = %s", response)
	}
	if usage.PromptTokens != 2 || usage.CompletionTokens != 3 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestResponsesAnthropicPreservesBoundedTerminationReasons(t *testing.T) {
	response, _, err := convertOpenAIResponsesToAnthropic([]byte(`{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`), "claude-test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response), `"stop_reason":"max_tokens"`) {
		t.Fatalf("response = %s", response)
	}
	if _, _, err := convertOpenAIResponsesToAnthropic([]byte(`{"id":"resp_1","status":"failed","output":[]}`), "claude-test"); err == nil {
		t.Fatal("expected failed Responses status to be rejected")
	}

	response, _, err = convertAnthropicToResponses([]byte(`{"id":"msg_1","model":"claude-test","stop_reason":"max_tokens","content":[{"type":"text","text":"partial"}]}`), "claude-test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response), `"status":"incomplete"`) || !strings.Contains(string(response), `"reason":"max_output_tokens"`) {
		t.Fatalf("response = %s", response)
	}
}

func TestAnthropicToolResultArrayIsNotDropped(t *testing.T) {
	response, _, err := convertAnthropicToResponses([]byte(`{"id":"msg_1","model":"claude-test","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}},{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"ok"}]}]}`), "claude-test")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(response, &payload); err != nil {
		t.Fatal(err)
	}
	output := payload["output"].([]any)
	if output[1].(map[string]any)["output"] != `[{"text":"ok","type":"text"}]` {
		t.Fatalf("response = %s", response)
	}
}

func TestResponsesAnthropicLevel3RoundTripThroughHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("upstream path = %s", r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Fatalf("conversion leaked client query upstream: %q", r.URL.RawQuery)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request["tools"].([]any)) != 1 {
			t.Fatalf("upstream tools = %#v", request["tools"])
		}
		messagesJSON, _ := json.Marshal(request["messages"])
		if !strings.Contains(string(messagesJSON), `"type":"tool_use"`) || !strings.Contains(string(messagesJSON), `"id":"call_history"`) || !strings.Contains(string(messagesJSON), `"type":"tool_result"`) || !strings.Contains(string(messagesJSON), `"tool_use_id":"call_history"`) {
			t.Fatalf("upstream messages = %s", messagesJSON)
		}
		if request["max_tokens"] != float64(32) {
			t.Fatalf("bounded max_tokens = %#v", request["max_tokens"])
		}
		if _, ok := request["thinking"]; ok {
			t.Fatal("conversion must not synthesize Anthropic thinking from Responses reasoning default")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","model":"claude-test","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}],"usage":{"input_tokens":2,"output_tokens":3}}`))
	}))
	defer upstream.Close()
	cfg := config.Config{Providers: map[string]config.Provider{
		"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: upstream.URL, APIKey: "test", Models: []string{"claude-test"}, Endpoints: []string{config.ProviderEndpointMessages}},
	}, ModelMetadata: map[string]config.ModelMetadata{
		"claude-test": {ID: "claude-test", ContextWindowTokens: 128000, MaxOutputTokens: 32, ReasoningSupported: true, ReasoningDeclared: true, ReasoningDefaultEffort: "low", ReasoningEfforts: []string{"low"}, ConversionCapabilities: map[string]config.ConversionCapability{
			config.ProviderEndpointMessages: {Level: 3, Text: true, Tools: true},
		}},
	}}
	h := NewHandler(mustHandlerConfig(cfg), usage.NewMemoryStore(), nil, metrics.NewRegistry())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses?beta=client-only", strings.NewReader(`{"model":"claude-test","input":[{"type":"message","role":"user","content":"look"},{"type":"function_call","call_id":"call_history","name":"lookup","arguments":"{\"q\":\"x\"}"},{"type":"function_call_output","call_id":"call_history","output":"history result"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"function_call"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAnthropicResponsesLevel3RoundTripThroughHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("upstream path = %s", r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Fatalf("conversion leaked client query upstream: %q", r.URL.RawQuery)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request["tools"].([]any)) != 1 {
			t.Fatalf("upstream tools = %#v", request["tools"])
		}
		inputJSON, _ := json.Marshal(request["input"])
		if !strings.Contains(string(inputJSON), `"type":"function_call"`) || !strings.Contains(string(inputJSON), `"call_id":"call_history"`) || !strings.Contains(string(inputJSON), `"type":"function_call_output"`) {
			t.Fatalf("upstream input = %s", inputJSON)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":2,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	cfg := config.Config{Providers: map[string]config.Provider{
		"openai": {Name: "openai", Protocol: "openai", BaseURL: upstream.URL, APIKey: "test", Models: []string{"gpt-test"}, Endpoints: []string{config.ProviderEndpointChatCompletions, config.ProviderEndpointResponses}},
	}, ModelMetadata: map[string]config.ModelMetadata{
		"gpt-test": {ID: "gpt-test", ContextWindowTokens: 128000, MaxOutputTokens: 32, ConversionCapabilities: map[string]config.ConversionCapability{
			config.ProviderEndpointResponses: {Level: 3, Text: true, Tools: true},
		}},
	}}
	h := NewHandler(mustHandlerConfig(cfg), usage.NewMemoryStore(), nil, metrics.NewRegistry())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=client-only", strings.NewReader(`{"model":"gpt-test","max_tokens":32,"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"tool_use","id":"call_history","name":"lookup","input":{"q":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_history","content":"history result"}]}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"type":"message"`) || !strings.Contains(rec.Body.String(), "done") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResponsesAnthropicLevel2StreamingThroughHTTP(t *testing.T) {
	tests := []struct {
		name                  string
		provider              config.Provider
		capabilityDirection   string
		requestPath           string
		requestBody           string
		wantUpstreamPath      string
		upstreamBody          string
		wantResponseFragments []string
		wantConversionMode    string
		wantInputTokens       int64
		wantOutputTokens      int64
	}{
		{
			name:                "responses to anthropic upstream",
			provider:            config.Provider{Name: "anthropic", Protocol: "anthropic", BaseURL: "https://anthropic.test", APIKey: "test", Models: []string{"shared-model"}, Endpoints: []string{config.ProviderEndpointMessages}},
			capabilityDirection: TransportModeResponsesToAnthropic,
			requestPath:         "/v1/responses",
			requestBody:         `{"model":"shared-model","stream":true,"input":"hello"}`,
			wantUpstreamPath:    "/v1/messages",
			upstreamBody: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"shared-model\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n" +
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			wantResponseFragments: []string{"response.created", "response.output_text.delta", "answer", "response.completed"},
			wantConversionMode:    TransportModeResponsesToAnthropic,
			wantInputTokens:       3,
			wantOutputTokens:      2,
		},
		{
			name:                "anthropic to responses upstream",
			provider:            config.Provider{Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "test", Models: []string{"shared-model"}, Endpoints: []string{config.ProviderEndpointResponses}},
			capabilityDirection: TransportModeAnthropicToResponses,
			requestPath:         "/v1/messages",
			requestBody:         `{"model":"shared-model","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`,
			wantUpstreamPath:    "/v1/responses",
			upstreamBody: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"shared-model\"}}\n\n" +
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}}\n\n",
			wantResponseFragments: []string{"message_start", "content_block_delta", "answer", "message_stop"},
			wantConversionMode:    TransportModeAnthropicToResponses,
			wantInputTokens:       4,
			wantOutputTokens:      2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			interactionRoot := filepath.Join(t.TempDir(), "interactions")
			recorder, err := archive.NewRecorder(interactionRoot)
			if err != nil {
				t.Fatal(err)
			}
			store := usage.NewMemoryStore()
			registry := metrics.NewRegistry()
			provider := tc.provider
			cfg := mustHandlerConfig(config.Config{
				Providers: map[string]config.Provider{provider.Name: provider},
				ModelMetadata: map[string]config.ModelMetadata{
					"shared-model": {ID: "shared-model", ContextWindowTokens: 128000, MaxOutputTokens: 4096, ConversionCapabilities: map[string]config.ConversionCapability{
						provider.Endpoints[0]: {Level: 2, Text: true, Streaming: true},
					}},
				},
			})
			h := NewHandler(cfg, store, recorder, registry)
			h.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != tc.wantUpstreamPath {
					t.Fatalf("upstream path=%s, want %s", r.URL.Path, tc.wantUpstreamPath)
				}
				return testResponse(http.StatusOK, "text/event-stream", tc.upstreamBody), nil
			})

			req := httptest.NewRequest(http.MethodPost, tc.requestPath, strings.NewReader(tc.requestBody))
			req.Header.Set("Authorization", "Bearer test-client-key")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			for _, fragment := range tc.wantResponseFragments {
				if !strings.Contains(rec.Body.String(), fragment) {
					t.Fatalf("response missing %q: %s", fragment, rec.Body.String())
				}
			}

			events := usageEvents(t, store)
			if len(events) != 1 {
				t.Fatalf("usage events=%d, want 1", len(events))
			}
			event := events[0]
			if event.ConversionMode != tc.wantConversionMode || event.ConversionLevel != 2 || event.InputTokens != tc.wantInputTokens || event.OutputTokens != tc.wantOutputTokens || event.Outcome != "success" {
				t.Fatalf("usage event=%+v", event)
			}

			roundDir := filepath.Join(interactionRoot, "000001")
			assertFileContains(t, filepath.Join(roundDir, "response.sse"), tc.wantResponseFragments[len(tc.wantResponseFragments)-1])
			assertFileContains(t, filepath.Join(roundDir, "metadata.json"), `"conversion_mode": "`+tc.wantConversionMode+`"`)
			assertFileContains(t, filepath.Join(roundDir, "metadata.json"), `"conversion_level": 2`)
			assertFileContains(t, filepath.Join(roundDir, "metadata.json"), `"outcome": "success"`)
		})
	}
}

func TestResponsesAnthropicConversionCancellationBeforeFirstEvent(t *testing.T) {
	tests := []struct {
		name, providerName, providerProtocol, providerEndpoint, direction, requestPath, requestBody string
	}{
		{name: "responses to anthropic", providerName: "anthropic", providerProtocol: "anthropic", providerEndpoint: config.ProviderEndpointMessages, direction: TransportModeResponsesToAnthropic, requestPath: "/v1/responses", requestBody: `{"model":"shared-model","stream":true,"input":"hello"}`},
		{name: "anthropic to responses", providerName: "openai", providerProtocol: "openai", providerEndpoint: config.ProviderEndpointResponses, direction: TransportModeAnthropicToResponses, requestPath: "/v1/messages", requestBody: `{"model":"shared-model","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			interactionRoot := filepath.Join(t.TempDir(), "interactions")
			recorder, err := archive.NewRecorder(interactionRoot)
			if err != nil {
				t.Fatal(err)
			}
			store := usage.NewMemoryStore()
			registry := metrics.NewRegistry()
			provider := config.Provider{Name: tc.providerName, Protocol: tc.providerProtocol, BaseURL: "https://upstream.test", APIKey: "test", Models: []string{"shared-model"}, Endpoints: []string{tc.providerEndpoint}}
			cfg := mustHandlerConfig(config.Config{
				Providers: map[string]config.Provider{tc.providerName: provider},
				ModelMetadata: map[string]config.ModelMetadata{"shared-model": {ID: "shared-model", ContextWindowTokens: 128000, MaxOutputTokens: 4096, ConversionCapabilities: map[string]config.ConversionCapability{
					tc.providerEndpoint: {Level: 2, Text: true, Streaming: true},
				}}},
			})
			cfg.StreamFirstEventTimeout = time.Second
			h := NewHandler(cfg, store, recorder, registry)
			upstreamStarted := make(chan struct{})
			h.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				close(upstreamStarted)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: contextBlockingReadCloser{ctx: r.Context()}}, nil
			})

			ctx, cancel := context.WithCancel(context.Background())
			req := httptest.NewRequest(http.MethodPost, tc.requestPath, strings.NewReader(tc.requestBody)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer test-client-key")
			done := make(chan struct{})
			go func() {
				defer close(done)
				h.ServeHTTP(httptest.NewRecorder(), req)
			}()
			<-upstreamStarted
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler did not stop after client cancellation")
			}

			events := usageEvents(t, store)
			if len(events) != 1 || events[0].Outcome != "client_canceled" || events[0].ErrorCode != "client_canceled" || events[0].HTTPStatus != 499 || events[0].ConversionMode != tc.direction || events[0].ConversionLevel != 2 {
				t.Fatalf("usage events=%+v", events)
			}
			assertFileContains(t, filepath.Join(interactionRoot, "000001", "metadata.json"), `"outcome": "client_canceled"`)
			assertFileContains(t, filepath.Join(interactionRoot, "000001", "metadata.json"), `"http_status": 499`)
			var prometheus strings.Builder
			if err := registry.WritePrometheus(&prometheus); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(prometheus.String(), `aetherrelay_upstream_errors_total{provider="`+tc.providerName+`"`) {
				t.Fatalf("client cancellation counted as upstream error:\n%s", prometheus.String())
			}
		})
	}
}

func TestConvertedSSEClientWriteFailureIsSettled(t *testing.T) {
	interactionRoot := filepath.Join(t.TempDir(), "interactions")
	recorder, err := archive.NewRecorder(interactionRoot)
	if err != nil {
		t.Fatal(err)
	}
	store := usage.NewMemoryStore()
	cfg := mustHandlerConfig(config.Config{
		Providers: map[string]config.Provider{"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: "https://anthropic.test", APIKey: "test", Models: []string{"shared-model"}, Endpoints: []string{config.ProviderEndpointMessages}}},
		ModelMetadata: map[string]config.ModelMetadata{"shared-model": {ID: "shared-model", ContextWindowTokens: 128000, MaxOutputTokens: 4096, ConversionCapabilities: map[string]config.ConversionCapability{
			config.ProviderEndpointMessages: {Level: 2, Text: true, Streaming: true},
		}}},
	})
	h := NewHandler(cfg, store, recorder, metrics.NewRegistry())
	h.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"shared-model\",\"usage\":{\"input_tokens\":1}}}\n\n"
		return testResponse(http.StatusOK, "text/event-stream", body), nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"shared-model","stream":true,"input":"hello"}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	w := &failingConversionResponseWriter{}
	h.ServeHTTP(w, req)
	if w.status != http.StatusOK {
		t.Fatalf("status=%d", w.status)
	}
	events := usageEvents(t, store)
	if len(events) != 1 || events[0].Outcome != "client_write" || events[0].ErrorCode != "client_write" || events[0].HTTPStatus != http.StatusOK {
		t.Fatalf("usage events=%+v", events)
	}
	assertFileContains(t, filepath.Join(interactionRoot, "000001", "metadata.json"), `"outcome": "client_write"`)
}

func TestResponsesAnthropicConversionPreflightRejectsBeforeUpstream(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: "https://anthropic.invalid", APIKey: "test", Models: []string{"claude-test"}, Endpoints: []string{config.ProviderEndpointMessages}},
			"openai":    {Name: "openai", Protocol: "openai", BaseURL: "https://openai.invalid", APIKey: "test", Models: []string{"gpt-test"}, Endpoints: []string{config.ProviderEndpointResponses}},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"claude-test": {ID: "claude-test", ContextWindowTokens: 128000, MaxOutputTokens: 32, ConversionCapabilities: map[string]config.ConversionCapability{
				config.ProviderEndpointMessages: {Level: 3, Text: true, Streaming: true, Tools: true},
			}},
			"gpt-test": {ID: "gpt-test", ContextWindowTokens: 128000, MaxOutputTokens: 32, ConversionCapabilities: map[string]config.ConversionCapability{
				config.ProviderEndpointResponses: {Level: 3, Text: true, Streaming: true, Tools: true},
			}},
		},
	}
	tests := []struct {
		name, path, body string
	}{
		{name: "responses streaming tools", path: "/v1/responses", body: `{"model":"claude-test","stream":true,"input":"look","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`},
		{name: "anthropic streaming tools", path: "/v1/messages", body: `{"model":"gpt-test","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"look"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`},
		{name: "responses output limit", path: "/v1/responses", body: `{"model":"claude-test","input":"hello","max_output_tokens":33}`},
		{name: "anthropic output limit", path: "/v1/messages", body: `{"model":"gpt-test","max_tokens":33,"messages":[{"role":"user","content":"hello"}]}`},
		{name: "responses dangling tool call", path: "/v1/responses", body: `{"model":"claude-test","input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`},
		{name: "anthropic dangling tool use", path: "/v1/messages", body: `{"model":"gpt-test","max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}]}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`},
		{name: "anthropic tool use with user role", path: "/v1/messages", body: `{"model":"gpt-test","max_tokens":32,"messages":[{"role":"user","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}]}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstreamCalls := 0
			h := NewHandler(mustHandlerConfig(cfg), usage.NewMemoryStore(), nil, metrics.NewRegistry())
			h.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				upstreamCalls++
				return nil, fmt.Errorf("unexpected upstream request")
			})
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer test-client-key")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorCodeConversionUnsupported) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if upstreamCalls != 0 {
				t.Fatalf("upstream calls=%d, want 0", upstreamCalls)
			}
		})
	}
}

func TestResponsesNativeSelectionAndAnthropicFallback(t *testing.T) {
	tests := []struct {
		name             string
		nativeStatus     int
		wantAnthropicHit bool
		wantBody         string
	}{
		{name: "native success", nativeStatus: http.StatusOK, wantBody: `"id":"resp_native"`},
		{name: "native retryable failure", nativeStatus: http.StatusBadGateway, wantAnthropicHit: true, wantBody: "converted fallback"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hosts := []string{}
			cfg := config.Config{
				Providers: map[string]config.Provider{
					"conversion": {Name: "conversion", Protocol: "anthropic", BaseURL: "https://anthropic.test", APIKey: "test", Models: []string{"shared-model"}, Endpoints: []string{config.ProviderEndpointMessages}, Priority: 200, Fallback: true},
					"native":     {Name: "native", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "test", Models: []string{"shared-model"}, Endpoints: []string{config.ProviderEndpointResponses}, Priority: 100, Fallback: true},
				},
				ModelMetadata: map[string]config.ModelMetadata{
					"shared-model": {ID: "shared-model", ContextWindowTokens: 128000, MaxOutputTokens: 4096, ConversionCapabilities: map[string]config.ConversionCapability{
						config.ProviderEndpointMessages: {Level: 1, Text: true},
					}},
				},
			}
			h := NewHandler(mustHandlerConfig(cfg), usage.NewMemoryStore(), nil, metrics.NewRegistry())
			h.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				hosts = append(hosts, r.URL.Host)
				if r.URL.Host == "openai.test" {
					return testResponse(tc.nativeStatus, "application/json", `{"id":"resp_native","model":"shared-model","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"native"}]}]}`), nil
				}
				return testResponse(http.StatusOK, "application/json", `{"id":"msg_fallback","type":"message","role":"assistant","model":"shared-model","content":[{"type":"text","text":"converted fallback"}],"usage":{"input_tokens":1,"output_tokens":2}}`), nil
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"shared-model","input":"hello"}`))
			req.Header.Set("Authorization", "Bearer test-client-key")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("status=%d body=%s hosts=%v", rec.Code, rec.Body.String(), hosts)
			}
			anthropicHit := len(hosts) == 2 && hosts[1] == "anthropic.test"
			wantCalls := 1
			if tc.wantAnthropicHit {
				wantCalls = 2
			}
			if anthropicHit != tc.wantAnthropicHit || len(hosts) != wantCalls {
				t.Fatalf("hosts=%v, want anthropic fallback=%v", hosts, tc.wantAnthropicHit)
			}
		})
	}
}

func TestAnthropicResponsesLevel1TextConversion(t *testing.T) {
	body, err := buildResponsesFromAnthropic(map[string]any{"system": []any{map[string]any{"type": "text", "text": "be concise"}}, "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "max_tokens": float64(64)}, "gpt-test", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"max_output_tokens":64`) || !strings.Contains(string(body), "hello") || !strings.Contains(string(body), "be concise") {
		t.Fatalf("request = %s", body)
	}

	response, usage, err := convertOpenAIResponsesToAnthropic([]byte(`{"id":"resp_1","model":"gpt-test","output":[{"type":"message","content":[{"type":"output_text","text":"world"}]}],"usage":{"input_tokens":4,"output_tokens":5}}`), "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response), `"type":"message"`) || !strings.Contains(string(response), "world") {
		t.Fatalf("response = %s", response)
	}
	if usage.PromptTokens != 4 || usage.CompletionTokens != 5 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestResponsesToAnthropicAcceptsFinalAnswerPhaseAndEmptyLogprobs(t *testing.T) {
	response, _, err := convertOpenAIResponsesToAnthropic([]byte(`{"id":"resp_1","model":"deepseek-v4-flash","status":"completed","output":[{"type":"message","phase":"final_answer","content":[{"type":"output_text","text":"world","annotations":[],"logprobs":[]}]}]}`), "deepseek-v4-flash")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response), "world") {
		t.Fatalf("response = %s", response)
	}

	for _, body := range []string{
		`{"output":[{"type":"message","phase":"commentary","content":[{"type":"output_text","text":"answer"}]}]}`,
		`{"output":[{"type":"message","phase":"final_answer","content":[{"type":"output_text","text":"answer","logprobs":[{"token":"answer"}]}]}]}`,
	} {
		if _, _, err := convertOpenAIResponsesToAnthropic([]byte(body), "deepseek-v4-flash"); err == nil {
			t.Fatalf("unsupported response was accepted: %s", body)
		}
	}
}

func TestResponsesAnthropicTextSSEEventConversion(t *testing.T) {
	state := &textConversionStreamState{}
	events, err := responsesEventToAnthropic([]byte(`{"type":"response.created","response":{"id":"resp_1","model":"gpt-test"}}`), state)
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	events, err = responsesEventToAnthropic([]byte(`{"type":"response.output_text.delta","delta":"hello"}`), state)
	if err != nil || events[0]["type"] != "content_block_delta" {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	events, err = responsesEventToAnthropic([]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`), state)
	if err != nil || len(events) != 3 || !state.Completed {
		t.Fatalf("events=%#v state=%#v err=%v", events, state, err)
	}
}

func TestResponsesToAnthropicSSEAcceptsBoundedReasoningEvents(t *testing.T) {
	capability := config.ConversionCapability{Reasoning: true}
	state := &textConversionStreamState{}
	mapper := responsesEventToAnthropicWithCapability(capability)
	if _, err := mapper([]byte(`{"type":"response.created","response":{"id":"resp_1","model":"deepseek-v4-flash"}}`), state); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{
		`{"type":"response.content_part.added","part":{"type":"reasoning_text"}}`,
		`{"type":"response.reasoning_text.delta","delta":"hidden"}`,
		`{"type":"response.reasoning_text.done","text":"hidden"}`,
		`{"type":"response.content_part.done","part":{"type":"reasoning_text"}}`,
		`{"type":"response.content_part.done","part":{"type":"output_text"}}`,
	} {
		if _, err := mapper([]byte(event), state); err != nil {
			t.Fatalf("event %s: %v", event, err)
		}
	}
	if len(state.IgnoredFeatures) != 1 || state.IgnoredFeatures[0] != "reasoning_output" {
		t.Fatalf("ignored features = %v", state.IgnoredFeatures)
	}

	strictState := &textConversionStreamState{Started: true}
	if _, err := responsesEventToAnthropic([]byte(`{"type":"response.reasoning_text.delta","delta":"hidden"}`), strictState); err == nil {
		t.Fatal("reasoning event must require an explicit conversion capability")
	}
}

func TestAnthropicTextSSEEventConversionIncludesBlockStop(t *testing.T) {
	state := &textConversionStreamState{}
	if _, err := anthropicEventToResponses([]byte(`{"type":"message_start","message":{"id":"m1","model":"claude-test"}}`), state); err != nil {
		t.Fatal(err)
	}
	if _, err := anthropicEventToResponses([]byte(`{"type":"content_block_start","content_block":{"type":"text","text":""}}`), state); err != nil {
		t.Fatal(err)
	}
	events, err := anthropicEventToResponses([]byte(`{"type":"content_block_stop"}`), state)
	if err != nil || len(events) != 2 || events[0]["type"] != "response.output_text.done" {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}

func TestAnthropicSSEIncompleteMapsToResponsesIncomplete(t *testing.T) {
	state := &textConversionStreamState{}
	if _, err := anthropicEventToResponses([]byte(`{"type":"message_start","message":{"id":"m1","model":"claude-test"}}`), state); err != nil {
		t.Fatal(err)
	}
	if _, err := anthropicEventToResponses([]byte(`{"type":"content_block_start","content_block":{"type":"text","text":""}}`), state); err != nil {
		t.Fatal(err)
	}
	if _, err := anthropicEventToResponses([]byte(`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":1}}`), state); err != nil {
		t.Fatal(err)
	}
	if _, err := anthropicEventToResponses([]byte(`{"type":"content_block_stop","index":0}`), state); err != nil {
		t.Fatal(err)
	}
	events, err := anthropicEventToResponses([]byte(`{"type":"message_stop"}`), state)
	if err != nil || len(events) != 1 || events[0]["type"] != "response.incomplete" {
		t.Fatalf("events=%#v state=%#v err=%v", events, state, err)
	}
}

func TestResponsesSSEIncompleteMapsToAnthropicMaxTokens(t *testing.T) {
	state := &textConversionStreamState{}
	if _, err := responsesEventToAnthropic([]byte(`{"type":"response.created","response":{"id":"r1","model":"gpt-test"}}`), state); err != nil {
		t.Fatal(err)
	}
	events, err := responsesEventToAnthropic([]byte(`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1,"output_tokens":2}}}`), state)
	if err != nil || len(events) != 3 {
		t.Fatalf("events=%#v state=%#v err=%v", events, state, err)
	}
	if delta, _ := events[1]["delta"].(map[string]any); delta["stop_reason"] != "max_tokens" {
		t.Fatalf("events=%#v", events)
	}
}

func TestEncodeConversionSSE(t *testing.T) {
	encoded, err := encodeConversionSSE([]map[string]any{{"type": "response.output_text.delta", "delta": "hi"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "event: response.output_text.delta\ndata: {\"delta\":\"hi\",\"type\":\"response.output_text.delta\"}\n\n" {
		t.Fatalf("encoded=%q", encoded)
	}
	if _, err := encodeConversionSSE([]map[string]any{{"delta": "missing type"}}, true); err == nil {
		t.Fatal("expected missing event type error")
	}
}

func TestConversionSSERejectsEventsAfterCompletion(t *testing.T) {
	state := &textConversionStreamState{Completed: true}
	if _, err := responsesEventToAnthropic([]byte(`{"type":"response.output_text.delta","delta":"late"}`), state); err == nil {
		t.Fatal("expected post-completion rejection")
	}
	if _, err := anthropicEventToResponses([]byte(`{"type":"ping"}`), state); err == nil {
		t.Fatal("expected post-completion rejection")
	}
}

func TestConvertSSEReaderRequiresTerminalEvent(t *testing.T) {
	input := strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"gpt\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	out, err := convertSSEReader(input, responsesEventToAnthropic, &textConversionStreamState{}, true)
	if err != nil || !strings.Contains(string(out), "message_stop") {
		t.Fatalf("out=%s err=%v", out, err)
	}
	if _, err := convertSSEReader(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{}}\n\n"), responsesEventToAnthropic, &textConversionStreamState{}, true); err == nil {
		t.Fatal("expected missing terminal error")
	}
}

func TestConvertSSEReaderContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := convertSSEReaderContext(ctx, strings.NewReader("data: {}\n\n"), responsesEventToAnthropic, &textConversionStreamState{}, true); err == nil {
		t.Fatal("expected context cancellation")
	}
}

func TestFunctionToolDefinitionConversion(t *testing.T) {
	tools, err := responsesToolsToAnthropic([]any{map[string]any{"type": "function", "name": "lookup", "description": "find", "parameters": map[string]any{"type": "object"}}})
	if err != nil || tools[0]["name"] != "lookup" || tools[0]["input_schema"] == nil {
		t.Fatalf("tools=%#v err=%v", tools, err)
	}
	back, err := anthropicToolsToResponses([]any{tools[0]})
	if err != nil || back[0]["type"] != "function" || back[0]["parameters"] == nil {
		t.Fatalf("tools=%#v err=%v", back, err)
	}
	if _, err := responsesToolsToAnthropic([]any{map[string]any{"type": "web_search"}}); err == nil {
		t.Fatal("expected non-function rejection")
	}
}

func TestResponsesAnthropicRejectsUnmodelledFields(t *testing.T) {
	base := map[string]any{"input": "hello"}
	for _, feature := range []string{"parallel_tool_calls", "store", "truncation", "include", "conversation", "prompt", "metadata", "service_tier", "reasoning", "previous_response_id"} {
		body := map[string]any{}
		for key, value := range base {
			body[key] = value
		}
		body[feature] = true
		if _, err := buildAnthropicFromResponses(body, "claude-test", false); err == nil {
			t.Fatalf("expected Responses field %q to be rejected", feature)
		}
	}
	if _, err := buildResponsesFromAnthropic(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"thinking": map[string]any{"type": "enabled"},
	}, "gpt-test", false); err == nil {
		t.Fatal("expected Anthropic thinking to be rejected")
	}
	if _, err := buildResponsesFromAnthropic(map[string]any{
		"messages":       []any{map[string]any{"role": "user", "content": "hello"}},
		"stop_sequences": []any{"DONE"},
	}, "gpt-test", false); err == nil {
		t.Fatal("expected Anthropic stop_sequences to be rejected")
	}
}

func TestReasoningAdaptersUseConfiguredTargetEffort(t *testing.T) {
	responsesToAnthropic := config.ConversionCapability{
		Level: 2, Text: true, Streaming: true, Reasoning: true,
		ReasoningAdapter: config.ReasoningAdapterResponsesToAnthropicAdaptive, ReasoningTargetEffort: "medium",
	}
	body, ignored, err := buildAnthropicFromResponsesWithCapability(map[string]any{
		"input": "hello", "reasoning": map[string]any{"effort": "high"},
	}, "claude-test", false, responsesToAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if request["thinking"].(map[string]any)["type"] != "adaptive" || request["output_config"].(map[string]any)["effort"] != "medium" || !strings.Contains(strings.Join(ignored, ","), "reasoning") {
		t.Fatalf("request=%s ignored=%#v", body, ignored)
	}

	anthropicToResponses := config.ConversionCapability{
		Level: 2, Text: true, Streaming: true, Reasoning: true,
		ReasoningAdapter: config.ReasoningAdapterAnthropicToResponsesEffort, ReasoningTargetEffort: "low",
	}
	body, ignored, err = buildResponsesFromAnthropicWithCapability(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"thinking": map[string]any{"type": "adaptive"}, "output_config": map[string]any{"effort": "high"},
	}, "gpt-test", false, anthropicToResponses)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"reasoning":{"effort":"low"}`) || !strings.Contains(strings.Join(ignored, ","), "thinking") {
		t.Fatalf("request=%s ignored=%#v", body, ignored)
	}
}

func TestResponsesReasoningNoneDisablesAnthropicThinking(t *testing.T) {
	capability := config.ConversionCapability{
		Level: 3, Text: true, Tools: true, Reasoning: true,
		ReasoningAdapter: config.ReasoningAdapterResponsesToAnthropicAdaptive, ReasoningTargetEffort: "medium",
	}
	body, ignored, err := buildAnthropicFromResponsesWithCapability(map[string]any{
		"input": "Use lookup.", "reasoning": map[string]any{"effort": "none"},
		"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
	}, "claude-test", false, capability)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	thinking, ok := request["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("request=%s", body)
	}
	if _, ok := request["output_config"]; ok {
		t.Fatalf("disabled thinking must not include output_config: %s", body)
	}
	if !strings.Contains(strings.Join(ignored, ","), "reasoning") {
		t.Fatalf("ignored=%#v", ignored)
	}
}

func TestReasoningOutputIsOmittedAndMarkedDegraded(t *testing.T) {
	capability := config.ConversionCapability{Level: 2, Text: true, Reasoning: true, ReasoningAdapter: config.ReasoningAdapterAnthropicToResponsesEffort, ReasoningTargetEffort: "low"}
	response, _, ignored, err := convertAnthropicToResponsesWithCapability([]byte(`{"id":"m1","model":"claude","content":[{"type":"thinking","thinking":"secret","signature":"sig"},{"type":"text","text":"visible"}]}`), "claude", capability)
	if err != nil || strings.Contains(string(response), "secret") || !strings.Contains(string(response), "visible") || !strings.Contains(strings.Join(ignored, ","), "thinking_output") {
		t.Fatalf("response=%s ignored=%#v err=%v", response, ignored, err)
	}

	capability.ReasoningAdapter = config.ReasoningAdapterResponsesToAnthropicAdaptive
	response, _, ignored, err = convertOpenAIResponsesToAnthropicWithCapability([]byte(`{"id":"r1","model":"gpt","output":[{"type":"reasoning","summary":[{"text":"secret"}]},{"type":"message","content":[{"type":"output_text","text":"visible"}]}]}`), "gpt", capability)
	if err != nil || strings.Contains(string(response), "secret") || !strings.Contains(string(response), "visible") || !strings.Contains(strings.Join(ignored, ","), "reasoning_output") {
		t.Fatalf("response=%s ignored=%#v err=%v", response, ignored, err)
	}
}

func TestResponsesProviderMetadataIsOmitted(t *testing.T) {
	capability := config.ConversionCapability{Level: 3, Text: true, Tools: true}
	response, _, ignored, err := convertOpenAIResponsesToAnthropicWithCapability([]byte(`{
		"id":"r1","model":"gpt","status":"completed",
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"visible"}],"internal_chat_message_metadata_passthrough":{"opaque":"secret"},"metadata":{"provider":"private"}}],
		"usage":{"input_tokens":1,"output_tokens":1}
	}`), "gpt", capability)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(response), "secret") || !strings.Contains(string(response), "visible") {
		t.Fatalf("response=%s", response)
	}
	if !strings.Contains(strings.Join(ignored, ","), "internal_chat_message_metadata_passthrough") {
		t.Fatalf("ignored=%#v", ignored)
	}
	if !strings.Contains(strings.Join(ignored, ","), "output_metadata") {
		t.Fatalf("ignored=%#v", ignored)
	}
}

func TestReasoningSSEBlocksAreOmitted(t *testing.T) {
	capability := config.ConversionCapability{Level: 2, Text: true, Reasoning: true, ReasoningAdapter: config.ReasoningAdapterAnthropicToResponsesEffort, ReasoningTargetEffort: "low"}
	state := &textConversionStreamState{}
	mapper := anthropicEventToResponsesWithCapability(capability)
	if _, err := mapper([]byte(`{"type":"message_start","message":{"id":"m1","model":"claude"}}`), state); err != nil {
		t.Fatal(err)
	}
	if events, err := mapper([]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"secret"}}`), state); err != nil || len(events) != 0 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	if events, err := mapper([]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"secret"}}`), state); err != nil || len(events) != 0 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	if _, err := mapper([]byte(`{"type":"content_block_stop","index":0}`), state); err != nil {
		t.Fatal(err)
	}
	if _, err := mapper([]byte(`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`), state); err != nil {
		t.Fatal(err)
	}
	if _, err := mapper([]byte(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ok"}}`), state); err != nil {
		t.Fatal(err)
	}
	if _, err := mapper([]byte(`{"type":"content_block_stop","index":1}`), state); err != nil {
		t.Fatal(err)
	}
	if _, err := mapper([]byte(`{"type":"message_stop"}`), state); err != nil || !state.Completed || !strings.Contains(strings.Join(state.IgnoredFeatures, ","), "thinking_output") {
		t.Fatalf("state=%#v err=%v", state, err)
	}
}

func TestResponsesAnthropicBoundsToolSchemaAndArguments(t *testing.T) {
	deep := map[string]any{}
	current := deep
	for i := 0; i < maxConversionSchemaDepth+2; i++ {
		next := map[string]any{}
		current["next"] = next
		current = next
	}
	if _, err := responsesToolsToAnthropic([]any{map[string]any{"type": "function", "name": "deep", "parameters": deep}}); err == nil {
		t.Fatal("expected deeply nested tool schema rejection")
	}
	args := strings.Repeat("x", maxConversionToolArgumentBytes+1)
	if _, err := responsesToolItemToAnthropic(map[string]any{"type": "function_call", "call_id": "c", "name": "lookup", "arguments": args}); err == nil {
		t.Fatal("expected oversized tool arguments rejection")
	}
	tooMany := make([]any, maxConversionTools+1)
	for i := range tooMany {
		tooMany[i] = map[string]any{"type": "function", "name": "lookup"}
	}
	if _, err := responsesToolsToAnthropic(tooMany); err == nil {
		t.Fatal("expected too many tool definitions rejection")
	}
}

func TestConvertedNonStreamRejectsSSEImmediately(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Handler, http.ResponseWriter, *http.Request, *http.Response)
	}{
		{name: "responses_to_anthropic", call: func(h *Handler, w http.ResponseWriter, r *http.Request, resp *http.Response) {
			h.handleResponsesToAnthropic(w, r, resp, nil, time.Now(), "anthropic", "claude", config.ConversionCapability{})
		}},
		{name: "anthropic_to_responses", call: func(h *Handler, w http.ResponseWriter, r *http.Request, resp *http.Response) {
			h.handleAnthropicToResponses(w, r, resp, nil, time.Now(), "openai", "gpt", config.ConversionCapability{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			rec := httptest.NewRecorder()
			resp := testResponse(http.StatusOK, "text/event-stream", "data: {\"type\":\"partial\"}\n\n")
			started := time.Now()
			tc.call(h, rec, req, resp)
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("SSE rejection took too long: %s", elapsed)
			}
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestFunctionToolCallResultConversion(t *testing.T) {
	use, err := responsesToolItemToAnthropic(map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": "{\"q\":\"x\"}"})
	if err != nil || use["type"] != "tool_use" || use["id"] != "call_1" {
		t.Fatalf("use=%#v err=%v", use, err)
	}
	back, err := anthropicToolBlockToResponses(use)
	if err != nil || back["type"] != "function_call" || back["call_id"] != "call_1" {
		t.Fatalf("back=%#v err=%v", back, err)
	}
	result, err := responsesToolItemToAnthropic(map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"})
	if err != nil || result["type"] != "tool_result" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestResponsesAnthropicFunctionToolsRequestAndResponse(t *testing.T) {
	body, err := buildAnthropicFromResponses(map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "look this up"},
		},
		"tools":       []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
		"tool_choice": "auto",
	}, "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if len(request["tools"].([]any)) != 1 || request["tool_choice"].(map[string]any)["type"] != "auto" {
		t.Fatalf("request=%s", body)
	}
	response, _, err := convertOpenAIResponsesToAnthropic([]byte(`{"id":"r1","model":"claude-test","output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}],"usage":{"input_tokens":1,"output_tokens":2}}`), "claude-test")
	if err != nil || !strings.Contains(string(response), `"type":"tool_use"`) {
		t.Fatalf("response=%s err=%v", response, err)
	}

	back, err := buildResponsesFromAnthropic(map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call_2", "name": "lookup", "input": map[string]any{"q": "x"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_2", "content": "ok"}}},
		},
		"tools": []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}},
	}, "gpt-test", false)
	if err != nil || !strings.Contains(string(back), `"function_call"`) || !strings.Contains(string(back), `"function_call_output"`) {
		t.Fatalf("back=%s err=%v", back, err)
	}
}

func TestServeConvertedSSEDelaysHeadersAndArchivesOutput(t *testing.T) {
	state := &textConversionStreamState{}
	rr := httptest.NewRecorder()
	err := serveConvertedSSEWithTimeouts(context.Background(), rr, strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"gpt\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"), responsesEventToAnthropic, state, true, time.Second, time.Second)
	if err != nil || rr.Code != 200 || !state.HeadersWritten || !strings.Contains(rr.Body.String(), "message_stop") {
		t.Fatalf("status=%d body=%s state=%#v err=%v", rr.Code, rr.Body.String(), state, err)
	}
}

func TestToolCallRegistry(t *testing.T) {
	r := newToolCallRegistry()
	if err := r.Add("call_1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Add("call_1"); err == nil {
		t.Fatal("expected duplicate rejection")
	}
	if err := r.Resolve("unknown"); err == nil {
		t.Fatal("expected unknown result rejection")
	}
	if err := r.Resolve("call_1"); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureResolved(); err != nil {
		t.Fatal(err)
	}
	if err := r.Resolve("call_1"); err == nil {
		t.Fatal("expected duplicate result rejection")
	}
	if err := r.Add("call_2"); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureResolved(); err == nil {
		t.Fatal("expected unresolved tool call rejection")
	}
}

func TestConvertedSSECommentsDoNotExtendFirstEventTimeout(t *testing.T) {
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer writer.Close()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if _, err := io.WriteString(writer, ": keepalive\n\n"); err != nil {
				return
			}
		}
	}()
	started := time.Now()
	err := serveConvertedSSEWithTimeouts(context.Background(), httptest.NewRecorder(), reader, responsesEventToAnthropic, &textConversionStreamState{}, true, 25*time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "first/next event timeout") {
		t.Fatalf("err=%v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("first-event timeout was extended by comments: %s", elapsed)
	}
	<-done
}

func TestConvertedSSEFirstEventFailureSettlesConversionMetricsOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}))
	defer upstream.Close()

	interactionRecorder, err := archive.NewRecorder(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry := metrics.NewRegistry()
	cfg := mustHandlerConfig(config.Config{
		Providers: map[string]config.Provider{
			"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: upstream.URL, APIKey: "test", Models: []string{"claude-test"}, Endpoints: []string{config.ProviderEndpointMessages}},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"claude-test": {ID: "claude-test", ContextWindowTokens: 128000, MaxOutputTokens: 32, ConversionCapabilities: map[string]config.ConversionCapability{
				config.ProviderEndpointMessages: {Level: 2, Text: true, Streaming: true},
			}},
		},
	})
	cfg.StreamFirstEventTimeout = 25 * time.Millisecond
	h := NewHandler(cfg, usage.NewMemoryStore(), interactionRecorder, registry)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"claude-test","stream":true,"input":"hello"}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var prometheus strings.Builder
	if err := registry.WritePrometheus(&prometheus); err != nil {
		t.Fatal(err)
	}
	metric := `aetherrelay_conversion_requests_total{provider="anthropic",model="claude-test",client_protocol="openai",upstream_protocol="anthropic",conversion_mode="responses_to_anthropic",conversion_level="2",upstream_status="200",degraded="false",estimated="false"} 1`
	if count := strings.Count(prometheus.String(), metric); count != 1 {
		t.Fatalf("conversion metric count=%d, want 1:\n%s", count, prometheus.String())
	}
}

func TestConversionStreamFailureClassification(t *testing.T) {
	tests := []struct {
		err  error
		kind streamKind
	}{
		{err: context.Canceled, kind: streamKindClientCanceled},
		{err: fmt.Errorf("upstream SSE idle timeout after 1s"), kind: streamKindIdleTimeout},
		{err: fmt.Errorf("conversion SSE exceeds 10 bytes"), kind: streamKindLimitExceeded},
		{err: fmt.Errorf("conversion SSE ended without terminal event"), kind: streamKindUpstreamTrunc},
		{err: fmt.Errorf("malformed event"), kind: streamKindProtocol},
	}
	for _, tc := range tests {
		if got := conversionStreamFailure(tc.err); got == nil || got.Kind != tc.kind {
			t.Fatalf("err=%v fail=%+v want=%s", tc.err, got, tc.kind)
		}
	}
}

func TestAnthropicSSEMultipleTextBlocksAndUnclosedBlock(t *testing.T) {
	state := &textConversionStreamState{}
	mapper := anthropicEventToResponses
	if _, err := mapper([]byte(`{"type":"message_start","message":{"id":"m1","model":"claude"}}`), state); err != nil {
		t.Fatal(err)
	}
	for index, text := range []string{"one", "two"} {
		if _, err := mapper([]byte(fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, index)), state); err != nil {
			t.Fatal(err)
		}
		if _, err := mapper([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, index, text)), state); err != nil {
			t.Fatal(err)
		}
		if _, err := mapper([]byte(fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)), state); err != nil {
			t.Fatal(err)
		}
	}
	if events, err := mapper([]byte(`{"type":"message_stop"}`), state); err != nil || len(events) != 1 || !state.Completed {
		t.Fatalf("events=%v state=%+v err=%v", events, state, err)
	}

	unclosed := &textConversionStreamState{}
	_, _ = mapper([]byte(`{"type":"message_start","message":{"id":"m2","model":"claude"}}`), unclosed)
	_, _ = mapper([]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`), unclosed)
	if _, err := mapper([]byte(`{"type":"message_stop"}`), unclosed); err == nil || !strings.Contains(err.Error(), "unclosed") {
		t.Fatalf("unclosed terminal err=%v", err)
	}
}

func TestLevel3StrictToolAndResponseBounds(t *testing.T) {
	if _, err := stringifyToolOutput(strings.Repeat("x", maxConversionToolArgumentBytes+1)); err == nil {
		t.Fatal("oversized string tool output must be rejected")
	}
	if _, err := responsesToolsToAnthropic([]any{map[string]any{"type": "function", "name": "lookup", "parameters": "not-a-schema"}}); err == nil {
		t.Fatal("invalid Responses parameters type must be rejected")
	}
	if _, err := anthropicToolsToResponses([]any{map[string]any{"name": "lookup", "input_schema": []any{}}}); err == nil {
		t.Fatal("invalid Anthropic input_schema type must be rejected")
	}
	if _, err := responsesToolChoiceToAnthropic(map[string]any{"type": "hosted", "name": "lookup"}); err == nil {
		t.Fatal("unknown Responses tool choice type must be rejected")
	}
	if _, err := anthropicToolChoiceToResponses(map[string]any{"type": "auto", "name": "lookup"}); err == nil {
		t.Fatal("unknown Anthropic object tool choice type must be rejected")
	}
	if _, _, err := convertOpenAIResponsesToAnthropic([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"answer","annotations":[{"type":"url_citation"}]}]}]}`), "gpt"); err == nil {
		t.Fatal("non-empty Responses annotations must be rejected")
	}
	if _, _, err := convertAnthropicToResponses([]byte(`{"content":[{"type":"text","text":"answer","citations":[{}]}]}`), "claude"); err == nil {
		t.Fatal("Anthropic citations must be rejected")
	}
}

func TestAnthropicObjectToolChoicesMapToResponses(t *testing.T) {
	for _, test := range []struct {
		input string
		want  any
	}{
		{input: `{"type":"auto"}`, want: "auto"},
		{input: `{"type":"any"}`, want: "required"},
		{input: `{"type":"none"}`, want: "none"},
	} {
		var input any
		if err := json.Unmarshal([]byte(test.input), &input); err != nil {
			t.Fatal(err)
		}
		got, err := anthropicToolChoiceToResponses(input)
		if err != nil || got != test.want {
			t.Fatalf("choice %s = %#v, %v; want %#v", test.input, got, err, test.want)
		}
	}
	if _, err := anthropicToolChoiceToResponses(map[string]any{"type": "auto", "name": "lookup"}); err == nil {
		t.Fatal("auto tool choice with a name must be rejected")
	}
}

func TestOmittedAnthropicThinkingDisablesSupportedTargetReasoning(t *testing.T) {
	metadata := config.ModelMetadata{ReasoningDeclared: true, ReasoningSupported: true, ReasoningEfforts: []string{"none", "low"}}
	encoded, err := disableResponsesReasoningForOmittedAnthropicThinking(map[string]any{}, []byte(`{"model":"deepseek-v4-flash"}`), metadata)
	if err != nil {
		t.Fatal(err)
	}
	var target map[string]any
	if err := json.Unmarshal(encoded, &target); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := target["reasoning"].(map[string]any)
	if reasoning["effort"] != "none" {
		t.Fatalf("reasoning = %#v", reasoning)
	}

	unchanged := []byte(`{"model":"deepseek-v4-flash"}`)
	encoded, err = disableResponsesReasoningForOmittedAnthropicThinking(map[string]any{"thinking": map[string]any{"type": "adaptive"}}, unchanged, metadata)
	if err != nil || string(encoded) != string(unchanged) {
		t.Fatalf("explicit thinking changed: %s, %v", encoded, err)
	}
}
