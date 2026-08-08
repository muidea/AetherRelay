package admin

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func TestMutateModelMetadataYAMLPreservesConversionTemplate(t *testing.T) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte("model_metadata:\n  demo:\n    context_window_tokens: 1000\n    conversion_capabilities:\n      messages:\n        profile: level3_reasoning\n"), &root); err != nil {
		t.Fatal(err)
	}
	mapping, err := documentRoot(&root)
	if err != nil {
		t.Fatal(err)
	}
	context := 2000
	if err := mutateModelMetadataYAML(mapping, "demo", modelMetadataPatch{ContextWindowTokens: &context}); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(&root)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{"context_window_tokens: \"2000\"", "messages:", "profile: level3_reasoning"} {
		if !strings.Contains(text, want) {
			t.Fatalf("rewritten YAML missing %q:\n%s", want, text)
		}
	}
}
