package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

// CP-EP-015: local preflight uses a stable tokenizer estimator.
func TestEstimateResponsesInputTokens(t *testing.T) {
	tests := []struct {
		name    string
		request responsesInputTokensRequest
		want    int
	}{
		{
			name:    "simple text input",
			request: responsesInputTokensRequest{Model: "gpt-5", Input: json.RawMessage(`[{"role":"user","content":"hello world"}]`)},
			want:    6,
		},
		{
			name: "instructions and tool",
			request: responsesInputTokensRequest{
				Model:        "gpt-5",
				Instructions: "You are helpful.",
				Input:        json.RawMessage(`[{"role":"user","content":"lookup weather in shanghai"}]`),
				Tools:        []json.RawMessage{json.RawMessage(`{"type":"function","name":"lookup_weather","description":"Look up current weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}`)},
			},
			want: 50,
		},
		{
			name: "content parts and tool output",
			request: responsesInputTokensRequest{
				Model: "gpt-4.1",
				Input: json.RawMessage(`[
					{"role":"user","content":[{"type":"input_text","text":"first line"},{"type":"input_text","text":"second line"}]},
					{"type":"function_call_output","call_id":"call_123","output":"{\"ok\":true}"}
				]`),
			},
			want: 24,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := estimateResponsesInputTokens(test.request)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("input tokens = %d, want %d", got, test.want)
			}
		})
	}
}

// CP-EP-015: preflight reuses Responses access without touching upstream.
func TestResponsesInputTokensAuthenticatesAndEstimatesLocally(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := config.Config{Providers: map[string]config.Provider{
		"openai": {
			Name: "openai", Protocol: "openai", BaseURL: upstream.URL, APIKey: "test",
			Models: []string{"gpt-test"}, Endpoints: []string{config.ProviderEndpointResponses},
		},
	}}
	handler := NewHandler(mustHandlerConfig(cfg), usage.NewMemoryStore(), nil, nil)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", strings.NewReader(`{"model":"gpt-test","input":"hello world"}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload responsesInputTokensResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Object != "response.input_tokens" || payload.InputTokens < 1 {
		t.Fatalf("response=%#v", payload)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("upstream calls=%d, want 0", got)
	}

	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", bytes.NewBufferString(`{"model":"gpt-test"}`)))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", unauthenticated.Code, unauthenticated.Body.String())
	}

	unknownRequest := httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", bytes.NewBufferString(`{"model":"unknown"}`))
	unknownRequest.Header.Set("Authorization", "Bearer test-client-key")
	unknown := httptest.NewRecorder()
	handler.ServeHTTP(unknown, unknownRequest)
	if unknown.Code != http.StatusBadRequest || !strings.Contains(unknown.Body.String(), ErrorCodeModelNotFound) {
		t.Fatalf("unknown model status=%d body=%s", unknown.Code, unknown.Body.String())
	}
}
