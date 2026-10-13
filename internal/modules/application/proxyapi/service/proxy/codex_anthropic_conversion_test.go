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
	config "aetherrelay/internal/pkg/aetherrelayconfig"
)

func mixedAnthropicTranscript() map[string]any {
	text := func(s string) any { return map[string]any{"type": "text", "text": s} }
	call := func(id string) any {
		return map[string]any{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{"number": json.Number("9007199254740993")}}
	}
	result := func(id string) any {
		return map[string]any{"type": "tool_result", "tool_use_id": id, "content": "result"}
	}
	return map[string]any{"model": "gpt-5.2-codex", "system": "stable <instructions> & guidance", "max_tokens": 128, "messages": []any{
		cacheUserMessage("start"),
		map[string]any{"role": "assistant", "content": []any{text("before"), text(" call"), call("a"), text("between calls"), call("b")}},
		map[string]any{"role": "user", "content": []any{result("a"), text("between results"), result("b"), text("after"), text(" results")}},
	}}
}

func assertMixedTranscript(t *testing.T, raw []byte) {
	t.Helper()
	var body map[string]any
	if err := decodeCodexJSON(raw, &body); err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	want := []string{"message", "message", "function_call", "message", "function_call", "function_call_output", "message", "function_call_output", "message"}
	if len(items) != len(want) {
		t.Fatalf("items=%d want=%d", len(items), len(want))
	}
	for i, typ := range want {
		if items[i].(map[string]any)["type"] != typ {
			t.Fatalf("item %d moved", i)
		}
	}
	for i, content := range map[int]string{1: "before call", 3: "between calls", 6: "between results", 8: "after results"} {
		if items[i].(map[string]any)["content"] != content {
			t.Fatalf("item %d text changed", i)
		}
	}
	for _, pair := range [][2]int{{2, 5}, {4, 7}} {
		if items[pair[0]].(map[string]any)["call_id"] != items[pair[1]].(map[string]any)["call_id"] {
			t.Fatal("tool pairing changed")
		}
	}
	if !strings.Contains(items[2].(map[string]any)["arguments"].(string), "9007199254740993") {
		t.Fatal("tool argument integer lost precision")
	}
}

// CP-REQ-012, CP-REQ-036, CP-FP-008: preserve tool pairing, order and numbers at ingress.
func TestCodexMixedAnthropicContentKeepsOrderAtExecutor(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			body := mixedAnthropicTranscript()
			body["stream"] = stream
			raw, _ := json.Marshal(body)
			calls := 0
			check := func(req codexresponses.Request) { calls++; assertMixedTranscript(t, req.Body) }
			h, _ := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{
				complete: func(_ context.Context, req codexresponses.Request) (codexresponses.Result, error) {
					check(req)
					return codexresponses.Result{Body: []byte(`{"id":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]}`)}, nil
				},
				stream: func(_ context.Context, req codexresponses.Request, start func(codexresponses.StreamStart) error, emit func([]byte) error) error {
					check(req)
					if err := start(codexresponses.StreamStart{}); err != nil {
						return err
					}
					if err := emit([]byte(`data: {"type":"response.created","response":{"id":"response","model":"gpt-5.2-codex"}}`)); err != nil {
						return err
					}
					return emit([]byte(`data: {"type":"response.completed","response":{"id":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]}}`))
				},
			})
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
			r.Header.Set("Authorization", "Bearer test-client-key")
			r.Header.Set("X-Claude-Code-Session-Id", "mixed-session")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK || calls != 1 {
				t.Fatalf("status=%d calls=%d", w.Code, calls)
			}
		})
	}
}

