package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptsearch"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	completion "aetherrelay/internal/pkg/aetherrelaycompletion"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
)

func serveWebResponse(t *testing.T, h *Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer test-client-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChatGPTWebResponsesRejectsSemanticLossBeforeEitherExecutor(t *testing.T) {
	for _, search := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				field   string
				value   any
				feature string
			}{
				{"previous_response_id", "resp_previous", "previous_response_id"}, {"background", true, "background"},
				{"text", map[string]any{"format": map[string]any{"type": "json_schema", "name": "answer", "schema": map[string]any{"type": "object"}}}, "text.format"},
				{"text", map[string]any{"verbosity": "low"}, "text.verbosity"}, {"reasoning", map[string]any{"summary": "detailed"}, "reasoning.summary"},
				{"max_output_tokens", 1, "max_output_tokens"}, {"temperature", 0.5, "temperature"}, {"top_p", 0.9, "top_p"},
				{"response_format", map[string]any{"type": "json_object"}, "response_format"}, {"future_semantics", true, "future_semantics"},
				{"store", true, "store"}, {"parallel_tool_calls", true, "parallel_tool_calls"}, {"truncation", "auto", "truncation"},
			} {
				t.Run(fmt.Sprintf("search=%v/stream=%v/%s", search, stream, tc.feature), func(t *testing.T) {
					calls := 0
					store := usage.NewMemoryStore()
					h := newChatGPTWebHandler(t, store, chatGPTTextExecutorStub{
						complete: func(context.Context, chatgpttext.Request) (chatgpttext.Result, error) {
							calls++
							return chatgpttext.Result{}, nil
						},
						stream: func(context.Context, chatgpttext.Request, func(chatgpttext.Delta) error) (chatgpttext.Result, error) {
							calls++
							return chatgpttext.Result{}, nil
						},
					}).WithChatGPTSearchExecutor(chatGPTSearchExecutorStub{search: func(context.Context, chatgptsearch.Request) (chatgptsearch.Result, error) {
						calls++
						return chatgptsearch.Result{}, nil
					}})
					body := map[string]any{"model": "gpt-5", "input": "hello", "stream": stream, tc.field: tc.value}
					if search {
						body["tools"] = []any{map[string]any{"type": "web_search"}}
					}
					rec := serveWebResponse(t, h, body)
					var result APIErrorResponse
					if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if rec.Code != 400 || calls != 0 || result.Error.Code != ErrorCodeConversionUnsupported || result.Error.Feature != tc.feature {
						t.Fatalf("status=%d calls=%d error=%s", rec.Code, calls, rec.Body.String())
					}
					events := usageEvents(t, store)
					if len(events) != 1 || events[0].Outcome == "success" || !reflect.DeepEqual(events[0].UnsupportedFeatures, []string{tc.feature}) {
						t.Fatalf("events=%+v", events)
					}
				})
			}
		}
	}
}

func TestChatGPTWebResponsesSearchRejectsDiscardedContextAndToolOptions(t *testing.T) {
	cases := []struct {
		body    map[string]any
		feature string
	}{
		{map[string]any{"instructions": "answer only in Chinese"}, "instructions"},
		{map[string]any{"reasoning": map[string]any{"effort": "high"}}, "reasoning.effort"},
		{map[string]any{"input": []any{map[string]any{"role": "user", "content": "first"}, map[string]any{"role": "user", "content": "second"}}}, "input.history"},
		{map[string]any{"tools": []any{map[string]any{"type": "web_search", "filters": map[string]any{"allowed_domains": []any{"example.test"}}}}}, "tools[0].filters"},
		{map[string]any{"tools": []any{map[string]any{"type": "web_search", "external_web_access": false}}}, "tools[0].external_web_access"},
		{map[string]any{"tool_choice": "auto"}, "tool_choice"},
		{map[string]any{"tool_choice": map[string]any{"type": "web_search_preview"}}, "tool_choice"},
		{map[string]any{"tool_choice": map[string]any{"type": []any{"web_search"}}}, "tool_choice"},
		{map[string]any{"input": strings.Repeat("x", chatgptsearch.MaxQueryBytes+1)}, "input"},
	}
	for _, tc := range cases {
		t.Run(tc.feature, func(t *testing.T) {
			calls := 0
			h := newChatGPTWebHandler(t, usage.NewMemoryStore(), chatGPTTextExecutorStub{}).WithChatGPTSearchExecutor(chatGPTSearchExecutorStub{search: func(context.Context, chatgptsearch.Request) (chatgptsearch.Result, error) {
				calls++
				return chatgptsearch.Result{}, nil
			}})
			body := map[string]any{"model": "gpt-5", "input": "hello", "tools": []any{map[string]any{"type": "web_search"}}}
			for key, value := range tc.body {
				body[key] = value
			}
			rec := serveWebResponse(t, h, body)
			var result APIErrorResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &result)
			if rec.Code != 400 || calls != 0 || result.Error.Feature != tc.feature {
				t.Fatalf("status=%d calls=%d error=%s", rec.Code, calls, rec.Body.String())
			}
		})
	}
}

