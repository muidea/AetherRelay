package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	archive "aetherrelay/internal/pkg/aetherrelayarchive"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	metrics "aetherrelay/internal/pkg/aetherrelaymetrics"
	"aetherrelay/internal/pkg/aetherrelaymetricsport"
)

var projectionCapability = config.ConversionCapability{Text: true, Tools: true, Reasoning: true, Streaming: true}

func projectionMessage(phase, text string) map[string]any {
	item := map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}
	if phase != "" {
		item["phase"] = phase
	}
	return item
}

func projectionResponse(output ...any) map[string]any {
	return map[string]any{"id": "resp_projection", "model": "gpt-5.2-codex", "status": "completed", "output": output,
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 12, "input_tokens_details": map[string]any{"cached_tokens": 64, "cache_write_tokens": 8}}}
}

func projectionJSON(value any) []byte { raw, _ := json.Marshal(value); return raw }

func TestCodexProjectionAcceptsCommentaryWithoutBroadeningGeneric(t *testing.T) {
	for _, phase := range []string{"", "commentary", "final_answer"} {
		t.Run(phase, func(t *testing.T) {
			raw := projectionJSON(projectionResponse(projectionMessage(phase, "visible")))
			got, usage, ignored, err := convertCodexResponsesToAnthropic(raw, "fallback", projectionCapability)
			if err != nil || !bytes.Contains(got, []byte(`"text":"visible"`)) || usage.PromptTokens != 100 || usage.CachedInputTokens != 64 {
				t.Fatalf("body=%s usage=%+v err=%v", got, usage, err)
			}
			if slices.Contains(ignored, "message_phase") != (phase == "commentary") {
				t.Fatalf("ignored=%v", ignored)
			}
			_, _, _, err = convertOpenAIResponsesToAnthropicWithCapability(raw, "fallback", projectionCapability)
			if (err != nil) != (phase == "commentary") {
				t.Fatalf("generic boundary changed: %v", err)
			}
		})
	}
	for _, item := range []any{projectionMessage("future_phase", "text"), map[string]any{"type": "function_call", "call_id": "call", "name": "now", "namespace": "clock", "arguments": `{}`}, map[string]any{"type": "custom_tool_call", "call_id": "call", "name": "shell", "input": "ls"}} {
		_, usage, _, err := convertCodexResponsesToAnthropic(projectionJSON(projectionResponse(item)), "fallback", projectionCapability)
		var detail *responseProjectionError
		if !errors.As(err, &detail) || detail.Path == "" || usage.PromptTokens != 100 || !usage.CachedInputTokensKnown || usage.CachedInputTokens != 64 {
			t.Fatalf("unsupported item=%v usage=%+v err=%v", item, usage, err)
		}
	}
}

func projectionFixture() (map[string]any, []map[string]any) {
	comment := projectionMessage("commentary", "before")
	final := projectionMessage("final_answer", "after")
	a := map[string]any{"type": "function_call", "call_id": "a", "name": "Read", "arguments": `{"number":9007199254740993}`}
	b := map[string]any{"type": "function_call", "call_id": "b", "name": "Read", "arguments": `{"path":"b"}`}
	for _, item := range []map[string]any{comment, a, b, final} {
		item["metadata"] = map[string]any{"turn_id": "fixture-turn", "future": map[string]any{"nested": []any{true, nil, "opaque marker"}}}
		item["internal_chat_message_metadata_passthrough"] = map[string]any{"turn_id": "fixture-turn"}
	}
	response := projectionResponse(comment, a, b, final)
	metadata := map[string]any{"type": "response.metadata", "sequence_number": 2, "response_id": "resp_projection", "metadata": map[string]any{"openai_chatgpt_moderation_metadata": map[string]any{"prompt": map[string]any{"omnimod": map[string]any{"protection_type": "omnimod", "outputs": []any{map[string]any{"is_blocked": false}}}}, "generation": map[string]any{}, "tool_call": map[string]any{}, "tool_response": map[string]any{}}}}
	return response, []map[string]any{
		metadata, // An observation is valid before response.created too.
		{"type": "response.created", "response": map[string]any{"id": "resp_projection", "model": "gpt-5.2-codex"}},
		{"type": "response.output_item.added", "output_index": 0, "item": comment},
		{"type": "response.output_text.delta", "output_index": 0, "delta": "be"},
		metadata,
		{"type": "response.output_text.delta", "output_index": 0, "delta": "fore"},
		{"type": "response.output_text.done", "output_index": 0, "text": "before"},
		{"type": "response.output_item.done", "output_index": 0, "item": comment},
		{"type": "response.output_item.added", "output_index": 1, "item": a},
		{"type": "response.output_item.added", "output_index": 2, "item": b},
		// Parallel call b finishes first, but stays after a in downstream output.
		{"type": "response.function_call_arguments.done", "output_index": 2, "arguments": b["arguments"]},
		{"type": "response.output_item.done", "output_index": 2, "item": b},
		{"type": "response.function_call_arguments.delta", "output_index": 1, "delta": `{"number":`},
		{"type": "response.function_call_arguments.delta", "output_index": 1, "delta": `9007199254740993}`},
		{"type": "response.function_call_arguments.done", "output_index": 1, "arguments": a["arguments"]},
		{"type": "response.output_item.done", "output_index": 1, "item": a},
		{"type": "response.output_item.added", "output_index": 3, "item": final},
		// Done-only text in a later message must still be emitted exactly once.
		{"type": "response.output_text.done", "output_index": 3, "text": "after"},
		{"type": "response.output_item.done", "output_index": 3, "item": final},
		metadata,
		{"type": "response.completed", "response": response},
	}
}

