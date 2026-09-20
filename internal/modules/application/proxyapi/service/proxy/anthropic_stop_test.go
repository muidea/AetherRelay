package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	archive "aetherrelay/internal/pkg/aetherrelayarchive"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

func TestLocalStopMatcherChunkIndependent(t *testing.T) {
	for _, tc := range []struct {
		text, want, match string
		stops             []string
	}{
		{"hello<END>discard", "hello", "<END>", []string{"<END>"}},
		{"hello<EN", "hello<EN", "", []string{"<END>"}},
		{"你好停止后续", "你好", "停止", []string{"停止"}},
		{"ababa", "", "aba", []string{"aba", "bab"}},
		{"abcd", "a", "b", []string{"abcd", "b"}},
		{"abcd", "", "abc", []string{"abc", "bc"}},
		{"plain", "plain", "", []string{"STOP"}},
		{"STOPtail", "", "STOP", []string{"STOP"}},
	} {
		for split := 0; split <= len(tc.text); split++ {
			if !utf8.ValidString(tc.text[:split]) {
				continue
			}
			m := localStopMatcher{stops: tc.stops}
			got := m.push(tc.text[:split]) + m.push(tc.text[split:]) + m.flush()
			if got != tc.want || m.matched != tc.match {
				t.Fatalf("text=%q split=%d got=%q match=%q", tc.text, split, got, m.matched)
			}
		}
	}
}

func TestAnthropicStopsValidationAndProjection(t *testing.T) {
	for _, invalid := range []any{nil, "STOP", []any{42}, []any{""}, []any{strings.Repeat("x", maxLocalStopBytes+1)}, make([]any, maxLocalStopSequences+1)} {
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}, "stop_sequences": invalid}
		_, _, err := buildResponsesFromAnthropicWithCapability(body, "gpt", false, config.ConversionCapability{})
		if err == nil {
			t.Fatalf("accepted invalid stop: %#v", invalid)
		}
		apiErr := conversionAPIError(TransportPlan{}, err)
		if apiErr.Code != ErrorCodeConversionUnsupported || !strings.HasPrefix(apiErr.Param, "stop_sequences") {
			t.Fatalf("error=%+v", apiErr)
		}
	}
	for _, stops := range [][]any{{}, {"STOP", "停止"}} {
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}, "stop_sequences": stops}
		encoded, ignored, err := buildResponsesFromAnthropicWithCapability(body, "gpt", false, config.ConversionCapability{})
		if err != nil || strings.Contains(string(encoded), "stop_sequences") || (len(stops) > 0 && !strings.Contains(strings.Join(ignored, ","), "stop_sequences")) {
			t.Fatalf("encoded=%s ignored=%v err=%v", encoded, ignored, err)
		}
	}
}

