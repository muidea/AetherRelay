package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

const webSearchToolFixture = `{"type":"web_search","external_web_access":false,"search_context_size":"low","filters":{"allowed_domains":["example.com"]},"user_location":{"type":"approximate","country":"US","timezone":"America/New_York"},"extension":{"sequence":9007199254740993}}`
const webSearchItemFixture = `{"type":"web_search_call","id":"ws_search_1","status":"completed","action":{"type":"search","query":"test query","sources":[{"type":"url","url":"https://example.com","title":"Example"}]}}`
const webSearchMessageFixture = `{"type":"message","id":"msg_answer","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Example","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":7}]}]}`

func TestCodexWebSearchNormalization(t *testing.T) {
	for _, mode := range []string{"http", "lite", "ws", "ws-lite", "compact"} {
		for _, choice := range []string{`"auto"`, `"required"`, `"none"`, `{"type":"web_search"}`, `{"type":"allowed_tools","mode":"auto","tools":[{"type":"web_search"}]}`} {
			t.Run(mode+"/"+choice, func(t *testing.T) {
				raw := []byte(`{"model":"gpt-test","input":[` + webSearchItemFixture + `,` + webSearchMessageFixture + `,{"role":"user","content":"continue"}],"tools":[` + webSearchToolFixture + `,{"type":"namespace","name":"local","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}],"tool_choice":` + choice + `,"include":["web_search_call.action.sources"],"reasoning":{"effort":"low"}}`)
				headers := http.Header{}
				if strings.Contains(mode, "lite") {
					headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
				}
				var normalized []byte
				var err error
				if strings.HasPrefix(mode, "ws") {
					create := bytes.Replace(raw, []byte(`{"model"`), []byte(`{"type":"response.create","model"`), 1)
					normalized, _, _, err = normalizeCodexWebsocketCreate(create, "", headers, codexRequestFeatures{})
				} else {
					normalized, _, _, _, err = normalizeCodexHTTPRequest(raw, mode == "compact", headers)
				}
				if err != nil {
					t.Fatal(err)
				}
				var body map[string]any
				if err := decodeCodexJSON(normalized, &body); err != nil {
					t.Fatal(err)
				}
				var wantTool, wantItem, wantMessage any
				_ = decodeCodexJSON([]byte(webSearchToolFixture), &wantTool)
				_ = decodeCodexJSON([]byte(webSearchItemFixture), &wantItem)
				_ = decodeCodexJSON([]byte(webSearchMessageFixture), &wantMessage)
				tools := body["tools"].([]any)
				input := body["input"].([]any)
				if !reflect.DeepEqual(tools[0], wantTool) || !reflect.DeepEqual(input[0], wantItem) || !reflect.DeepEqual(input[1], wantMessage) {
					t.Fatalf("search changed: %s", normalized)
				}
				if !bytes.Contains(normalized, []byte("web_search_call.action.sources")) {
					t.Fatalf("sources include lost: %s", normalized)
				}
				if mode != "compact" {
					var expected any
					_ = decodeCodexJSON([]byte(choice), &expected)
					if !reflect.DeepEqual(body["tool_choice"], expected) {
						t.Fatalf("choice changed: %s", normalized)
					}
				}
				if strings.Contains(mode, "lite") && (len(tools) != 1 || len(input) != 4 || input[3].(map[string]any)["type"] != "additional_tools") {
					t.Fatalf("Lite moved hosted search: %s", normalized)
				}
			})
		}
	}
}

