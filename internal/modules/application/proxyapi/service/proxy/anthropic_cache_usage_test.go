package proxy

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	config "aetherrelay/internal/pkg/aetherrelayconfig"
)

// CP-OBS-007: Anthropic reports input_tokens without the tokens read from or
// written to a cache, so the internal accounting must add them before any ratio
// or total is derived. Golden values come from the 2026-09-22 deepseek-flash
// incident: input_tokens=1533 with cache_read_input_tokens=176128 produced a
// cache usage rate of 11489.1% because the denominator kept the raw input.
const anthropicCacheIncidentUsage = `{"input_tokens":1533,"cache_creation_input_tokens":0,"cache_read_input_tokens":176128,"output_tokens":542,"service_tier":"standard"}`

func TestAnthropicStreamUsageUpdatesAreCumulative(t *testing.T) {
	acc := newAnthropicRawStreamAccumulator("model")
	var usage tokenUsage
	var content strings.Builder
	id, model, finish := "", "model", ""
	role := false
	for _, step := range []struct {
		event                        string
		input, read, created, output int
	}{
		{`{"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":0}}}`, 100, 0, 0, 0},
		{`{"type":"message_delta","usage":{"cache_read_input_tokens":200,"cache_creation_input_tokens":30,"output_tokens":5}}`, 330, 200, 30, 5},
		{`{"type":"message_delta","usage":{"cache_read_input_tokens":200,"cache_creation_input_tokens":30,"output_tokens":5}}`, 330, 200, 30, 5},
		{`{"type":"message_delta","usage":{"output_tokens":8}}`, 330, 200, 30, 8},
		{`{"type":"message_delta","usage":{"input_tokens":90,"cache_read_input_tokens":150,"cache_creation_input_tokens":0}}`, 240, 150, 0, 8},
	} {
		acc.TrackSSELine([]byte("data: " + step.event))
		if _, err := anthropicStreamEvents(step.event, &id, &model, &usage, &content, &finish, &role, 0); err != nil {
			t.Fatal(err)
		}
		if usage.PromptTokens != step.input || usage.CachedInputTokens != step.read || usage.CacheCreationInputTokens != step.created || usage.CompletionTokens != step.output {
			t.Fatalf("converted usage=%+v step=%+v", usage, step)
		}
		if acc.InputTokens != step.input || acc.CachedInputTokens != step.read || acc.CacheCreationInputTokens != step.created || acc.OutputTokens != step.output {
			t.Fatalf("raw accumulator differs at %+v", step)
		}
	}
	final := acc.FinalizeUsage(nil)
	if final.CacheHitRate() > 1 {
		t.Fatalf("rate=%v", final.CacheHitRate())
	}
	if got := anthropicUsagePayload(final)["input_tokens"]; got != 90 {
		t.Fatalf("archived input=%v", got)
	}
}

func TestAnthropicUsageNormalizesCacheIntoInput(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(anthropicCacheIncidentUsage), &raw); err != nil {
		t.Fatal(err)
	}
	usage, ok := anthropicUsage(raw)
	if !ok {
		t.Fatal("usage object was not recognized")
	}
	if usage.PromptTokens != 177661 || usage.CompletionTokens != 542 {
		t.Fatalf("normalized tokens=%+v", usage)
	}
	if !usage.CachedInputTokensKnown || usage.CachedInputTokens != 176128 {
		t.Fatalf("cache read lost: %+v", usage)
	}
	// An explicit zero stays a known zero; missing counters stay unknown.
	if !usage.CacheCreationInputTokensKnown || usage.CacheCreationInputTokens != 0 {
		t.Fatalf("explicit zero creation lost: %+v", usage)
	}
	rate := usage.CacheHitRate()
	if rate > 1 || math.Abs(rate-176128.0/177661.0) > 1e-12 {
		t.Fatalf("cache usage rate=%v", rate)
	}
	// A fully cached prompt is the boundary: the rate lands on 1, never above.
	cached, ok := anthropicUsage(map[string]any{"input_tokens": 0, "output_tokens": 0, "cache_read_input_tokens": 5})
	if !ok || cached.PromptTokens != 5 || cached.CacheHitRate() != 1 {
		t.Fatalf("fully cached prompt=%+v rate=%v", cached, cached.CacheHitRate())
	}
	// Cache creation counts as input too.
	created, _ := anthropicUsage(map[string]any{"input_tokens": 100, "output_tokens": 1, "cache_creation_input_tokens": 40})
	if created.PromptTokens != 140 || created.CachedInputTokensKnown {
		t.Fatalf("cache creation folding=%+v", created)
	}
	// Absent counters must not be treated as declared cache activity.
	bare, _ := anthropicUsage(map[string]any{"input_tokens": 100, "output_tokens": 1})
	if bare.PromptTokens != 100 || bare.CachedInputTokensKnown || bare.CacheCreationInputTokensKnown {
		t.Fatalf("absent counters must stay unknown: %+v", bare)
	}
}

