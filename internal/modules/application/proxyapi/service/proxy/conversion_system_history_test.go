package proxy

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	config "aetherrelay/internal/pkg/aetherrelayconfig"
)

func TestCodexHistoricalSystemPreservesOrderAndStableInstructions(t *testing.T) {
	messages := []any{cacheUserMessage("start")}
	var previous convertedPrompt
	for turn := 1; turn <= 3; turn++ {
		messages = append(messages, map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "<total_tokens>900 tokens left</total_tokens>"}, map[string]any{"type": "text", "text": " Keep tool results intact."}}})
		messages = append(messages, cacheToolRound(turn, 128)...)
		before, _ := json.Marshal(messages)
		raw := convertedCodexBody(t, "stable-session", messages)
		after, _ := json.Marshal(messages)
		if !bytes.Equal(before, after) {
			t.Fatal("source mutated")
		}
		if repeated := convertedCodexBody(t, "stable-session", messages); !bytes.Equal(raw, repeated) {
			t.Fatal("conversion not deterministic")
		}
		current := parseConvertedPrompt(t, raw)
		if current.instructions == "" || strings.Contains(current.instructions, "<total_tokens>") {
			t.Fatal("historical system changed top-level instructions")
		}
		if current.instructions != previous.instructions && turn > 1 {
			t.Fatal("top-level instructions changed with history")
		}
		if turn > 1 {
			if current.cacheKey != previous.cacheKey || !reflect.DeepEqual(current.tools, previous.tools) {
				t.Fatal("stable fields changed")
			}
			if !reflect.DeepEqual(current.items[:len(previous.items)], previous.items) {
				t.Fatal("history prefix changed")
			}
		}
		var items []map[string]any
		for _, entry := range current.items {
			var item map[string]any
			if err := json.Unmarshal(entry, &item); err != nil {
				t.Fatal(err)
			}
			items = append(items, item)
		}
		for i := 0; i < turn; i++ {
			item := items[1+i*4]
			if item["role"] != "developer" || item["type"] != "message" {
				t.Fatal("historical system role or order lost")
			}
			content, ok := item["content"].([]any)
			if !ok || len(content) != 1 {
				t.Fatal("historical system content lost")
			}
			block, ok := content[0].(map[string]any)
			if !ok || block["type"] != "input_text" || block["text"] != "<total_tokens>900 tokens left</total_tokens> Keep tool results intact." {
				t.Fatal("historical system text changed")
			}
		}
		base := 2 + (turn-1)*4
		if items[base]["role"] != "user" || items[base+1]["type"] != "function_call" || items[base+2]["type"] != "function_call_output" || items[base+1]["call_id"] != items[base+2]["call_id"] {
			t.Fatal("tool chain order lost")
		}
		previous = current
	}
}

func TestHistoricalSystemRejectsNonText(t *testing.T) {
	for _, typ := range []string{"tool_use", "tool_result", "image", "thinking"} {
		_, err := buildResponsesFromAnthropic(map[string]any{"messages": []any{map[string]any{"role": "system", "content": []any{map[string]any{"type": typ}}}}}, "model", false)
		if err == nil {
			t.Fatalf("accepted system block %s", typ)
		}
	}
}

func TestHistoricalSystemBetweenToolCallAndResult(t *testing.T) {
	messages := cacheToolRound(1, 32)
	messages = append(messages[:2:2], append([]any{map[string]any{"role": "system", "content": "Keep the pending tool result."}}, messages[2:]...)...)
	raw, _, err := buildCodexResponsesFromAnthropicWithCapability(map[string]any{"messages": messages}, "model", false, config.ConversionCapability{})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input        []map[string]any `json:"input"`
		Instructions string           `json:"instructions"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Instructions != "" || len(body.Input) != 4 || body.Input[1]["type"] != "function_call" || body.Input[2]["role"] != "developer" || body.Input[3]["type"] != "function_call_output" || body.Input[1]["call_id"] != body.Input[3]["call_id"] {
		t.Fatalf("incorrect transcript: %s", raw)
	}
}

func TestGenericResponsesHistoricalSystemMappingUnchanged(t *testing.T) {
	raw, _, err := buildResponsesFromAnthropicWithCapability(map[string]any{
		"system": "stable", "messages": []any{map[string]any{"role": "user", "content": "hello"}, map[string]any{"role": "system", "content": " dynamic"}},
	}, "model", false, config.ConversionCapability{})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Instructions string           `json:"instructions"`
		Input        []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Instructions != "stable dynamic" || len(body.Input) != 1 || body.Input[0]["role"] != "user" {
		t.Fatal("non-Codex Responses mapping changed")
	}
}

func TestCodexCacheSummaryContainsOnlyDigestsAndCounts(t *testing.T) {
	a := []byte(`{"instructions":"secret instructions","tools":[{"name":"private tool"}],"prompt_cache_key":"private session","input":[{"role":"system","content":"private history"},{"role":"user"}]}`)
	attrs := codexConversionCacheSummary(a)
	if len(attrs) != 5 {
		t.Fatalf("attrs=%v", attrs)
	}
	for _, attr := range attrs {
		if strings.HasSuffix(attr.Key, "_digest") && len(attr.Value.String()) != 64 {
			t.Fatalf("not a digest: %v", attr)
		}
	}
	if attrs[3].Value.Int64() != 2 || attrs[4].Value.Int64() != 1 {
		t.Fatal("incorrect counts")
	}
	if !reflect.DeepEqual(attrs, codexConversionCacheSummary(a)) {
		t.Fatal("unstable summary")
	}
	b := bytes.Replace(a, []byte("secret instructions"), []byte("changed instructions"), 1)
	other := codexConversionCacheSummary(b)
	if attrs[0].Equal(other[0]) || !attrs[1].Equal(other[1]) || !attrs[2].Equal(other[2]) {
		t.Fatal("digest scope incorrect")
	}
}
