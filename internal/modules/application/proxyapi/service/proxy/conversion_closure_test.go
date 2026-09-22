package proxy

import (
	"encoding/json"
	"testing"

	config "aetherrelay/internal/pkg/aetherrelayconfig"
)

func TestAnthropicStructuredFormatAndIndependentEffort(t *testing.T) {
	var body map[string]any
	if err := json.Unmarshal([]byte(`{"messages":[{"role":"user","content":"title"}],"max_tokens":100,"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}}}`), &body); err != nil {
		t.Fatal(err)
	}
	capability, err := anthropicTargetReasoning(body, config.ModelMetadata{ReasoningDeclared: true, ReasoningSupported: true, ReasoningEfforts: []string{"high"}}, config.ConversionCapability{StructuredOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := buildResponsesFromAnthropicWithCapability(body, "model", false, capability)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["output_config"] != nil || out["reasoning"].(map[string]any)["effort"] != "high" {
		t.Fatalf("projection=%s", raw)
	}
	format := out["text"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" || format["strict"] != true || format["schema"] == nil {
		t.Fatalf("format=%v", format)
	}
	if _, ok := body["output_config"].(map[string]any)["format"]; !ok {
		t.Fatal("source request mutated")
	}
	if _, err = anthropicTargetReasoning(body, config.ModelMetadata{ReasoningDeclared: true, ReasoningSupported: true, ReasoningEfforts: []string{"low"}}, capability); err == nil {
		t.Fatal("unsupported effort accepted")
	}
}

func TestUnknownConversionFieldHasLocation(t *testing.T) {
	err := rejectConversionFields(map[string]any{"unexpected": true}, map[string]struct{}{})
	result := conversionAPIError(TransportPlan{}, err)
	if result.Feature != "unexpected" || result.Param != "unexpected" {
		t.Fatalf("diagnostic=%+v", result)
	}
}
