package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

// Structural reproduction of claude-owner/001203; no production text or IDs.
const claudeToolContinuation = `{"model":"gpt-5.2-codex","messages":[{"role":"user","content":"inspect"},{"role":"assistant","content":[{"type":"tool_use","id":"call_a","name":"read","input":{}},{"type":"tool_use","id":"call_b","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_a","content":"first","is_error":false},{"type":"tool_result","tool_use_id":"call_b","content":"second","is_error":false,"cache_control":{"type":"ephemeral"}}]}]}`

func TestClaudeToolResultContinuation(t *testing.T) {
	for _, failed := range []bool{false, true} {
		raw := claudeToolContinuation
		if failed {
			raw = strings.ReplaceAll(raw, `"is_error":false`, `"is_error":true`)
		}
		calls := 0
		h := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{complete: func(_ context.Context, req codexresponses.Request) (codexresponses.Result, error) {
			calls++
			var body map[string]any
			if err := json.Unmarshal(req.Body, &body); err != nil {
				t.Fatal(err)
			}
			items := body["input"].([]any)
			if len(items) != 5 {
				t.Fatalf("items=%v", items)
			}
			for i, id := range []string{"call_a", "call_b"} {
				item := items[3+i].(map[string]any)
				if item["type"] != "function_call_output" || item["call_id"] != id {
					t.Fatalf("item=%v", item)
				}
				if failed {
					var output struct {
						Error  bool
						Output string
					}
					if err := json.Unmarshal([]byte(item["output"].(string)), &output); err != nil || !output.Error || output.Output == "" {
						t.Fatalf("lost error result: %v", item)
					}
				}
			}
			for _, forbidden := range []string{`"is_error"`, `"tool_result"`, `"cache_control"`} {
				if strings.Contains(string(req.Body), forbidden) {
					t.Fatalf("source field leaked: %s", forbidden)
				}
			}
			return codexresponses.Result{Body: []byte(`{"id":"resp_test","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)}, nil
		}})
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(raw))
		r.Header.Set("Authorization", "Bearer test-client-key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || calls != 1 {
			t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
		}
	}
}

func TestToolResultTextAndValidation(t *testing.T) {
	block := map[string]any{"type": "tool_result", "tool_use_id": "call_a", "content": []any{map[string]any{"type": "text", "text": "one"}, map[string]any{"type": "text", "text": "two"}}}
	result, err := anthropicToolBlockToResponses(block)
	if err != nil || result["output"] != "one\ntwo" {
		t.Fatalf("result=%v err=%v", result, err)
	}
	block["content"] = []any{map[string]any{"type": "image", "source": "private"}}
	if _, err := anthropicToolBlockToResponses(block); err == nil {
		t.Fatal("image silently serialized")
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(strings.ReplaceAll(claudeToolContinuation, `"is_error":false`, `"is_error":"private-value"`)), &body)
	_, _, err = buildResponsesFromAnthropicWithCapability(body, "test", false, config.ConversionCapability{Tools: true})
	if err == nil {
		t.Fatal("invalid error flag accepted")
	}
	apiErr := conversionAPIError(TransportPlan{}, err)
	if apiErr.Feature != "tool_result.is_error" || apiErr.Param != "messages[2].content[0].is_error" || strings.Contains(apiErr.Message, "private-value") {
		t.Fatalf("error=%+v", apiErr)
	}
}

func TestResponsesKeepaliveIsNotBusinessOutput(t *testing.T) {
	state := &textConversionStreamState{}
	for _, started := range []bool{false, true} {
		state.Started = started
		events, err := responsesEventToAnthropic([]byte(`{"type":"keepalive"}`), state)
		if err != nil || len(events) != 0 || state.Completed || state.OutputEvidence || state.Started != started {
			t.Fatalf("events=%v state=%+v err=%v", events, state, err)
		}
	}
	if _, err := responsesEventToAnthropic([]byte(`{"type":"unknown_event"}`), state); err == nil {
		t.Fatal("unknown event accepted")
	}
}

func TestCodexConversionStreamErrorIsNotClientWrite(t *testing.T) {
	h := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{stream: func(_ context.Context, _ codexresponses.Request, start func(codexresponses.StreamStart) error, emit func([]byte) error) error {
		if err := start(codexresponses.StreamStart{}); err != nil {
			return err
		}
		if err := emit([]byte("data: {\"type\":\"keepalive\"}\n")); err != nil {
			t.Fatal(err)
		}
		err := emit([]byte("data: {\"type\":\"unknown_event\"}\n"))
		failure, ok := codexresponses.AsFailure(err)
		if !ok || failure.Kind != codexresponses.KindConversion {
			t.Fatalf("wrong failure: %v", err)
		}
		return err
	}})
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.2-codex","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	r.Header.Set("Authorization", "Bearer test-client-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
