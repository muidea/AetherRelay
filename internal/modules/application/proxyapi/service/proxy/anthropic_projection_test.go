package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

func TestClaudeEmbeddedSessionAdmission(t *testing.T) {
	calls := 0
	h := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{complete: func(_ context.Context, request codexresponses.Request) (codexresponses.Result, error) {
		calls++
		if request.SessionHash == "" || strings.Contains(string(request.Body), "device-private") {
			t.Fatal("identity projection invalid")
		}
		return codexresponses.Result{Body: []byte(`{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)}, nil
	}})
	for _, header := range []string{"", "session-a", "conflicting"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.2-codex","messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"{\"device_id\":\"device-private\",\"account_uuid\":\"\",\"session_id\":\"session-a\"}"}}`))
		r.Header.Set("Authorization", "Bearer test-client-key")
		r.Header.Set("X-Claude-Code-Session-Id", header)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := http.StatusOK
		if header == "conflicting" {
			want = http.StatusBadRequest
		}
		if w.Code != want {
			t.Fatalf("header=%q status=%d body=%s", header, w.Code, w.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d", calls)
	}
}

func TestConvertedCodexRequestsExcludeSourceIdentity(t *testing.T) {
	for _, endpoint := range []string{"/v1/messages", "/v1/chat/completions"} {
		for _, stream := range []bool{false, true} {
			t.Run(endpoint+fmt.Sprint(stream), func(t *testing.T) {
				calls := 0
				check := func(request codexresponses.Request) {
					calls++
					if request.ClientUserAgent != "" || request.ClientOriginator != "" || request.BetaFeatures != "" {
						t.Fatal("source protocol identity escaped conversion")
					}
					var body map[string]any
					if err := json.Unmarshal(request.Body, &body); err != nil {
						t.Fatal(err)
					}
					for _, key := range []string{"messages", "metadata", "system", "max_tokens", "thinking", "output_config", "user"} {
						if _, exists := body[key]; exists {
							t.Fatalf("source field %s escaped conversion", key)
						}
					}
					if body["stream"] != true || body["store"] != false || body["input"] == nil || body["instructions"] == nil || body["model"] != "gpt-5.2-codex" || body["prompt_cache_key"] == nil {
						t.Fatal("Codex target contract missing")
					}
				}
				result := []byte(`{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
				h := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{
					complete: func(_ context.Context, r codexresponses.Request) (codexresponses.Result, error) {
						check(r)
						return codexresponses.Result{Body: result}, nil
					},
					stream: func(_ context.Context, r codexresponses.Request, start func(codexresponses.StreamStart) error, emit func([]byte) error) error {
						check(r)
						if err := start(codexresponses.StreamStart{}); err != nil {
							return err
						}
						for _, line := range []string{
							`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.2-codex"}}`,
							`data: {"type":"response.output_text.delta","delta":"ok"}`,
							`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":2,"output_tokens":1}}}`,
						} {
							if err := emit([]byte(line + "\n\n")); err != nil {
								return err
							}
						}
						return nil
					},
				})
				payload := fmt.Sprintf(`{"model":"gpt-5.2-codex","messages":[{"role":"user","content":"hello"}],"max_tokens":64,"stream":%t,"metadata":{"user_id":"opaque-source-id"}}`, stream)
				r := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(payload))
				r.Header.Set("Authorization", "Bearer test-client-key")
				r.Header.Set("User-Agent", "claude-cli/2.1.278")
				r.Header.Set("Originator", "claude-cli")
				r.Header.Set("Anthropic-Beta", "source-only-beta")
				r.Header.Set("X-Stainless-Runtime", "node")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if calls != 1 || w.Code != http.StatusOK {
					t.Fatalf("calls=%d status=%d body=%s", calls, w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestClaudeObservedRequestProjection(t *testing.T) {
	var body map[string]any
	err := json.Unmarshal([]byte(`{"metadata":{"user_id":"{\"device_id\":\"device\",\"account_uuid\":\"\",\"session_id\":\"session-a\"}"},"system":[{"type":"text","text":"instructions","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"5m"}}]}],"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"cache_control":{"type":"string"}}}}],"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"max_tokens":128000}`), &body)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(body)
	metadata := config.ModelMetadata{ReasoningDeclared: true, ReasoningSupported: true, ReasoningDefaultEffort: "medium", ReasoningEfforts: []string{"medium", "max"}}
	capability, err := anthropicTargetReasoning(body, metadata, config.ConversionCapability{Tools: true})
	if err != nil {
		t.Fatal(err)
	}
	encoded, ignored, err := buildResponsesFromAnthropicWithCapability(body, "target", true, capability)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if result["metadata"] != nil || result["instructions"] != "instructions" || result["reasoning"].(map[string]any)["effort"] != "max" {
		t.Fatalf("projection=%s", encoded)
	}
	if !strings.Contains(string(encoded), `"cache_control":{"type":"string"}`) {
		t.Fatal("tool schema was changed")
	}
	if !strings.Contains(strings.Join(ignored, ","), "cache_control") || !strings.Contains(strings.Join(ignored, ","), "metadata.user_id") {
		t.Fatalf("ignored=%v", ignored)
	}
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatal("source request mutated")
	}
	if anthropicEmbeddedSession(body) != "session-a" {
		t.Fatal("embedded session missing")
	}
	metadata.ReasoningEfforts = []string{"medium"}
	if _, err := anthropicTargetReasoning(body, metadata, capability); err == nil {
		t.Fatal("unsupported effort accepted")
	}
}

func TestAnthropicProjectionRejectsUnknownAnnotations(t *testing.T) {
	for _, input := range []string{
		`{"metadata":{"unknown":"value"}}`,
		`{"metadata":{"user_id":42}}`,
		`{"system":[{"type":"text","text":"hello","cache_control":{"type":"persistent"}}]}`,
		`{"system":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","extra":true}}]}`,
	} {
		var body map[string]any
		_ = json.Unmarshal([]byte(input), &body)
		if _, _, err := projectAnthropicAnnotations(body); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	if anthropicEmbeddedSession(map[string]any{"metadata": map[string]any{"user_id": "opaque-user"}}) != "" {
		t.Fatal("opaque user became session")
	}
}
