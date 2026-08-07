package admin

import (
	"strings"
	"testing"

	config "ai-proxy/internal/pkg/aiproxyconfig"
	"go.yaml.in/yaml/v4"
)

func TestMutateModelMetadataYAMLPreservesReasoningAdapter(t *testing.T) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte("model_metadata:\n  demo:\n    reasoning_supported: true\n    reasoning_efforts: [low]\n"), &root); err != nil {
		t.Fatal(err)
	}
	capabilities := map[string]config.ConversionCapability{
		config.ConversionDirectionResponsesToAnthropic: {
			Level: 2, Text: true, Streaming: true, Reasoning: true,
			ReasoningAdapter: config.ReasoningAdapterResponsesToAnthropicAdaptive, ReasoningTargetEffort: "low",
		},
	}
	mapping, err := documentRoot(&root)
	if err != nil {
		t.Fatal(err)
	}
	if err := mutateModelMetadataYAML(mapping, "demo", modelMetadataPatch{ConversionCapabilities: &capabilities}); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(&root)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{"reasoning_adapter: responses_to_anthropic_adaptive", "reasoning_target_effort: low"} {
		if !strings.Contains(text, want) {
			t.Fatalf("rewritten YAML missing %q:\n%s", want, text)
		}
	}
}