func TestChatGPTWebResponsesRejectsMalformedMessageTypes(t *testing.T) {
	for _, search := range []bool{false, true} {
		for _, typ := range []any{false, 123, map[string]any{}, "", " message "} {
			body := map[string]any{"input": []any{map[string]any{"type": typ, "role": "user", "content": "hello"}}}
			if search {
				body["tools"] = []any{map[string]any{"type": "web_search"}}
			}
			if _, _, err := chatGPTResponsesRequest("gpt-5", body); err == nil || err.Feature != "input[0].type" {
				t.Fatalf("search=%v type=%v error=%v", search, typ, err)
			}
		}
	}
	// Whitespace must not bypass the content-part field allowlist.
	body := map[string]any{"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": " input_text ", "text": "hello", "future_semantics": true}}}}}
	if _, _, err := chatGPTResponsesRequest("gpt-5", body); err == nil {
		t.Fatal("accepted malformed content type")
	}
}

func TestChatGPTWebSearchFailureDoesNotInventTextFromSources(t *testing.T) {
	for _, stream := range []bool{false, true} {
		h := newChatGPTWebHandler(t, usage.NewMemoryStore(), chatGPTTextExecutorStub{}).WithChatGPTSearchExecutor(chatGPTSearchExecutorStub{search: func(context.Context, chatgptsearch.Request) (chatgptsearch.Result, error) {
			return chatgptsearch.Result{Sources: []chatgptsearch.Source{{Title: "Source", URL: "https://example.test"}}}, chatgptfail.New(chatgptfail.KindTimeout, errors.New("poll timeout"))
		}})
		rec := serveWebResponse(t, h, map[string]any{"model": "gpt-5", "input": "hello", "stream": stream, "tools": []any{map[string]any{"type": "web_search"}}})
		var payload map[string]any
		if stream {
			events := decodeWebEvents(t, rec.Body.String())
			payload = events[len(events)-1]["response"].(map[string]any)
		} else if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["status"] != "failed" || len(payload["output"].([]any)) != 1 || strings.Contains(rec.Body.String(), "Sources:") {
			t.Fatalf("stream=%v body=%s", stream, rec.Body.String())
		}
	}
}

func TestChatGPTWebResponsesAcceptsDefaultsAndPreservesMetadata(t *testing.T) {
	for _, search := range []bool{false, true} {
		h := newChatGPTWebHandler(t, usage.NewMemoryStore(), chatGPTTextExecutorStub{}).WithChatGPTSearchExecutor(chatGPTSearchExecutorStub{})
		body := map[string]any{"model": "gpt-5", "input": "hello", "instructions": nil, "background": false, "parallel_tool_calls": false, "store": false, "previous_response_id": nil, "include": []any{}, "truncation": "disabled", "tools": []any{}, "tool_choice": "none", "text": map[string]any{"format": map[string]any{"type": "text"}}, "metadata": map[string]any{"request": "123"}, "user": "audit-only"}
		if search {
			body["tools"] = []any{map[string]any{"type": "web_search"}}
			body["tool_choice"] = "required"
		}
		rec := serveWebResponse(t, h, body)
		var result responses.Response
		err := json.Unmarshal(rec.Body.Bytes(), &result)
		if rec.Code != 200 || err != nil || result.Status != "completed" || result.Metadata["request"] != "123" || result.OutputText() == "" || rec.Header().Get("X-AetherRelay-Usage-Estimated") != "true" {
			t.Fatalf("search=%v status=%d err=%v body=%s", search, rec.Code, err, rec.Body.String())
		}
	}
}

func decodeWebEvents(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "data: ") {
			var value map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &value); err != nil {
				t.Fatal(err)
			}
			events = append(events, value)
		}
	}
	return events
}