func TestCodexWebSearchRejectsInvalidDeclarations(t *testing.T) {
	for _, fragment := range []string{
		`"tools":[{"type":"web_search","external_web_access":"false"}]`,
		`"tools":[{"type":"web_search","external_web_access":null}]`,
		`"tools":[{"type":"web_search","search_context_size":{}}]`,
		`"tools":[{"type":"web_search","user_location":"US"}]`,
		`"tools":[{"type":"web_search","user_location":{"type":"precise"}}]`,
		`"tools":[{"type":"web_search","user_location":{"type":"approximate","city":42}}]`,
		`"tools":[{"type":"web_search","filters":[]}]`,
		`"tools":[{"type":"web_search","filters":{"allowed_domains":"example.com"}}]`,
		`"tools":[{"type":"web_search","filters":{"allowed_domains":[42]}}]`,
		`"tools":[{"type":"web_search","filters":{"allowed_domains":[""]}}]`,
		`"tools":[{"type":"namespace","name":"local","tools":[{"type":"web_search"}]}]`,
		`"tools":[{"type":"web_search_preview"}]`,
		`"tools":[{"type":"unknown_tool"}]`,
		`"tool_choice":{"type":"web_search"}`,
		`"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"web_search"}]}`,
	} {
		for _, lite := range []bool{false, true} {
			t.Run(fmt.Sprintf("%t/%s", lite, fragment), func(t *testing.T) {
				headers := http.Header{}
				if lite {
					headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
				}
				_, _, _, _, err := normalizeCodexHTTPRequest([]byte(`{"model":"gpt-test","input":[],`+fragment+`}`), false, headers)
				if err == nil {
					t.Fatal("invalid tool was accepted")
				}
			})
		}
	}
	_, _, _, err := normalizeCodexRequest([]byte(`{"model":"gpt-test","input":[{"type":"additional_tools","tools":[{"type":"web_search"}]}]}`), false)
	if err == nil {
		t.Fatal("accepted hosted search in additional_tools")
	}
}

func TestCodexWebSearchMinimalAndLiveOptions(t *testing.T) {
	for _, tool := range []string{
		`{"type":"web_search"}`,
		`{"type":"web_search","external_web_access":true}`,
		`{"type":"web_search","user_location":null,"filters":null}`,
		`{"type":"web_search","user_location":{"country":"US"}}`,
		`{"type":"web_search","user_location":{}}`,
		`{"type":"web_search","user_location":{"type":"approximate","city":null,"country":null,"region":null,"timezone":null}}`,
	} {
		normalized, body, _, err := normalizeCodexRequest([]byte(`{"model":"gpt-test","input":"hello","tools":[`+tool+`]}`), false)
		if err != nil {
			t.Fatal(err)
		}
		var want any
		_ = decodeCodexJSON([]byte(tool), &want)
		if !reflect.DeepEqual(body["tools"].([]any)[0], want) {
			t.Fatalf("tool changed: %s", normalized)
		}
	}
}

func TestCodexWebSearchHTTPForwardsAndSettles(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, lite := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/lite=%t", stream, lite), func(t *testing.T) {
				store := usage.NewMemoryStore()
				calls := 0
				check := func(request codexresponses.Request) {
					calls++
					if request.Model != "gpt-5.2-codex" || request.ResponsesLite != lite || !bytes.Contains(request.Body, []byte(`"external_web_access":false`)) {
						t.Fatalf("upstream request=%+v", request)
					}
				}
				terminal := `{"object":"response","id":"resp_search","status":"completed","output":[` + webSearchItemFixture + `,` + webSearchMessageFixture + `],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":60},"output_tokens":10}}`
				executor := codexResponsesExecutorStub{
					complete: func(_ context.Context, request codexresponses.Request) (codexresponses.Result, error) {
						check(request)
						return codexresponses.Result{Body: []byte(terminal)}, nil
					},
					stream: func(_ context.Context, request codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
						check(request)
						if err := started(codexresponses.StreamStart{}); err != nil {
							return err
						}
						if err := emit([]byte("data: {\"type\":\"response.web_search_call.searching\",\"item_id\":\"ws_search_1\"}\n\n")); err != nil {
							return err
						}
						return emit([]byte("data: {\"type\":\"response.completed\",\"response\":" + terminal + "}\n\n"))
					},
				}
				handler := newCodexResponsesHandler(t, store, executor)
				r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.2-codex","input":"hello","stream":%t,"tools":[%s]}`, stream, webSearchToolFixture)))
				r.Header.Set("Authorization", "Bearer test-client-key")
				if lite {
					r.Header.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != 200 || calls != 1 || !strings.Contains(w.Body.String(), "url_citation") || !strings.Contains(w.Body.String(), "web_search_call") {
					t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
				}
				events := usageEvents(t, store)
				if len(events) != 1 || events[0].Outcome != "success" || events[0].CachedInputTokens != 60 {
					t.Fatalf("usage=%+v", events)
				}
			})
		}
	}
}