// CP-REQ-012, CP-REQ-015, CP-FP-008: appending history preserves pairing and cache identity.
func TestCodexMixedContentAppendKeepsPrefixAndGenericMapping(t *testing.T) {
	body := mixedAnthropicTranscript()
	before := convertedCodexBody(t, "mixed-session", body["messages"].([]any))
	messages := body["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	last["content"] = append(last["content"].([]any), map[string]any{"type": "tool_result", "tool_use_id": "missing", "content": "bad"})
	if _, _, err := buildCodexResponsesFromAnthropicWithCapability(body, "model", true, config.ConversionCapability{}); err == nil {
		t.Fatal("unknown result accepted")
	}
	last["content"] = last["content"].([]any)[:5]
	body["messages"] = append(messages, cacheUserMessage("next turn"))
	after := convertedCodexBody(t, "mixed-session", body["messages"].([]any))
	a, b := parseConvertedPrompt(t, before), parseConvertedPrompt(t, after)
	if !reflect.DeepEqual(a.items, b.items[:len(a.items)]) || a.cacheKey != b.cacheKey {
		t.Fatal("append changed history prefix or cache identity")
	}
	generic, _, err := buildResponsesFromAnthropicWithCapability(body, "model", true, config.ConversionCapability{})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(generic, &out)
	items := out["input"].([]any)
	if len(items) != 8 || items[1].(map[string]any)["content"] != "before callbetween calls" || items[5].(map[string]any)["type"] != "function_call_output" {
		t.Fatal("generic mapping changed")
	}
}

// CP-REQ-015, CP-FP-008: new text must follow the existing immutable input prefix.
func TestCodexTextAppendedAfterExistingToolResultKeepsPrefix(t *testing.T) {
	body := mixedAnthropicTranscript()
	messages := body["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	last["content"] = last["content"].([]any)[:3] // The history ends at result b.
	before := parseConvertedPrompt(t, convertedCodexBody(t, "mixed-session", messages))
	last["content"] = append(last["content"].([]any), map[string]any{"type": "text", "text": "follow-up instruction"})
	after := parseConvertedPrompt(t, convertedCodexBody(t, "mixed-session", messages))
	if len(after.items) != len(before.items)+1 || !reflect.DeepEqual(before.items, after.items[:len(before.items)]) {
		t.Fatal("new text was inserted before an existing tool result")
	}
	if before.instructions != after.instructions || !reflect.DeepEqual(before.tools, after.tools) || before.cacheKey != after.cacheKey {
		t.Fatal("text append changed stable fields")
	}
}

// CP-REQ-035, CP-REQ-036: object normalization preserves schema ownership and wire values.
func TestCodexConvertedObjectMatchesBytePathAndOwnsSchemas(t *testing.T) {
	body := mixedAnthropicTranscript()
	body["tools"] = []any{map[string]any{"name": "Read", "input_schema": map[string]any{"$id": "source-id", "properties": map[string]any{"number": map[string]any{"type": "integer", "default": json.Number("9007199254740993")}}}}}
	body["output_config"] = map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object", "properties": map[string]any{}}}}
	capability := config.ConversionCapability{Images: true, StructuredOutput: true}
	source, _ := json.Marshal(body)
	encoded, degraded, err := buildCodexResponsesFromAnthropicWithCapability(body, "model", true, capability)
	if err != nil {
		t.Fatal(err)
	}
	_, legacy, ignored, err := normalizeCodexRequest(encoded, false)
	if err != nil {
		t.Fatal(err)
	}
	object, gotDegraded, err := buildResponsesObjectFromAnthropic(body, "model", true, capability, historicalSystemDeveloperMessage)
	if err != nil {
		t.Fatal(err)
	}
	gotIgnored, err := normalizeCodexRequestObject(object, codexNormalizationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	oldBytes, _ := encodeCodexJSON(legacy)
	newBytes, _ := encodeCodexJSON(object)
	if !bytes.Equal(oldBytes, newBytes) || !reflect.DeepEqual(degraded, gotDegraded) || !reflect.DeepEqual(ignored, gotIgnored) {
		t.Fatal("object and byte normalization differ")
	}
	unchanged, _ := json.Marshal(body)
	if !bytes.Equal(source, unchanged) {
		t.Fatal("normalizer mutated source schema or history")
	}
	if bytes.Contains(newBytes, []byte("source-id")) || !bytes.Contains(newBytes, []byte("9007199254740993")) || !bytes.Contains(newBytes, []byte("<instructions> &")) {
		t.Fatal("normalization or byte preservation failed")
	}
}

func BenchmarkCodexAnthropicConversion(b *testing.B) {
	body := mixedAnthropicTranscript()
	body["messages"].([]any)[0].(map[string]any)["content"] = strings.Repeat("stable context <value> & data ", 10000)
	capability := config.ConversionCapability{Images: true}
	for _, objectPath := range []bool{false, true} {
		b.Run(map[bool]string{false: "byte_roundtrips", true: "owned_object"}[objectPath], func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if objectPath {
					object, _, err := buildResponsesObjectFromAnthropic(body, "model", true, capability, historicalSystemDeveloperMessage)
					if err != nil {
						b.Fatal(err)
					}
					if _, err := normalizeCodexRequestObject(object, codexNormalizationOptions{}); err != nil {
						b.Fatal(err)
					}
					setCodexPromptCacheKey(object, "stable-cache")
					if _, err := encodeCodexJSON(object); err != nil {
						b.Fatal(err)
					}
				} else {
					raw, _, err := buildCodexResponsesFromAnthropicWithCapability(body, "model", true, capability)
					if err != nil {
						b.Fatal(err)
					}
					raw, object, _, err := normalizeCodexRequest(raw, false)
					if err != nil {
						b.Fatal(err)
					}
					if _, _, _, err := ensureCodexPromptCacheKey(raw, object, "stable-cache"); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