func TestChatGPTWebResponsesStreamHasCompleteLifecycle(t *testing.T) {
	for _, search := range []bool{false, true} {
		h := newChatGPTWebHandler(t, usage.NewMemoryStore(), chatGPTTextExecutorStub{}).WithChatGPTSearchExecutor(chatGPTSearchExecutorStub{})
		body := map[string]any{"model": "gpt-5", "input": "hello", "stream": true}
		if search {
			body["tools"] = []any{map[string]any{"type": "web_search"}}
		}
		rec := serveWebResponse(t, h, body)
		events := decodeWebEvents(t, rec.Body.String())
		var types []string
		added := map[string]bool{}
		content := map[string]bool{}
		var responseID any
		var created any
		for seq, event := range events {
			typ := event["type"].(string)
			types = append(types, typ)
			if event["sequence_number"] != float64(seq) {
				t.Fatalf("sequence=%v expected=%d", event, seq)
			}
			if payload, ok := event["response"].(map[string]any); ok {
				if responseID == nil {
					responseID = payload["id"]
					created = payload["created_at"]
				}
				if payload["id"] != responseID || payload["created_at"] != created {
					t.Fatalf("identity changed: %v", payload)
				}
			}
			if typ == "response.output_item.added" {
				item := event["item"].(map[string]any)
				added[item["id"].(string)] = true
			}
			if typ == "response.content_part.added" {
				id := event["item_id"].(string)
				if !added[id] {
					t.Fatal("content before item", event)
				}
				content[id] = true
			}
			if typ == "response.output_text.delta" {
				id := event["item_id"].(string)
				if !content[id] {
					t.Fatal("delta before content", event)
				}
			}
		}
		expected := []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"}
		if search {
			expected = []string{"response.created", "response.in_progress", "response.output_item.added", "response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed", "response.output_item.done", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.annotation.added", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"}
		}
		if !reflect.DeepEqual(types, expected) {
			t.Fatalf("search=%v types=%v", search, types)
		}
		final := events[len(events)-1]["response"].(map[string]any)
		output := final["output"].([]any)
		msg := output[len(output)-1].(map[string]any)
		if !added[msg["id"].(string)] {
			t.Fatal("final item ID differs", msg)
		}
	}
}

func TestChatGPTWebSearchTerminalEvidenceControlsStatusAndUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, state := range []string{"completed", "length", "unknown", "mismatch", "timeout"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, state), func(t *testing.T) {
				store := usage.NewMemoryStore()
				evidence := searchEvidence("partial answer")
				switch state {
				case "length":
					evidence.Completed = false
					evidence.Truncated = true
					evidence.Terminal = "max_tokens"
					evidence.Reason = completion.ModelLength
				case "unknown":
					evidence = completion.Search{}
				case "mismatch":
					evidence.TextSHA256 = completion.Hash("different")
				}
				h := newChatGPTWebHandler(t, store, chatGPTTextExecutorStub{}).WithChatGPTSearchExecutor(chatGPTSearchExecutorStub{search: func(context.Context, chatgptsearch.Request) (chatgptsearch.Result, error) {
					var err error
					if state == "timeout" {
						err = chatgptfail.New(chatgptfail.KindTimeout, errors.New("private upstream body"))
					}
					return chatgptsearch.Result{Completion: evidence, Text: "partial answer"}, err
				}})
				rec := serveWebResponse(t, h, map[string]any{"model": "gpt-5", "input": "hello", "stream": stream, "tools": []any{map[string]any{"type": "web_search"}}})
				expected := "failed"
				if state == "completed" {
					expected = "completed"
				}
				if state == "length" {
					expected = "incomplete"
				}
				var payload map[string]any
				if stream {
					events := decodeWebEvents(t, rec.Body.String())
					terminals := 0
					added := map[float64]any{}
					for index, event := range events {
						if event["sequence_number"] != float64(index) {
							t.Fatal("invalid event sequence", event)
						}
						switch event["type"] {
						case "response.completed", "response.incomplete", "response.failed":
							terminals++
						case "response.output_item.added":
							added[event["output_index"].(float64)] = event["item"].(map[string]any)["id"]
						}
						if terminals > 0 && index != len(events)-1 {
							t.Fatal("event after terminal", event)
						}
					}
					last := events[len(events)-1]
					if terminals != 1 || last["type"] != "response."+expected {
						t.Fatal(last)
					}
					payload = last["response"].(map[string]any)
					for index, item := range payload["output"].([]any) {
						if item.(map[string]any)["id"] != added[float64(index)] {
							t.Fatal("final output differs from announced items", item)
						}
					}
				} else {
					if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
						t.Fatal(err)
					}
				}
				if payload["status"] != expected || !strings.Contains(rec.Body.String(), "partial answer") || strings.Contains(rec.Body.String(), "private upstream body") {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				events := usageEvents(t, store)
				if len(events) != 1 || (events[0].Outcome == "success") != (state == "completed") {
					t.Fatalf("events=%+v", events)
				}
				if state == "length" && (events[0].ErrorCode != "text_model_length" || rec.Code != 200) {
					t.Fatalf("event=%+v status=%d", events[0], rec.Code)
				}
				if state == "length" && payload["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
					t.Fatal("missing model truncation reason", payload)
				}
				if !stream && expected == "failed" && rec.Code < 400 {
					t.Fatal(rec.Code)
				}
			})
		}
	}
}

