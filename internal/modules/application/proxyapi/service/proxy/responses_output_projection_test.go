package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestResponsesOutputNormalizationPreservesSource(t *testing.T) {
	source := map[string]any{"type": "function_call", "call_id": "edit", "name": "Edit", "arguments": `{}`,
		"metadata": map[string]any{"future": []any{nil, true, map[string]any{"key": "opaque"}}},
		"internal_chat_message_metadata_passthrough": nil,
		"internal_future_routing":                    "must remain for rejection"}
	before := projectionJSON(source)
	item, ignored := normalizeResponsesOutputItem(source)
	if _, ok := item["metadata"]; ok || !slices.Contains(ignored, "output_metadata") || !slices.Contains(ignored, "internal_chat_message_metadata_passthrough") {
		t.Fatalf("item=%v ignored=%v", item, ignored)
	}
	if !bytes.Equal(before, projectionJSON(source)) || item["internal_future_routing"] != source["internal_future_routing"] {
		t.Fatal("source mutated or unknown field silently removed")
	}
}

// Minimized from x800 Event 0f733a6913ea1e29855f4e07b4e64f16. The actual
// capture stops at Edit/added; the rest is a synthetic lifecycle, not live proof.
func editProjectionItem(arguments string, extra map[string]any) map[string]any {
	item := map[string]any{"type": "function_call", "id": "fc_edit", "call_id": "call_edit", "name": "Edit", "arguments": arguments, "status": "in_progress"}
	for key, value := range extra {
		item[key] = value
	}
	return item
}

