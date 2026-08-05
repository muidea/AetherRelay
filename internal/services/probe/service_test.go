package probe

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-proxy/internal/pkg/aiproxyconfig"
)

func TestBuildProbeRequest(t *testing.T) {
	tests := []struct {
		endpoint string
		path     string
		stream   bool
	}{
		{config.ProviderEndpointChatCompletions, "/v1/chat/completions", true},
		{config.ProviderEndpointMessages, "/v1/messages", true},
		{config.ProviderEndpointResponses, "/v1/responses", true},
		{config.ProviderEndpointCompletions, "/v1/completions", true},
		{config.ProviderEndpointEmbeddings, "/v1/embeddings", false},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			path, body, err := buildProbeRequest(tt.endpoint, "DeepSeek-V4-Flash", tt.stream)
			if err != nil {
				t.Fatal(err)
			}
			if path != tt.path {
				t.Fatalf("path = %q, want %q", path, tt.path)
			}
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["model"] != "DeepSeek-V4-Flash" {
				t.Fatalf("model = %#v", payload["model"])
			}
		})
	}
	if _, _, err := buildProbeRequest(config.ProviderEndpointEmbeddings, "m", true); err == nil {
		t.Fatal("expected embeddings stream probe rejection")
	}
	if _, _, err := buildProbeRequest("unknown", "m", false); err == nil {
		t.Fatal("expected unknown endpoint rejection")
	}
}

func TestJoinURL(t *testing.T) {
	tests := map[string]string{
		"https://api.example.test|/v1/chat/completions":         "https://api.example.test/v1/chat/completions",
		"https://api.example.test/v1/|/v1/chat/completions":     "https://api.example.test/v1/chat/completions",
		"https://api.example.test/codex/v1|/v1/responses":       "https://api.example.test/codex/v1/responses",
		"https://api.example.test/codex/v1|v1/chat/completions": "https://api.example.test/codex/v1/chat/completions",
	}
	for input, want := range tests {
		parts := strings.SplitN(input, "|", 2)
		if got := joinURL(parts[0], parts[1]); got != want {
			t.Fatalf("joinURL(%q, %q) = %q, want %q", parts[0], parts[1], got, want)
		}
	}
}

func TestSanitizeSummary(t *testing.T) {
	for _, secret := range []string{
		`{"error":"Authorization: Bearer secret"}`,
		`{"api_key":"secret"}`,
		`{"message":"x-api-key invalid"}`,
		`token sk-secret`,
	} {
		if got := sanitizeSummary(secret); got != "upstream response (details redacted)" {
			t.Fatalf("secret was not redacted: %q", got)
		}
	}
	if got := sanitizeSummary("line one\nline two"); got != "line one line two" {
		t.Fatalf("summary = %q", got)
	}
}

func TestIsEndpointDriftResponse(t *testing.T) {
	for _, tt := range []struct {
		status  int
		summary string
		want    bool
	}{
		{http.StatusNotFound, "", true},
		{http.StatusInternalServerError, `{"error":{"message":"not implemented"}}`, true},
		{http.StatusBadRequest, "completions api is only available when using beta", true},
		{520, "cloudflare origin error", false},
		{http.StatusInternalServerError, "temporary upstream failure", false},
	} {
		if got := isEndpointDriftResponse(tt.status, tt.summary); got != tt.want {
			t.Fatalf("isEndpointDriftResponse(%d, %q) = %t, want %t", tt.status, tt.summary, got, tt.want)
		}
	}
}

func TestRunProbePopulatesContractFieldsAndOutput(t *testing.T) {
	client := &http.Client{Transport: probeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-key" {
			t.Fatalf("authorization = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-probe"}`)),
		}, nil
	})}

	provider := config.Provider{
		Name: "display-name", Protocol: "openai", BaseURL: "https://upstream.test", APIKey: "upstream-key",
	}
	result := runProbe(client, "route-owner", config.ProviderEndpointChatCompletions,
		"DeepSeek-V4-Flash", provider, "/v1/chat/completions", []byte(`{"model":"DeepSeek-V4-Flash"}`), time.Second, false)
	if !result.OK || result.Provider != "route-owner" || result.Protocol != "openai" ||
		result.Endpoint != config.ProviderEndpointChatCompletions || result.Model != "DeepSeek-V4-Flash" ||
		result.UpstreamPath != "/v1/chat/completions" || result.Status != http.StatusOK || result.Conclusion != "success" {
		t.Fatalf("result = %#v", result)
	}

	var out bytes.Buffer
	printResultTo(&out, result)
	for _, field := range []string{
		"provider=route-owner", "protocol=openai", "endpoint=chat_completions",
		"model=DeepSeek-V4-Flash", "path=/v1/chat/completions", "status=200", "conclusion=success",
	} {
		if !strings.Contains(out.String(), field) {
			t.Fatalf("output missing %q: %s", field, out.String())
		}
	}
}

func TestCheckUsesStableFirstCatalogModel(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		gotModel = request.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-probe"}`))
	}))
	defer upstream.Close()
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"route": {Protocol: "openai", BaseURL: upstream.URL, Models: []string{"z-model", "a-model"}, Endpoints: []string{config.ProviderEndpointChatCompletions}},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"z-model": {},
			"a-model": {},
		},
	}
	result, err := Check(t.Context(), cfg, "route")
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Model != "a-model" || gotModel != "a-model" {
		t.Fatalf("result = %#v, request model = %q", result, gotModel)
	}
}

type probeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f probeRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