func TestChatGPTWebOfficialSDKResponsesAndCitations(t *testing.T) {
	h := newChatGPTWebHandler(t, usage.NewMemoryStore(), chatGPTTextExecutorStub{}).WithChatGPTSearchExecutor(chatGPTSearchExecutorStub{})
	srv := httptest.NewServer(h)
	defer srv.Close()
	client := openai.NewClient(option.WithBaseURL(srv.URL+"/v1/"), option.WithAPIKey("test-client-key"), option.WithMaxRetries(0))
	params := responses.ResponseNewParams{Model: "gpt-5", Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("hello")}}
	for _, search := range []bool{false, true} {
		var opts []option.RequestOption
		if search {
			opts = append(opts, option.WithJSONSet("tools", []map[string]any{{"type": "web_search"}}))
		}
		result, err := client.Responses.New(context.Background(), params, opts...)
		if err != nil || result.OutputText() == "" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if search {
			a := result.Output[1].AsMessage().Content[0].AsOutputText().Annotations[0].AsURLCitation()
			if a.URL != "https://example.test" || a.Title != "Example" {
				t.Fatalf("annotation=%+v", a)
			}
		}
		stream := client.Responses.NewStreaming(context.Background(), params, opts...)
		var seen []string
		for stream.Next() {
			event := stream.Current()
			seen = append(seen, event.Type)
			if event.Type == "response.completed" && event.Response.OutputText() != result.OutputText() {
				t.Fatal("SDK final text mismatch")
			}
		}
		if err := stream.Err(); err != nil {
			t.Fatal(err)
		}
		_ = stream.Close()
		if len(seen) == 0 || seen[len(seen)-1] != "response.completed" {
			t.Fatal(seen)
		}
	}
}

func TestChatGPTWebSearchEndpointAndAdminRejectIncompleteAnswers(t *testing.T) {
	for _, state := range []string{"length", "missing", "mismatch"} {
		t.Run(state, func(t *testing.T) {
			evidence := searchEvidence("partial")
			switch state {
			case "length":
				evidence.Completed, evidence.Truncated = false, true
				evidence.Reason, evidence.Terminal = completion.ModelLength, "length"
			case "missing":
				evidence = completion.Search{}
			case "mismatch":
				evidence.TextSHA256 = completion.Hash("different")
			}
			store := usage.NewMemoryStore()
			calls := 0
			h := newChatGPTWebHandler(t, store, chatGPTTextExecutorStub{}).WithChatGPTSearchExecutor(chatGPTSearchExecutorStub{search: func(context.Context, chatgptsearch.Request) (chatgptsearch.Result, error) {
				calls++
				return chatgptsearch.Result{Completion: evidence, Text: "partial"}, nil
			}})
			req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"model":"gpt-5","query":"hello"}`))
			req.Header.Set("Authorization", "Bearer test-client-key")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code < 400 {
				t.Fatalf("search returned partial answer as success: %s", rec.Body.String())
			}
			if _, err := h.ExecuteFeatureSearch(context.Background(), proxyevents.ExecuteFeatureSearchCommand{OwnerID: "admin", Model: "gpt-5", Query: "hello"}); err == nil {
				t.Fatal("Admin consumer accepted incomplete answer")
			}
			if calls != 2 {
				t.Fatalf("consumers did not reach search executor: calls=%d", calls)
			}
			for _, event := range usageEvents(t, store) {
				if event.Outcome == "success" {
					t.Fatal("recorded incomplete search as success", event)
				}
			}
		})
	}
}