// Reconstruct client-visible content while enforcing Anthropic block lifecycle.
func projectionContent(t *testing.T, events []map[string]any) []any {
	t.Helper()
	var content []any
	open := -1
	arguments := ""
	for _, event := range events {
		index, _ := event["index"].(int)
		switch event["type"] {
		case "content_block_start":
			if open >= 0 || index != len(content) {
				t.Fatalf("invalid block start: %v", event)
			}
			block := event["content_block"].(map[string]any)
			content = append(content, block)
			open, arguments = index, ""
		case "content_block_delta":
			if open != index {
				t.Fatalf("delta outside block: %v", event)
			}
			delta := event["delta"].(map[string]any)
			block := content[index].(map[string]any)
			if delta["type"] == "text_delta" {
				block["text"] = block["text"].(string) + delta["text"].(string)
			} else {
				arguments += delta["partial_json"].(string)
			}
		case "content_block_stop":
			if open != index {
				t.Fatalf("stop outside block: %v", event)
			}
			if arguments != "" {
				var input any
				if err := decodeCodexJSON([]byte(arguments), &input); err != nil {
					t.Fatalf("duplicated or invalid arguments %q: %v", arguments, err)
				}
				content[index].(map[string]any)["input"] = input
			}
			open = -1
		}
	}
	if open >= 0 {
		t.Fatal("unclosed block")
	}
	return content
}

func TestCodexProjectionStreamMatchesCompletion(t *testing.T) {
	response, source := projectionFixture()
	for _, terminalOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(terminalOnly), func(t *testing.T) {
			fixture := source
			if terminalOnly {
				fixture = []map[string]any{source[1], source[len(source)-1]}
			}
			mapper, state := codexResponsesEventToAnthropic(projectionCapability), &textConversionStreamState{}
			var events []map[string]any
			for _, event := range fixture {
				out, err := mapper(projectionJSON(event), state)
				if err != nil {
					t.Fatalf("%v: %v", event["type"], err)
				}
				events = append(events, out...)
			}
			raw, usage, _, err := convertCodexResponsesToAnthropic(projectionJSON(response), "fallback", projectionCapability)
			if err != nil {
				t.Fatal(err)
			}
			var complete map[string]any
			_ = decodeCodexJSON(raw, &complete)
			if got := projectionContent(t, events); !reflect.DeepEqual(got, complete["content"]) {
				t.Fatalf("stream=%v complete=%v", got, complete["content"])
			}
			if state.tokenUsage() != usage || !state.Completed || events[len(events)-1]["type"] != "message_stop" {
				t.Fatalf("usage=%+v want=%+v state=%+v", state.tokenUsage(), usage, state)
			}
		})
	}
}