func TestAnthropicStopsBufferedPreservesUsageAndToolArguments(t *testing.T) {
	body := []byte(`{"content":[{"type":"tool_use","input":{"text":"STOP","number":9007199254740993}},{"type":"text","text":"visibleSTOPhidden"},{"type":"tool_use","name":"must_not_run"}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"output_tokens":99}}`)
	converted, err := applyAnthropicStops(body, []string{"STOP"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"text":"visible"`, `"stop_reason":"stop_sequence"`, `"stop_sequence":"STOP"`, `"output_tokens":99`, `9007199254740993`, `"text":"STOP"`} {
		if !strings.Contains(string(converted), want) {
			t.Fatalf("missing %s: %s", want, converted)
		}
	}
	if strings.Contains(string(converted), "must_not_run") || strings.Contains(string(converted), "hidden") {
		t.Fatalf("leaked tail: %s", converted)
	}
}

func TestAnthropicStopsCodexEndToEnd(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			calls := 0
			checkRequest := func(req codexresponses.Request) {
				calls++
				if strings.Contains(string(req.Body), "stop_sequences") {
					t.Fatal("source field leaked upstream")
				}
			}
			executor := codexResponsesExecutorStub{
				complete: func(_ context.Context, req codexresponses.Request) (codexresponses.Result, error) {
					checkRequest(req)
					return codexresponses.Result{Body: []byte(`{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"visible<END>hidden"}]}],"usage":{"input_tokens":7,"output_tokens":99}}`)}, nil
				},
				stream: func(_ context.Context, req codexresponses.Request, start func(codexresponses.StreamStart) error, emit func([]byte) error) error {
					checkRequest(req)
					if err := start(codexresponses.StreamStart{}); err != nil {
						return err
					}
					for _, event := range []string{
						`{"type":"response.created","response":{"id":"r","model":"gpt-5.2-codex"}}`,
						`{"type":"response.output_text.delta","delta":"visible<EN"}`,
						`{"type":"response.output_text.delta","delta":"D>hidden"}`,
						`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"c","name":"must_not_run"}}`,
						`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"c","name":"must_not_run","arguments":"{}"}}`,
						`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":7,"output_tokens":99}}}`,
					} {
						if err := emit([]byte("data: " + event)); err != nil {
							return err
						}
					}
					return nil
				},
			}
			h, root := newArchiveFidelityHandler(t, false, executor)
			var err error
			h.interactionRecorder, err = archive.NewRecorderOptions(root, archive.RecorderOptions{MaxRounds: 10, ScopeByAPIKey: true, FullContent: true})
			if err != nil {
				t.Fatal(err)
			}
			raw := fmt.Sprintf(`{"model":"gpt-5.2-codex","stream":%t,"stop_sequences":["<END>"],"messages":[{"role":"user","content":"test"}]}`, stream)
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(raw))
			r.Header.Set("Authorization", "Bearer test-client-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
			for _, want := range []string{`"text":"visible"`, `"stop_reason":"stop_sequence"`, `"output_tokens":99`} {
				if !strings.Contains(w.Body.String(), want) {
					t.Fatalf("missing %s: %s", want, w.Body.String())
				}
			}
			if strings.Contains(w.Body.String(), "hidden") || strings.Contains(w.Body.String(), "must_not_run") {
				t.Fatalf("leaked tail: %s", w.Body.String())
			}
			events := usageEvents(t, h.usageStore)
			if len(events) != 1 || events[0].OutputTokens != 99 || events[0].Outcome != "success" || !events[0].ConversionDegraded {
				t.Fatalf("events=%+v", events)
			}
			name := "response.json"
			if stream {
				name = "response.sse"
			}
			archived, err := os.ReadFile(filepath.Join(root, "test-client", "000001", name))
			if err != nil || string(archived) != w.Body.String() {
				t.Fatalf("archive mismatch: %v", err)
			}
		})
	}
}

func TestAnthropicStopsResponsesHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			cfg := mustHandlerConfig(config.Config{
				Providers:     map[string]config.Provider{"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "test", Models: []string{"shared-model"}, Endpoints: []string{config.ProviderEndpointResponses}}},
				ModelMetadata: map[string]config.ModelMetadata{"shared-model": {ID: "shared-model", ContextWindowTokens: 128000, MaxOutputTokens: 4096, ConversionCapabilities: map[string]config.ConversionCapability{config.ProviderEndpointResponses: {Level: 2, Text: true, Streaming: true}}}},
			})
			h := NewHandler(cfg, usage.NewMemoryStore(), nil, nil)
			h.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), "stop_sequences") {
					t.Fatal("leaked source stop field")
				}
				if !stream {
					return testResponse(200, "application/json", `{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"visibleSTOPtail"}]}],"usage":{"input_tokens":7,"output_tokens":99}}`), nil
				}
				return testResponse(200, "text/event-stream", "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"visibleST\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OPtail\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":99}}}\n\n"), nil
			})
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":"shared-model","stream":%t,"stop_sequences":["STOP"],"messages":[{"role":"user","content":"hello"}]}`, stream)))
			r.Header.Set("Authorization", "Bearer test-client-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"stop_sequence":"STOP"`) || !strings.Contains(w.Body.String(), `"text":"visible"`) || strings.Contains(w.Body.String(), "tail") {
				t.Fatalf("response=%d %s", w.Code, w.Body.String())
			}
			events := usageEvents(t, h.usageStore)
			if len(events) != 1 || events[0].OutputTokens != 99 || !events[0].ConversionDegraded {
				t.Fatalf("events=%+v", events)
			}
		})
	}
}

func TestAnthropicStopStreamFlushAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name, text, terminal, want string
		fail                       bool
	}{
		{"partial prefix", "answerST", `{"type":"response.completed","response":{"status":"completed"}}`, "answerST", false},
		{"terminal fallback", "", `{"type":"response.completed","response":{"status":"completed"}}`, "", false},
		{"upstream fault after stop", "answerSTOPtail", `{"type":"response.failed","response":{"status":"failed"}}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &textConversionStreamState{}
			mapper := withAnthropicStopMapper(responsesEventToAnthropic, []string{"STOP"})
			if _, err := mapper([]byte(`{"type":"response.created","response":{}}`), state); err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": tc.text})
			events, err := mapper(payload, state)
			if err != nil {
				t.Fatal(err)
			}
			last, err := mapper([]byte(tc.terminal), state)
			if tc.fail {
				if err == nil || state.Completed {
					t.Fatal("upstream failure turned into success")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			for _, e := range append(events, last...) {
				if delta, ok := e["delta"].(map[string]any); ok && delta["type"] == "text_delta" {
					text.WriteString(delta["text"].(string))
				}
			}
			if text.String() != tc.want || !state.Completed {
				t.Fatalf("text=%q state=%+v", text.String(), state)
			}
		})
	}
}

func TestAnthropicStopStreamDoneFallbackAndPartBoundary(t *testing.T) {
	for _, chunks := range [][]string{
		{`{"type":"response.output_text.done","text":"visibleSTOPtail"}`},
		{`{"type":"response.output_text.delta","delta":"visibleST"}`, `{"type":"response.output_text.done","text":"visibleST"}`, `{"type":"response.output_text.delta","delta":"OPtail"}`},
	} {
		state := &textConversionStreamState{}
		mapper := withAnthropicStopMapper(responsesEventToAnthropic, []string{"STOP"})
		chunks = append([]string{`{"type":"response.created","response":{}}`}, chunks...)
		chunks = append(chunks, `{"type":"response.completed","response":{"status":"completed"}}`)
		var text, reason string
		for _, chunk := range chunks {
			events, err := mapper([]byte(chunk), state)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if delta, ok := event["delta"].(map[string]any); ok {
					if s, ok := delta["text"].(string); ok {
						text += s
					}
					if s, ok := delta["stop_reason"].(string); ok {
						reason = s
					}
				}
			}
		}
		if len(chunks) == 3 {
			if text != "visible" || reason != "stop_sequence" {
				t.Fatalf("fallback: %q %q", text, reason)
			}
		} else if text != "visibleSTOPtail" || reason != "end_turn" {
			t.Fatalf("cross-part false match: %q %q", text, reason)
		}
	}
}

func TestAnthropicStopStreamDoesNotMatchToolJSON(t *testing.T) {
	state := &textConversionStreamState{}
	mapper := withAnthropicStopMapper(responsesEventToAnthropicWithCapability(config.ConversionCapability{Tools: true}), []string{"STOP"})
	var output strings.Builder
	for _, payload := range []string{
		`{"type":"response.created","response":{}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c","name":"lookup"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":\"STOP\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"c","name":"lookup"}}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	} {
		events, err := mapper([]byte(payload), state)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := encodeConversionSSE(events, true)
		if err != nil {
			t.Fatal(err)
		}
		output.Write(encoded)
	}
	if !strings.Contains(output.String(), `"stop_reason":"tool_use"`) || !strings.Contains(output.String(), "STOP") || !strings.Contains(output.String(), `"type":"input_json_delta"`) {
		t.Fatalf("tool content lost: %s", output.String())
	}
}

func TestLocalRejectionRetainsStructuredErrorAndClientContext(t *testing.T) {
	for _, tc := range []struct{ model, extra, code, path string }{
		{"claude-sonnet-5", "", ErrorCodeModelNotFound, ""},
		{"gpt-5.2-codex", `,"stop_sequences":[42]`, ErrorCodeConversionUnsupported, "stop_sequences[0]"},
		{"gpt-5.2-codex", `,"service_tier":"unknown"`, ErrorCodeConversionUnsupported, "service_tier"},
	} {
		t.Run(tc.code+tc.path, func(t *testing.T) {
			h, root := newArchiveFidelityHandler(t, false, codexResponsesExecutorStub{complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
				t.Fatal("local rejection reached upstream")
				return codexresponses.Result{}, nil
			}})
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"`+tc.model+`","messages":[{"role":"user","content":"test"}]`+tc.extra+`}`))
			r.Header.Set("Authorization", "Bearer test-client-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code < 400 || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("response=%d %s", w.Code, w.Body.String())
			}
			events := usageEvents(t, h.usageStore)
			if len(events) != 1 || events[0].ErrorCode != tc.code || events[0].Operation != "messages" || events[0].ClientEndpoint != "/v1/messages" || events[0].ClientProtocol != "anthropic" || events[0].UpstreamStatus != 0 {
				t.Fatalf("events=%+v", events)
			}
			raw, err := os.ReadFile(filepath.Join(root, "test-client", "000001", "metadata.json"))
			if err != nil {
				t.Fatal(err)
			}
			var meta archive.Metadata
			if err := json.Unmarshal(raw, &meta); err != nil {
				t.Fatal(err)
			}
			if meta.ErrorCode != tc.code || meta.ConversionErrorPath != tc.path || meta.ClientEndpoint != "/v1/messages" || meta.Operation != "messages" {
				t.Fatalf("meta=%+v", meta)
			}
		})
	}
}
