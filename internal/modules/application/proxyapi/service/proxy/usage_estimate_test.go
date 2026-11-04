package proxy

import (
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	"testing"
)

func TestResponsesUsageEstimateWithoutUpstreamCounters(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		outputTokens int
	}{
		{"text only", `{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"abcdefgh"}]}]}`, 2},
		{"tools and reasoning are not visible text", `{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"private"}]},{"type":"function_call","name":"tool","arguments":"large arguments"},{"type":"message","content":[{"type":"output_text","text":"abcdefgh"},{"type":"refusal","refusal":"private refusal"}]}]}`, 2},
		{"tool only", `{"output":[{"type":"function_call","name":"tool","arguments":"large arguments"}]}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := usageFromRawResponse(config.Provider{Protocol: "openai"}, []byte(tc.body), map[string]any{"input": "hello"})
			if !usage.Known || !usage.Estimated || usage.CompletionTokens != tc.outputTokens || usage.PromptTokens <= 0 || usage.CachedInputTokensKnown || usage.CacheCreationInputTokensKnown {
				t.Fatalf("estimated usage=%+v", usage)
			}
		})
	}
}
