package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

const clientToolSearchFixture = `{"type":"tool_search","execution":"client","description":"Find available tools","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false},"extension":{"sequence":9007199254740993}}`
const clientToolSearchHistory = `{"type":"tool_search_call","id":"tsc_search","call_id":"call_search","execution":"client","status":"completed","arguments":{"query":"read"}},{"type":"tool_search_output","id":"tso_search","call_id":"call_search","execution":"client","status":"completed","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}`

func TestCodexToolSearchIndependentOfLite(t *testing.T) {
	for _, mode := range []string{"http", "lite", "ws", "ws-lite", "compact"} {
		for _, tool := range []string{`{"type":"tool_search"}`, `{"type":"tool_search","execution":"server"}`, clientToolSearchFixture} {
			t.Run(mode+"/"+tool, func(t *testing.T) {
				raw := []byte(`{"model":"gpt-test","input":[` + clientToolSearchHistory + `,{"role":"user","content":"continue"}],"tools":[` + tool + `,` + webSearchToolFixture + `,{"type":"namespace","name":"local","tools":[{"type":"function","name":"read","defer_loading":true,"parameters":{"type":"object"}}]}]}`)
				headers := http.Header{}
				lite := strings.Contains(mode, "lite")
				if lite {
					headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
				}
				var normalized []byte
				var features codexRequestFeatures
				var err error
				if strings.HasPrefix(mode, "ws") {
					create := bytes.Replace(raw, []byte(`{"model"`), []byte(`{"type":"response.create","model"`), 1)
					normalized, _, features, err = normalizeCodexWebsocketCreate(create, "", headers, codexRequestFeatures{})
				} else {
					normalized, _, _, features, err = normalizeCodexHTTPRequest(raw, mode == "compact", headers)
				}
				if err != nil || features.ResponsesLite != lite {
					t.Fatalf("features=%+v err=%v", features, err)
				}
				var body, original map[string]any
				if err := decodeCodexJSON(normalized, &body); err != nil {
					t.Fatal(err)
				}
				_ = decodeCodexJSON(raw, &original)
				tools := body["tools"].([]any)
				input := body["input"].([]any)
				if body["model"] != "gpt-test" || !reflect.DeepEqual(tools[0], original["tools"].([]any)[0]) || !reflect.DeepEqual(input[:2], original["input"].([]any)[:2]) {
					t.Fatalf("tool/history changed: %s", normalized)
				}
				if lite && len(tools) != 2 || !lite && len(tools) != 3 {
					t.Fatalf("unexpected tool layout: %s", normalized)
				}
			})
		}
	}
}

func TestCodexToolSearchRejectsInvalidOptions(t *testing.T) {
	for _, options := range []string{`"execution":"local"`, `"execution":{}`, `"description":42`, `"parameters":[]`} {
		for _, lite := range []bool{false, true} {
			t.Run(fmt.Sprintf("%t/%s", lite, options), func(t *testing.T) {
				headers := http.Header{}
				if lite {
					headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
				}
				_, _, _, _, err := normalizeCodexHTTPRequest([]byte(`{"model":"gpt-test","input":"hello","tools":[{"type":"tool_search",`+options+`}]}`), false, headers)
				if err == nil {
					t.Fatal("invalid options accepted")
				}
			})
		}
	}
}

func TestCodexToolSearchHTTPAllowsQuestionWithoutSearch(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			calls := 0
			check := func(request codexresponses.Request) {
				calls++
				if request.ResponsesLite || request.Model != "gpt-5.2-codex" || !bytes.Contains(request.Body, []byte(`"execution":"client"`)) || !bytes.Contains(request.Body, []byte(`"type":"web_search"`)) {
					t.Fatalf("request changed: %+v", request)
				}
			}
			answer := `{"id":"resp_question","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":10,"output_tokens":1}}`
			executor := codexResponsesExecutorStub{
				complete: func(_ context.Context, request codexresponses.Request) (codexresponses.Result, error) {
					check(request)
					return codexresponses.Result{Body: []byte(answer)}, nil
				},
				stream: func(_ context.Context, request codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
					check(request)
					if err := started(codexresponses.StreamStart{}); err != nil {
						return err
					}
					return emit([]byte("data: {\"type\":\"response.completed\",\"response\":" + answer + "}\n\n"))
				},
			}
			store := usage.NewMemoryStore()
			handler := newCodexResponsesHandler(t, store, executor)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.2-codex","input":"hello","stream":%t,"tools":[%s,%s]}`, stream, clientToolSearchFixture, webSearchToolFixture)))
			request.Header.Set("Authorization", "Bearer test-client-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			events := usageEvents(t, store)
			if response.Code != http.StatusOK || calls != 1 || !strings.Contains(response.Body.String(), "Hello") || len(events) != 1 || events[0].Outcome != "success" {
				t.Fatalf("status=%d calls=%d usage=%+v body=%s", response.Code, calls, events, response.Body.String())
			}
		})
	}
}
