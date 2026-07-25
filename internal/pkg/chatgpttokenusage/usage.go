// Package tokenusage defines the bounded token accounting view shared across
// upstream protocol results and persisted image tasks.
package tokenusage

import "unicode"

// Usage covers the token names returned by the supported ChatGPT/OpenAI
// response variants. A producer sets the fields it receives; absent counters
// remain zero rather than being represented as an unbounded map.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	InputTokens      int `json:"input_tokens,omitempty"`
	OutputTokens     int `json:"output_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

// EstimateTextTokens returns a stable local approximation for text when an
// upstream protocol does not expose tokenizer counts. ASCII words are charged
// at roughly four characters per token, while non-ASCII letters and symbols
// are counted per rune. It deliberately does not infer image token usage.
func EstimateTextTokens(value string) int {
	tokens, asciiRun := 0, 0
	flushASCII := func() {
		if asciiRun > 0 {
			tokens += (asciiRun + 3) / 4
			asciiRun = 0
		}
	}
	for _, r := range value {
		if unicode.IsSpace(r) {
			flushASCII()
			continue
		}
		if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			asciiRun++
			continue
		}
		flushASCII()
		tokens++
	}
	flushASCII()
	return tokens
}

// EstimateChatTextUsage creates the OpenAI-compatible text counters from a
// normalized prompt and completion. It is an estimate, not upstream billing.
func EstimateChatTextUsage(promptParts []string, completion string) *Usage {
	prompt := 0
	for _, part := range promptParts {
		prompt += EstimateTextTokens(part)
	}
	completionTokens := EstimateTextTokens(completion)
	return &Usage{
		PromptTokens:     prompt,
		CompletionTokens: completionTokens,
		TotalTokens:      prompt + completionTokens,
	}
}
