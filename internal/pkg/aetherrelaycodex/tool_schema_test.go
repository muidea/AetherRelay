package aetherrelaycodex

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeToolSchemasAppliesCodexCompatibilityRules(t *testing.T) {
	var body map[string]any
	decoder := json.NewDecoder(strings.NewReader(`{"tools":[{"type":"namespace","tools":[{"type":"function","name":"lookup","parameters":{"$schema":"draft","$id":"root","type":["object","null"],"pattern":"\\p{L}+","description":{"pattern":"\\p{L}+"},"properties":{"a.b":{"type":"string","pattern":"\\P{N}+"},"choice":{"oneOf":[{"const":9007199254740993},{"const":"b"},{"const":"c"},{"const":"d"},{"const":"e"},{"const":"f"},{"const":"g"},{"const":"h"}]}}}}]}]}`))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		t.Fatal(err)
	}
	NormalizeToolSchemas(body["tools"])
	tool := body["tools"].([]any)[0].(map[string]any)["tools"].([]any)[0].(map[string]any)
	schema := tool["parameters"].(map[string]any)
	if _, ok := schema["$schema"]; ok || schema["$id"] != nil || schema["pattern"] != nil {
		t.Fatalf("dialect or unsupported pattern retained: %#v", schema)
	}
	if _, ok := schema["properties"].(map[string]any); !ok {
		t.Fatalf("object properties missing: %#v", schema)
	}
	properties := schema["properties"].(map[string]any)
	if properties["a.b"].(map[string]any)["pattern"] != nil {
		t.Fatalf("nested pattern retained: %#v", properties["a.b"])
	}
	if properties["choice"].(map[string]any)["oneOf"] != nil || len(properties["choice"].(map[string]any)["enum"].([]any)) != 8 {
		t.Fatalf("pure const union not normalized: %#v", properties["choice"])
	}
	if schema["description"].(map[string]any)["pattern"] != `\p{L}+` {
		t.Fatalf("non-schema user data changed: %#v", schema["description"])
	}
	first, _ := json.Marshal(body)
	NormalizeToolSchemas(body["tools"])
	second, _ := json.Marshal(body)
	if !reflect.DeepEqual(first, second) || !bytes.Contains(first, []byte("9007199254740993")) {
		t.Fatalf("normalization is not idempotent or lost integer: %s / %s", first, second)
	}
}

func TestNormalizeToolSchemasRepairsExplicitNullOnly(t *testing.T) {
	tools := []any{
		map[string]any{"type": "function", "name": "a", "parameters": nil},
		map[string]any{"type": "custom", "name": "b"},
	}
	NormalizeToolSchemas(tools)
	if tools[0].(map[string]any)["parameters"].(map[string]any)["type"] != "object" {
		t.Fatal("explicit null parameters not repaired")
	}
	if _, exists := tools[1].(map[string]any)["parameters"]; exists {
		t.Fatal("missing custom parameters were invented")
	}
}

func TestNormalizeToolSchemasKeepsUnionWhenExistingEnumHasDuplicates(t *testing.T) {
	branches := make([]any, 0, complexUnionBranchThreshold)
	values := make([]any, 0, complexUnionBranchThreshold)
	for index := range complexUnionBranchThreshold {
		branches = append(branches, map[string]any{"const": json.Number(string(rune('1' + index)))})
		values = append(values, json.Number(string(rune('1'+index))))
	}
	values[len(values)-1] = values[0]
	parameter := map[string]any{"type": "object", "properties": map[string]any{"choice": map[string]any{"oneOf": branches, "enum": values}}}
	tools := []any{map[string]any{"type": "function", "name": "choose", "parameters": parameter}}
	NormalizeToolSchemas(tools)
	choice := parameter["properties"].(map[string]any)["choice"].(map[string]any)
	if _, exists := choice["oneOf"]; !exists {
		t.Fatal("non-equivalent duplicate enum removed the constraining union")
	}
}
