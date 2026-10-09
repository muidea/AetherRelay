package generation

import (
	"testing"
	"time"
)

func TestClockIgnoresTTFTAndFreezesAtTerminal(t *testing.T) {
	for _, protocol := range []struct{ name, prelude, output, terminal string }{
		{"responses", `{"type":"response.created"}`, `{"type":"response.output_text.delta","delta":"hello"}`, `{"type":"response.completed"}`},
		{"openai", `{"choices":[{"delta":{"role":"assistant"}}]}`, `{"choices":[{"delta":{"content":"hello"}}]}`, `[DONE]`},
		{"anthropic", `{"type":"message_start"}`, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`, `{"type":"message_stop"}`},
	} {
		t.Run(protocol.name, func(t *testing.T) {
			var c Clock
			at := time.Now()
			c.ObserveSSE([]byte("data: "+protocol.prelude), at)
			c.ObserveSSE([]byte(": heartbeat"), at.Add(5*time.Second))
			if s := c.Snapshot(at.Add(9 * time.Second)); !s.FirstOutputAt.IsZero() {
				t.Fatal(s)
			}
			c.ObserveSSE([]byte("data: "+protocol.output), at.Add(10*time.Second))
			c.ObserveSSE([]byte("data: "+protocol.terminal), at.Add(14*time.Second))
			s := c.Snapshot(at.Add(time.Minute))
			if s.Duration != 4*time.Second || s.Partial || !s.FirstOutputAt.Equal(at.Add(10*time.Second)) {
				t.Fatal(s)
			}
			c.Reset()
			if s := c.Snapshot(at.Add(time.Hour)); !s.FirstOutputAt.IsZero() {
				t.Fatal("retry retained sample", s)
			}
		})
	}
}
func TestReasoningToolsAndPartialOutput(t *testing.T) {
	for _, output := range []string{
		`{"type":"response.reasoning_summary_text.delta","delta":"reason"}`,
		`{"type":"response.function_call_arguments.delta","delta":"{}"}`,
		`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}`,
		`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"reason"}}`,
		`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{}"}}`,
	} {
		var c Clock
		at := time.Now()
		c.ObserveSSE([]byte("data: "+output), at)
		c.ObserveSSE([]byte(`data: {"type":"response.incomplete"}`), at.Add(time.Second))
		if s := c.Snapshot(at.Add(time.Hour)); s.Duration != time.Second || !s.Partial {
			t.Fatalf("%s: %+v", output, s)
		}
	}
	var c Clock
	at := time.Now()
	c.Output(at)
	c.Stop(at.Add(time.Second))
	if s := c.Snapshot(at.Add(time.Hour)); s.Duration != time.Second || !s.Partial {
		t.Fatal(s)
	}
}
func TestSinglePacketOutputHasUnknownDuration(t *testing.T) {
	var c Clock
	at := time.Now()
	c.ObserveSSE([]byte(`data: {"type":"response.output_text.delta","delta":"hello"}{"type":"response.completed"}`), at)
	if s := c.Snapshot(at.Add(time.Second)); s.Duration != 0 || !s.FirstOutputAt.IsZero() {
		t.Fatal(s)
	}
}