func TestCodexProjectionOutputMetadataLifecycle(t *testing.T) {
	for label, extra := range map[string]map[string]any{
		"absent":        nil,
		"null":          {"metadata": nil, "internal_chat_message_metadata_passthrough": nil},
		"empty":         {"metadata": map[string]any{}, "internal_chat_message_metadata_passthrough": map[string]any{}},
		"live_shape":    {"metadata": map[string]any{"turn_id": "fixture-turn"}, "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "fixture-turn"}},
		"future_nested": {"metadata": map[string]any{"future": map[string]any{"data": []any{true, nil, "opaque marker"}}}, "internal_chat_message_metadata_passthrough": []any{"opaque marker", map[string]any{"future": false}}},
	} {
		for _, mode := range []string{"delta", "delta_reformatted", "arguments_done_only", "item_done_only", "terminal_only"} {
			t.Run(label+"/"+mode, func(t *testing.T) {
				arguments := `{"file_path":"/tmp/example.txt","number":9007199254740993}`
				item := editProjectionItem(arguments, extra)
				item["status"] = "completed"
				response := projectionResponse(item)
				source := []map[string]any{{"type": "response.created", "response": map[string]any{"id": "resp_edit", "model": "gpt-6.1-sol"}}}
				if mode != "terminal_only" {
					source = append(source, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": editProjectionItem("", extra)})
					if mode == "delta" || mode == "delta_reformatted" {
						source = append(source, map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": arguments[:20]}, map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": arguments[20:]})
					}
					if mode != "item_done_only" {
						doneArguments := arguments
						if mode == "delta_reformatted" {
							doneArguments = "{\n\"number\":9007199254740993,\"file_path\":\"/tmp/example.txt\"}"
						}
						source = append(source, map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "arguments": doneArguments})
					}
					source = append(source, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
				}
				source = append(source, map[string]any{"type": "response.completed", "response": response})
				before := projectionJSON(source)
				mapper, state := codexResponsesEventToAnthropic(projectionCapability), &textConversionStreamState{}
				var events []map[string]any
				for _, event := range source {
					out, err := mapper(projectionJSON(event), state)
					if err != nil {
						t.Fatalf("event=%s: %v", event["type"], err)
					}
					events = append(events, out...)
				}
				raw, usage, ignored, err := convertCodexResponsesToAnthropic(projectionJSON(response), "fallback", projectionCapability)
				if err != nil {
					t.Fatal(err)
				}
				var complete map[string]any
				if err := decodeCodexJSON(raw, &complete); err != nil {
					t.Fatal(err)
				}
				content := projectionContent(t, events)
				if len(content) != 1 || !reflect.DeepEqual(content, complete["content"]) || complete["stop_reason"] != "tool_use" || !state.Completed || state.tokenUsage() != usage {
					t.Fatalf("stream=%v complete=%v state=%+v", content, complete, state)
				}
				if label != "absent" && (!slices.Contains(ignored, "output_metadata") || !slices.Contains(state.IgnoredFeatures, "output_metadata")) {
					t.Fatalf("ignored=%v state=%+v", ignored, state)
				}
				if !bytes.Equal(before, projectionJSON(source)) || bytes.Contains(projectionJSON(events), []byte("opaque marker")) {
					t.Fatal("source changed or observational data leaked to client")
				}
			})
		}
	}
}

func TestCodexProjectionRejectsUnknownItemFieldsInEveryStage(t *testing.T) {
	for _, field := range []string{"namespace", "future_execution_hint", "internal_future_routing", "metadata_namespace"} {
		for _, kind := range []string{"function_call", "message"} {
			for _, stage := range []string{"response.output_item.added", "response.output_item.done", "terminal"} {
				t.Run(fmt.Sprintf("%s/%s/%s", kind, field, stage), func(t *testing.T) {
					item := editProjectionItem(`{}`, map[string]any{"metadata": map[string]any{"future": true}, field: "unsupported"})
					if kind == "message" {
						item = projectionMessage("final_answer", "text")
						item[field] = "unsupported"
					}
					var err error
					if stage == "terminal" {
						_, _, _, err = convertCodexResponsesToAnthropic(projectionJSON(projectionResponse(item)), "fallback", projectionCapability)
					} else {
						mapper, state := codexResponsesEventToAnthropic(projectionCapability), &textConversionStreamState{}
						_, _ = mapper([]byte(`{"type":"response.created","response":{}}`), state)
						_, err = mapper(projectionJSON(map[string]any{"type": stage, "output_index": 0, "item": item}), state)
					}
					var detail *responseProjectionError
					if !errors.As(err, &detail) || !strings.HasSuffix(detail.Path, "."+field) {
						t.Fatalf("missing field diagnostic: %v", err)
					}
				})
			}
		}
	}
}

func TestCodexProjectionChecksCompletedToolArguments(t *testing.T) {
	for _, stage := range []string{"response.function_call_arguments.done", "response.output_item.done"} {
		for _, arguments := range []any{`{"value":2}`, true, `{incomplete`} {
			mapper, state := codexResponsesEventToAnthropic(projectionCapability), &textConversionStreamState{}
			for _, event := range []map[string]any{
				{"type": "response.created", "response": map[string]any{}},
				{"type": "response.output_item.added", "output_index": 0, "item": editProjectionItem("", nil)},
				{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": `{"value":1}`},
			} {
				if _, err := mapper(projectionJSON(event), state); err != nil {
					t.Fatal(err)
				}
			}
			event := map[string]any{"type": stage, "output_index": 0, "arguments": arguments}
			if stage == "response.output_item.done" {
				item := editProjectionItem("", nil)
				item["arguments"] = arguments
				event["item"] = item
			}
			out, err := mapper(projectionJSON(event), state)
			var detail *responseProjectionError
			if !errors.As(err, &detail) || !strings.HasSuffix(detail.Path, ".arguments") || len(out) != 0 || state.Completed {
				t.Fatalf("stage=%s args=%v out=%v error=%v", stage, arguments, out, err)
			}
		}
	}
	if !equivalentResponsesToolArguments(`{"a":1,"b":2}`, "{\n\"b\":2,\"a\":1}") {
		t.Fatal("formatting-only argument difference rejected")
	}
}