func TestCodexProjectionRejectsUnknownAndInconsistentEvents(t *testing.T) {
	for name, bad := range map[string]map[string]any{
		"unknown_event":    {"type": "response.future_business_event"},
		"unknown_phase":    {"type": "response.output_item.added", "item": projectionMessage("future", "x")},
		"namespace":        {"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "namespace": "clock", "name": "now", "call_id": "a"}},
		"partial_terminal": {"type": "response.completed", "response": projectionResponse(projectionMessage("final_answer", "missing text"))},
	} {
		t.Run(name, func(t *testing.T) {
			mapper, state := codexResponsesEventToAnthropic(projectionCapability), &textConversionStreamState{}
			_, _ = mapper([]byte(`{"type":"response.created","response":{}}`), state)
			_, _ = mapper([]byte(`{"type":"response.output_text.delta","delta":"already streamed"}`), state)
			out, err := mapper(projectionJSON(bad), state)
			var detail *responseProjectionError
			if !errors.As(err, &detail) || detail.EventType != bad["type"] || len(out) != 0 {
				t.Fatalf("out=%v err=%v", out, err)
			}
		})
	}
	mapper, state := codexResponsesEventToAnthropic(projectionCapability), &textConversionStreamState{}
	_, _ = mapper([]byte(`{"type":"response.created","response":{}}`), state)
	_, _ = mapper([]byte(`{"type":"response.completed","response":{"status":"completed"}}`), state)
	if _, err := mapper([]byte(`{"type":"response.metadata","metadata":{}}`), state); err == nil {
		t.Fatal("metadata after completion accepted")
	}
}

func TestCodexProjectionWebsocketMetadataIsObservational(t *testing.T) {
	_, fixture := projectionFixture()
	mapper, state := codexResponsesEventToAnthropic(projectionCapability), &textConversionStreamState{}
	metadata := []byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"opaque","x-models-etag":"etag"},"model":"gpt-6.1-sol"}`)
	for _, event := range fixture {
		// Metadata can precede response.created or arrive during generation.
		out, err := mapper(metadata, state)
		if err != nil || len(out) != 0 || state.Completed || state.tokenUsage() != (tokenUsage{}) {
			t.Fatalf("metadata changed response state: out=%v state=%+v err=%v", out, state, err)
		}
		out, err = mapper([]byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":25}}}`), state)
		if err != nil || len(out) != 0 || state.Completed || state.tokenUsage() != (tokenUsage{}) {
			t.Fatalf("rate limit observation changed response state: out=%v state=%+v err=%v", out, state, err)
		}
		if _, err := mapper(projectionJSON(event), state); err != nil {
			t.Fatalf("%v: %v", event["type"], err)
		}
	}
	if !state.Completed || state.tokenUsage().TotalTokens == 0 || !slices.Contains(state.IgnoredFeatures, "response_metadata") || !slices.Contains(state.IgnoredFeatures, "codex_rate_limits") {
		t.Fatalf("missing completion/usage/metadata observation: %+v", state)
	}
	if _, err := mapper(metadata, state); err == nil {
		t.Fatal("metadata after completion accepted")
	}
}

func TestCodexChatWebsocketObservationsPreserveOutputAndCompletion(t *testing.T) {
	state := &codexChatStreamState{}
	for _, typ := range []string{"response.metadata", "codex.response.metadata", "codex.rate_limits"} {
		out, err := codexResponsesEventToChat(projectionJSON(map[string]any{"type": typ, "headers": map[string]any{"x-codex-turn-state": "opaque"}}), state)
		if err != nil || len(out) != 0 || state.OutputEvidence || state.Started || state.Done || state.Usage != (tokenUsage{}) {
			t.Fatalf("%s changed response state: out=%q state=%+v err=%v", typ, out, state, err)
		}
	}
	for _, event := range []string{
		`{"type":"response.created","response":{"id":"resp_chat","model":"gpt-6.1-sol"}}`,
		`{"type":"response.output_text.delta","delta":"hello"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}`,
	} {
		out, err := codexResponsesEventToChat([]byte(event), state)
		if err != nil || len(out) == 0 {
			t.Fatalf("event=%s out=%q err=%v", event, out, err)
		}
	}
	if !state.Done || state.Usage.TotalTokens != 5 {
		t.Fatalf("state=%+v", state)
	}
	if _, err := codexResponsesEventToChat([]byte(`{"type":"response.future_business_event"}`), &codexChatStreamState{}); err == nil {
		t.Fatal("unknown business event accepted")
	}
}