func TestCodexWebSearchHistorySurvivesWebsocketReplay(t *testing.T) {
	state := newCodexWebsocketReplayState(1 << 20)
	first, err := state.prepare([]byte(`{"model":"gpt-test","input":[{"role":"user","content":"search"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	collector := codexWebsocketOutputCollector{}
	collector.addEvent([]byte(`{"type":"response.output_item.done","item":` + webSearchItemFixture + `}`))
	collector.addEvent([]byte(`{"type":"response.completed","response":{"output":[` + webSearchItemFixture + `,` + webSearchMessageFixture + `]}}`))
	state.commit(first, collector.result())
	payload := []byte(`{"type":"response.create","model":"gpt-test","previous_response_id":"resp_search","input":[{"role":"user","content":"continue"}],"tools":[` + webSearchToolFixture + `]}`)
	turn, err := state.prepare(payload)
	if err != nil {
		t.Fatal(err)
	}
	retry, safe, err := buildCodexWebsocketRetryPayload(payload, turn, 1<<20)
	if err != nil || !safe {
		t.Fatalf("safe=%t err=%v", safe, err)
	}
	var body struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(retry, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Input) != 4 || !bytes.Contains(body.Input[1], []byte("ws_search_1")) || !bytes.Contains(body.Input[2], []byte("url_citation")) || bytes.Contains(retry, []byte("function_call_output")) {
		t.Fatalf("replay=%s", retry)
	}
}

func TestCodexWebSearchHTTPPreservesUpstreamRejection(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			calls := 0
			reject := func(request codexresponses.Request) error {
				calls++
				if request.Model != "gpt-5.2-codex" || !bytes.Contains(request.Body, []byte(`"external_web_access":false`)) {
					t.Fatalf("request changed: %+v", request)
				}
				failure := codexresponses.NewFailure(codexresponses.KindInvalidRequest, 0, fmt.Errorf("unsupported tool"))
				failure.HTTPStatus = http.StatusBadRequest
				failure.UpstreamCode = "unsupported_tool"
				failure.UpstreamParam = "tools[0]"
				failure.UpstreamMessage = "web_search is unavailable"
				return failure
			}
			executor := codexResponsesExecutorStub{
				complete: func(_ context.Context, request codexresponses.Request) (codexresponses.Result, error) {
					return codexresponses.Result{}, reject(request)
				},
				stream: func(_ context.Context, request codexresponses.Request, _ func(codexresponses.StreamStart) error, _ func([]byte) error) error {
					return reject(request)
				},
			}
			handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), executor)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.2-codex","input":"hello","stream":%t,"tools":[%s]}`, stream, webSearchToolFixture)))
			request.Header.Set("Authorization", "Bearer test-client-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || calls != 1 || !strings.Contains(response.Body.String(), `"code":"unsupported_tool"`) || !strings.Contains(response.Body.String(), `"param":"tools[0]"`) {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
			}
		})
	}
}

func TestCodexWebSearchFailureAfterProgressIsNotSuccess(t *testing.T) {
	for _, kind := range []codexresponses.ErrorKind{codexresponses.KindClientCanceled, codexresponses.KindProtocol} {
		t.Run(string(kind), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			store := usage.NewMemoryStore()
			executor := codexResponsesExecutorStub{stream: func(ctx context.Context, _ codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
				calls++
				if err := started(codexresponses.StreamStart{}); err != nil {
					return err
				}
				if err := emit([]byte("data: {\"type\":\"response.web_search_call.searching\",\"item_id\":\"ws_search_1\"}\n\n")); err != nil {
					return err
				}
				cause := fmt.Errorf("stream closed before response.completed")
				if kind == codexresponses.KindClientCanceled {
					cancel()
					cause = ctx.Err()
				}
				return codexresponses.NewFailure(kind, 0, cause)
			}}
			handler := newCodexResponsesHandler(t, store, executor)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.2-codex","input":"search","stream":true,"tools":[`+webSearchToolFixture+`]}`)).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer test-client-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if calls != 1 || !strings.Contains(response.Body.String(), "response.web_search_call.searching") || strings.Contains(response.Body.String(), `"type":"response.completed"`) {
				t.Fatalf("calls=%d body=%s", calls, response.Body.String())
			}
			events := usageEvents(t, store)
			if len(events) != 1 || events[0].Outcome != string(kind) {
				t.Fatalf("usage=%+v", events)
			}
		})
	}
}