func TestAnthropicUsagePayloadRestoresWireShape(t *testing.T) {
	usage, _ := anthropicUsage(map[string]any{"input_tokens": 1533, "output_tokens": 542, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 176128})
	payload := anthropicUsagePayload(usage)
	if payload["input_tokens"] != 1533 || payload["output_tokens"] != 542 {
		t.Fatalf("payload=%v", payload)
	}
	if payload["cache_read_input_tokens"] != 176128 || payload["cache_creation_input_tokens"] != 0 {
		t.Fatalf("payload=%v", payload)
	}
	// Round trip: re-parsing the restored wire shape reproduces the internal
	// usage, so the archived message and the accounting cannot drift apart.
	restored, _ := anthropicUsage(payload)
	if restored.PromptTokens != usage.PromptTokens || restored.CachedInputTokens != usage.CachedInputTokens {
		t.Fatalf("round trip=%+v want=%+v", restored, usage)
	}
}

func TestUsageFromRawResponseNormalizesAnthropicCache(t *testing.T) {
	body := []byte(`{"id":"msg_x","type":"message","model":"deepseek-flash","content":[{"type":"text","text":"hi"}],"usage":` + anthropicCacheIncidentUsage + `}`)
	usage := usageFromRawResponse(config.Provider{Protocol: "anthropic"}, body, nil)
	if usage.PromptTokens != 177661 || usage.CachedInputTokens != 176128 || !usage.Known {
		t.Fatalf("usage=%+v", usage)
	}
	if rate := usage.CacheHitRate(); rate > 1 {
		t.Fatalf("cache usage rate=%v", rate)
	}
	// A non-Anthropic provider keeps the OpenAI convention untouched: there
	// prompt_tokens already includes the cached tokens.
	openAI := usageFromRawResponse(config.Provider{Protocol: "openai"}, []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":80}}}`), nil)
	if openAI.PromptTokens != 100 || openAI.CachedInputTokens != 80 {
		t.Fatalf("openai usage=%+v", openAI)
	}
}

func TestOpenAIChatToAnthropicResponseDeclaresCacheReads(t *testing.T) {
	// The reverse direction (OpenAI chat upstream, Anthropic client) must not
	// leave the cache reads inside input_tokens, or the Anthropic client would
	// count the prompt twice.
	body := []byte(`{"id":"chatcmpl-1","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"prompt_tokens_details":{"cached_tokens":80}}}`)
	encoded, usage, err := convertOpenAIChatToAnthropicResponse(body, "fallback-model")
	if err != nil {
		t.Fatal(err)
	}
	if usage.PromptTokens != 100 || usage.CompletionTokens != 5 || !usage.CachedInputTokensKnown || usage.CachedInputTokens != 80 {
		t.Fatalf("usage=%+v", usage)
	}
	if usage.TotalTokens != 105 {
		t.Fatalf("total tokens=%d", usage.TotalTokens)
	}
	var payload struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Usage["input_tokens"] != float64(20) || payload.Usage["output_tokens"] != float64(5) {
		t.Fatalf("anthropic usage=%v", payload.Usage)
	}
	if payload.Usage["cache_read_input_tokens"] != float64(80) {
		t.Fatalf("anthropic usage=%v", payload.Usage)
	}
	// Without cache details the projection keeps the plain prompt split.
	plain, _, err := convertOpenAIChatToAnthropicResponse([]byte(`{"id":"chatcmpl-2","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":1}}`), "fallback-model")
	if err != nil {
		t.Fatal(err)
	}
	var plainPayload struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(plain, &plainPayload); err != nil {
		t.Fatal(err)
	}
	if plainPayload.Usage["input_tokens"] != float64(7) || plainPayload.Usage["cache_read_input_tokens"] != nil {
		t.Fatalf("plain anthropic usage=%v", plainPayload.Usage)
	}
}

func TestAnthropicRawStreamAccumulatorKeepsCacheInclusiveInput(t *testing.T) {
	accumulator := newAnthropicRawStreamAccumulator("fallback-model")
	accumulator.SetMaxContent(1024)
	accumulator.TrackSSELine([]byte(`data: {"type":"message_start","message":{"id":"msg_x","model":"deepseek-flash","usage":` + anthropicCacheIncidentUsage + `}}`))
	accumulator.TrackSSELine([]byte(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`))
	accumulator.TrackSSELine([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + anthropicCacheIncidentUsage + `}`))
	usage := accumulator.FinalizeUsage(nil)
	if usage.PromptTokens != 177661 || usage.CompletionTokens != 542 {
		t.Fatalf("accumulated usage=%+v", usage)
	}
	if usage.CachedInputTokens != 176128 || !usage.CachedInputTokensKnown {
		t.Fatalf("accumulated cache read=%+v", usage)
	}
	if rate := usage.CacheHitRate(); rate > 1 {
		t.Fatalf("cache usage rate=%v", rate)
	}
	// The archived message keeps the upstream wire shape: no double counting.
	encoded, err := accumulator.ResponseJSON(usage)
	if err != nil {
		t.Fatal(err)
	}
	var message struct {
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(encoded, &message); err != nil {
		t.Fatal(err)
	}
	if message.Usage.InputTokens != 1533 || message.Usage.OutputTokens != 542 || message.Usage.CacheReadInputTokens != 176128 {
		t.Fatalf("archived usage=%+v", message.Usage)
	}
}