func TestCodexProjectionStopsUseTextBlockAfterTools(t *testing.T) {
	_, fixture := projectionFixture()
	mapper := withAnthropicStopMapper(codexResponsesEventToAnthropic(projectionCapability), []string{"afterX"})
	state := &textConversionStreamState{}
	var events []map[string]any
	for _, event := range fixture {
		out, err := mapper(projectionJSON(event), state)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, out...)
	}
	content := projectionContent(t, events)
	if len(content) != 4 || content[3].(map[string]any)["text"] != "after" {
		t.Fatalf("stop prefix flushed to wrong block: %v", content)
	}
}

func TestCodexProjectionDelayedTextKeepsStopBoundaries(t *testing.T) {
	tool := map[string]any{"type": "function_call", "call_id": "call", "name": "Read", "arguments": `{}`}
	a, b := projectionMessage("commentary", "a"), projectionMessage("final_answer", "b")
	mapper := withAnthropicStopMapper(codexResponsesEventToAnthropic(projectionCapability), []string{"ab"})
	state := &textConversionStreamState{}
	source := []map[string]any{
		{"type": "response.created", "response": map[string]any{}},
		{"type": "response.output_item.added", "output_index": 0, "item": tool},
		{"type": "response.output_item.added", "output_index": 1, "item": a},
		{"type": "response.output_text.done", "text": "a"},
		{"type": "response.output_item.added", "output_index": 2, "item": b},
		{"type": "response.output_text.done", "text": "b"},
		{"type": "response.output_item.done", "output_index": 0, "item": tool},
		{"type": "response.completed", "response": projectionResponse(tool, a, b)},
	}
	var events []map[string]any
	for _, event := range source {
		out, err := mapper(projectionJSON(event), state)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, out...)
	}
	content := projectionContent(t, events)
	if len(content) != 3 || content[1].(map[string]any)["text"] != "a" || content[2].(map[string]any)["text"] != "b" {
		t.Fatalf("lost delayed text: %v", content)
	}
	for _, event := range events {
		if event["type"] == "message_delta" && event["delta"].(map[string]any)["stop_reason"] != "tool_use" {
			t.Fatalf("stop matched across parts: %v", event)
		}
	}
}

func TestCodexProjectionHandlerFailureUsageAndArchive(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, full := range []bool{false, true} {
			for _, usageKind := range []string{"known", "zero", "absent"} {
				t.Run(fmt.Sprintf("stream=%t/full=%t/usage=%s", stream, full, usageKind), func(t *testing.T) {
					response := projectionResponse(projectionMessage("future_phase", "private body marker"))
					if usageKind == "absent" {
						delete(response, "usage")
					}
					if usageKind == "zero" {
						response["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0, "input_tokens_details": map[string]any{"cached_tokens": 0, "cache_write_tokens": 0}}
					}
					executor := codexResponsesExecutorStub{
						complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
							return codexresponses.Result{Body: projectionJSON(response), Attempt: codexArchiveTestAttempt()}, nil
						},
						stream: func(_ context.Context, _ codexresponses.Request, start func(codexresponses.StreamStart) error, emit func([]byte) error) error {
							if err := start(codexresponses.StreamStart{Attempt: codexArchiveTestAttempt()}); err != nil {
								return err
							}
							if err := emit([]byte(`data: {"type":"response.created","response":{}}`)); err != nil {
								return err
							}
							return emit(append([]byte("data: "), projectionJSON(map[string]any{"type": "response.completed", "response": response})...))
						},
					}
					h, root := newArchivedCodexResponsesHandler(t, executor)
					recorder, err := archive.NewRecorderOptions(root, archive.RecorderOptions{MaxRounds: 10, ScopeByAPIKey: true, FullContent: full})
					if err != nil {
						t.Fatal(err)
					}
					h.interactionRecorder = recorder
					registry := metrics.NewRegistry()
					h.metricsRegistry = metricsport.AsPort(registry)
					var logs bytes.Buffer
					previous := log.Writer()
					log.SetOutput(&logs)
					defer log.SetOutput(previous)
					r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.2-codex","stream":%t,"max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`, stream)))
					r.Header.Set("Authorization", "Bearer test-client-key")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if !stream && w.Code != 502 || stream && (w.Code != 200 || !strings.Contains(w.Body.String(), "event: error") || strings.Contains(w.Body.String(), "event: message_stop")) {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
					events := usageEvents(t, h.usageStore)
					if len(events) != 1 || events[0].Outcome != "conversion" || events[0].Estimated {
						t.Fatalf("events=%+v", events)
					}
					e := events[0]
					if usageKind == "known" && (e.InputTokens != 100 || e.OutputTokens != 12 || e.CachedInputTokens != 64 || e.CacheCreationInputTokens != 8) {
						t.Fatalf("lost usage: %+v", e)
					}
					if (e.CachedInputTokensKnown && e.CacheCreationInputTokensKnown) != (usageKind != "absent") {
						t.Fatalf("usage presence: %+v", e)
					}
					if len(registry.ProviderHealthSnapshot()) != 0 {
						t.Fatal("conversion changed provider health")
					}
					dir := filepath.Join(root, "test-client", "000001")
					raw, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
					if err != nil {
						t.Fatal(err)
					}
					var meta archive.Metadata
					if err := json.Unmarshal(raw, &meta); err != nil {
						t.Fatal(err)
					}
					if meta.ConversionErrorPath != "output[0].phase" || meta.ConversionErrorPhase != "future_phase" || meta.ConversionErrorStage == "" || meta.ConversionErrorDetail == "" {
						t.Fatalf("meta=%s", raw)
					}
					if full {
						body, err := os.ReadFile(filepath.Join(dir, meta.UpstreamProjectionPath))
						if err != nil {
							t.Fatal(err)
						}
						if !bytes.Contains(body, []byte("private body marker")) || meta.UpstreamProjectionFormat == "" {
							t.Fatalf("missing adapter input: %s", body)
						}
					} else if meta.UpstreamProjectionPath != "" {
						t.Fatal("full-content disabled but response persisted")
					}
					if strings.Contains(logs.String(), "private body marker") || !strings.Contains(logs.String(), "conversion_error_path=output[0].phase") {
						t.Fatalf("diagnostic logs=%s", logs.String())
					}
				})
			}
		}
	}
}

func TestCodexProjectionMetadataCannotCompleteStream(t *testing.T) {
	for _, created := range []bool{false, true} {
		t.Run(fmt.Sprint(created), func(t *testing.T) {
			h, _ := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{stream: func(_ context.Context, _ codexresponses.Request, _ func(codexresponses.StreamStart) error, emit func([]byte) error) error {
				if created {
					if err := emit([]byte(`data: {"type":"response.created","response":{}}`)); err != nil {
						return err
					}
				}
				return emit([]byte(`data: {"type":"response.metadata","metadata":{"openai_chatgpt_moderation_metadata":{}}}`))
			}})
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.2-codex","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`))
			r.Header.Set("Authorization", "Bearer test-client-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if strings.Contains(w.Body.String(), "event: message_stop") || !created && w.Code != 502 || created && !strings.Contains(w.Body.String(), "event: error") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCodexProjectionArchiveBudgetAndPersistence(t *testing.T) {
	var captured codexProjectionArchive
	first := `{"type":"response.created"}`
	captured.capture(first, len(first)+8)
	if captured.truncated || captured.body.String() != "data: "+first+"\n\n" {
		t.Fatal("exact budget rejected")
	}
	captured.capture(`{"type":"response.metadata"}`, len(first)+8)
	captured.capture(`{}`, maxConversionSSEBytes)
	if !captured.truncated || captured.body.String() != "data: "+first+"\n\n" {
		t.Fatal("truncation did not preserve a complete prefix")
	}
	h, root := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{})
	recorder, err := archive.NewRecorderOptions(root, archive.RecorderOptions{FullContent: true})
	if err != nil {
		t.Fatal(err)
	}
	round, err := recorder.Start()
	if err != nil {
		t.Fatal(err)
	}
	h.archiveCodexProjection(round, captured.body.Bytes(), true, captured.truncated)
	if !round.UpstreamProjectionTruncated || round.UpstreamProjectionFormat != "responses_sse" || round.UpstreamProjectionPath != "upstream_response_001.projection.sse" {
		t.Fatalf("round=%+v", round)
	}
	h.archiveCodexProjection(nil, []byte("disabled"), false, false)
	h.archiveCodexProjection(archive.NewObservationRound(), []byte("disabled"), false, false)
}

func TestCodexProjectionHandlerSuccessHasSameStreamSemantics(t *testing.T) {
	response, source := projectionFixture()
	var content []any
	for _, stream := range []bool{false, true} {
		executor := codexResponsesExecutorStub{
			complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
				return codexresponses.Result{Body: projectionJSON(response), Attempt: codexArchiveTestAttempt()}, nil
			},
			stream: func(_ context.Context, _ codexresponses.Request, start func(codexresponses.StreamStart) error, emit func([]byte) error) error {
				if err := start(codexresponses.StreamStart{Attempt: codexArchiveTestAttempt()}); err != nil {
					return err
				}
				for _, event := range source {
					if err := emit(append([]byte("data: "), projectionJSON(event)...)); err != nil {
						return err
					}
				}
				return nil
			},
		}
		h, root := newArchivedCodexResponsesHandler(t, executor)
		recorder, err := archive.NewRecorderOptions(root, archive.RecorderOptions{MaxRounds: 10, ScopeByAPIKey: true, FullContent: true})
		if err != nil {
			t.Fatal(err)
		}
		h.interactionRecorder = recorder
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.2-codex","stream":%t,"max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`, stream)))
		r.Header.Set("Authorization", "Bearer test-client-key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if !stream {
			var complete map[string]any
			if err := decodeCodexJSON(w.Body.Bytes(), &complete); err != nil {
				t.Fatal(err)
			}
			content = complete["content"].([]any)
			if complete["stop_reason"] != "tool_use" {
				t.Fatalf("message=%v", complete)
			}
		} else {
			var events []map[string]any
			for _, line := range strings.Split(w.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event map[string]any
				if err := decodeCodexJSON([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				if index, ok := event["index"].(json.Number); ok {
					n, _ := index.Int64()
					event["index"] = int(n)
				}
				events = append(events, event)
			}
			if !reflect.DeepEqual(projectionContent(t, events), content) {
				t.Fatal("handler stream content differs from completion")
			}
			if strings.Count(w.Body.String(), "event: message_stop\n") != 1 || !strings.Contains(w.Body.String(), `"stop_reason":"tool_use"`) {
				t.Fatalf("bad terminal: %s", w.Body.String())
			}
		}
		events := usageEvents(t, h.usageStore)
		if len(events) != 1 || events[0].Outcome != "success" || events[0].InputTokens != 100 || events[0].OutputTokens != 12 {
			t.Fatalf("events=%+v", events)
		}
		if stream && !slices.Contains(events[0].IgnoredFeatures, "response_metadata") {
			t.Fatal("metadata consumption missing from observation")
		}
		for _, feature := range []string{"output_metadata", "internal_chat_message_metadata_passthrough"} {
			if !slices.Contains(events[0].IgnoredFeatures, feature) {
				t.Fatalf("output metadata consumption missing: %s", feature)
			}
		}
		name := "upstream_response_001.body.json"
		if stream {
			name = "upstream_response_001.projection.sse"
		}
		raw, err := os.ReadFile(filepath.Join(root, "test-client", "000001", name))
		if err != nil || !bytes.Contains(raw, []byte("opaque marker")) || !bytes.Contains(raw, []byte("internal_chat_message_metadata_passthrough")) {
			t.Fatalf("original metadata missing from archive: %v", err)
		}
		expected := projectionJSON(response)
		if stream {
			var captured bytes.Buffer
			for _, event := range source {
				captured.WriteString("data: ")
				captured.Write(projectionJSON(event))
				captured.WriteString("\n\n")
			}
			expected = captured.Bytes()
		}
		if !bytes.Equal(raw, expected) {
			t.Fatal("archive differs from original projection input")
		}
		if strings.Contains(w.Body.String(), "opaque marker") || strings.Contains(w.Body.String(), "internal_chat_message_metadata_passthrough") {
			t.Fatal("observational metadata leaked to client")
		}
	}
}

func TestCodexProjectionFailuresDoNotBlockNativeResponses(t *testing.T) {
	calls := 0
	h, _ := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
		calls++
		return codexresponses.Result{Body: projectionJSON(projectionResponse(projectionMessage("future_phase", "native response"))), Attempt: codexArchiveTestAttempt()}, nil
	}})
	registry := metrics.NewRegistry()
	h.metricsRegistry = metricsport.AsPort(registry)
	for range 3 {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.2-codex","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`))
		r.Header.Set("Authorization", "Bearer test-client-key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 502 {
			t.Fatalf("status=%d", w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.2-codex","input":"hello"}`))
	r.Header.Set("Authorization", "Bearer test-client-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || calls != 4 || !strings.Contains(w.Body.String(), "future_phase") {
		t.Fatalf("native request blocked or changed: calls=%d status=%d body=%s", calls, w.Code, w.Body.String())
	}
}
