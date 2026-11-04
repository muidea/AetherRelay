package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	completion "aetherrelay/internal/pkg/aetherrelaycompletion"
)

func textSnapshot(text string) string {
	data, _ := json.Marshal(map[string]any{"conversation_id": "c", "message": map[string]any{"id": "a", "author": map[string]any{"role": "assistant"}, "content": map[string]any{"content_type": "text", "parts": []string{text}}}})
	return "data: " + string(data) + "\n\n"
}

func TestTextCompletionProtocolAndSnapshots(t *testing.T) {
	const done = "data: [DONE]\n\n"
	first := textSnapshot("AB")
	for _, tc := range []struct {
		name, stream, want string
		reason             completion.Reason
	}{
		{"done", first + done, "AB", completion.Stop},
		{"no_final_blank", first + "data: [DONE]", "AB", completion.Stop},
		{"no_final_blank_line", first + "data: [DONE]\n", "AB", completion.Stop},
		{"eof", first, "AB", completion.EOF},
		{"malformed_final", first + "data: {\"message\":\n\n" + done, "AB", completion.Protocol},
		{"typed_part", first + strings.Replace(textSnapshot("ABC"), `["ABC"]`, `[{"text":"ABC"}]`, 1) + done, "AB", completion.Protocol},
		{"null_part", first + strings.Replace(textSnapshot("ABC"), `["ABC"]`, `[null]`, 1) + done, "AB", completion.Protocol},
		{"unknown_patch", first + "data: {\"p\":\"message/content/parts/0\",\"v\":\"ABC\"}\n\n" + done, "AB", completion.Protocol},
		{"metadata", first + "data: {\"type\":\"ping\"}\n\n" + textSnapshot("ABC") + done, "ABC", completion.Stop},
		{"native_length", strings.Replace(first, `"content":`, `"metadata":{"finish_details":{"type":"max_tokens"}},"content":`, 1) + done, "AB", completion.ModelLength},
		{"repeat", first + first + textSnapshot("ABC") + done, "ABC", completion.Stop},
		{"rewrite", first + textSnapshot("AC") + done, "AB", completion.Snapshot},
		{"shrink", first + textSnapshot("A") + done, "AB", completion.Snapshot},
		{"empty", first + textSnapshot("") + done, "AB", completion.Snapshot},
		{"message_change", first + strings.Replace(textSnapshot("ABC"), `"id":"a"`, `"id":"b"`, 1) + done, "AB", completion.Snapshot},
		{"conversation_change", first + strings.Replace(textSnapshot("ABC"), `"conversation_id":"c"`, `"conversation_id":"d"`, 1) + done, "AB", completion.Snapshot},
		{"channel_change", first + strings.Replace(textSnapshot("ABC"), `"id":"a"`, `"id":"a","channel":"final"`, 1) + done, "AB", completion.Snapshot},
		{"invalid_business_json", textSnapshot(`{"a":`) + done, `{"a":`, completion.Stop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var emitted strings.Builder
			result, err := parseTextSSE(context.Background(), strings.NewReader(tc.stream), func(delta TextDelta) error { emitted.WriteString(delta.Text); return nil })
			if result.Text != tc.want || emitted.String() != tc.want || result.Completion.Reason != tc.reason || result.Completion.TextSHA256 != completion.Hash(tc.want) || result.Completion.SSESHA256 != completion.Hash(tc.stream) {
				t.Fatalf("result=%+v emitted=%q err=%v", result, emitted.String(), err)
			}
			success := tc.reason == completion.Stop
			if (err == nil) != success || result.Done != success || result.Completion.Completed != success || success && !result.Completion.Valid(emitted.String()) {
				t.Fatalf("invalid terminal result=%+v err=%v", result, err)
			}
		})
	}
}

func TestTextExactBudgets(t *testing.T) {
	stream := textSnapshot("AB") + "data: [DONE]\n\n"
	for _, offset := range []int{-1, 0, 1} {
		t.Run(fmt.Sprintf("wire/%d", offset), func(t *testing.T) {
			limits := defaultTextLimits
			limits.total = int64(len(stream) + offset)
			result, err := parseTextSSELimited(context.Background(), strings.NewReader(stream), nil, nil, limits)
			if offset < 0 {
				if err == nil || result.Done || result.Completion.Reason != completion.SSELimit || result.Completion.Limit != limits.total {
					t.Fatal(result, err)
				}
			} else if err != nil || !result.Completion.Valid("AB") {
				t.Fatal(result, err)
			}
		})
	}
	for _, kind := range []string{"content", "output", "line", "event"} {
		for _, offset := range []int{-1, 0, 1} {
			t.Run(fmt.Sprintf("%s/%d", kind, offset), func(t *testing.T) {
				limits := defaultTextLimits
				input := stream
				var want completion.Reason
				switch kind {
				case "content":
					limits.content = 2 + offset
					want = completion.ContentLimit
				case "output":
					limits.output = 2 + offset
					want = completion.OutputLimit
				case "line":
					limits.line = 512 + offset
					input = ":" + strings.Repeat("x", 511) + "\n" + stream
					want = completion.LineLimit
				case "event":
					// Split JSON across data lines at a legal whitespace boundary. Neither
					// single line exceeds its limit; the aggregated event is bounded too.
					payload := strings.TrimSuffix(strings.TrimPrefix(textSnapshot("AB"), "data: "), "\n\n")
					split := strings.Index(payload, ",") + 1
					input = "data: " + payload[:split] + "\ndata: " + payload[split:] + "\n\n" + "data: [DONE]\n\n"
					limits.event = len(payload) + 2 + offset
					want = completion.EventLimit
				}
				result, err := parseTextSSELimited(context.Background(), strings.NewReader(input), nil, nil, limits)
				if offset < 0 {
					if err == nil || result.Done || result.Completion.Reason != want {
						t.Fatal(result, err)
					}
				} else if err != nil || !result.Completion.Valid("AB") {
					t.Fatal(result, err)
				}
			})
		}
	}
}

func TestTextReadCancelAndEmitErrorsKeepPartial(t *testing.T) {
	first := textSnapshot("AB")
	ctx, cancel := context.WithCancel(context.Background())
	result, err := parseTextSSE(ctx, strings.NewReader(first+"data: [DONE]\n\n"), func(TextDelta) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) || result.Text != "AB" || result.Done || result.Completion.Reason != completion.Canceled {
		t.Fatal(result, err)
	}
	result, err = parseTextSSE(context.Background(), io.MultiReader(strings.NewReader(first), generationTextReadError{}), nil)
	if err == nil || result.Text != "AB" || result.Done || result.Completion.Reason != completion.ReadError {
		t.Fatal(result, err)
	}
	sentinel := errors.New("sink closed")
	result, err = parseTextSSE(context.Background(), strings.NewReader(first+"data: [DONE]\n\n"), func(TextDelta) error { return sentinel })
	if !errors.Is(err, sentinel) || result.Text != "AB" || result.Done || result.Completion.Reason != completion.EmitError {
		t.Fatal(result, err)
	}
}

func growingSnapshots(size, step int) string {
	var stream strings.Builder
	for n := step; n < size; n += step {
		stream.WriteString(textSnapshot(strings.Repeat("x", n)))
	}
	stream.WriteString(textSnapshot(strings.Repeat("x", size)))
	stream.WriteString("data: [DONE]\n\n")
	return stream.String()
}

func TestLongSnapshotTransportBudget(t *testing.T) {
	for _, step := range []int{128, 16} {
		stream := growingSnapshots(36<<10, step)
		var emitted strings.Builder
		result, err := parseTextSSE(context.Background(), strings.NewReader(stream), func(d TextDelta) error { emitted.WriteString(d.Text); return nil })
		if step == 128 {
			if err != nil || !result.Completion.Valid(emitted.String()) || emitted.Len() != 36<<10 {
				t.Fatal(result.Completion, err)
			}
		} else if len(stream) <= maxTextSSEBytes || err == nil || result.Done || result.Completion.Reason != completion.SSELimit {
			t.Fatal(len(stream), result.Completion, err)
		}
	}
}

func BenchmarkTextGrowingSnapshots(b *testing.B) {
	stream := growingSnapshots(36<<10, 128)
	b.ReportAllocs()
	b.SetBytes(int64(len(stream)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := ParseTextSSE(context.Background(), strings.NewReader(stream))
		if err != nil || !result.Completion.Valid(result.Text) {
			b.Fatal(result.Completion, err)
		}
	}
}

func TestTextUTF8BudgetsAndConcurrentIsolation(t *testing.T) {
	limits := defaultTextLimits
	limits.content = 4
	limits.output = 4
	for _, text := range []string{"你a", "你好"} {
		var emitted strings.Builder
		result, err := parseTextSSELimited(context.Background(), strings.NewReader(textSnapshot("你")+textSnapshot(text)+"data: [DONE]\n\n"), func(d TextDelta) error { emitted.WriteString(d.Text); return nil }, nil, limits)
		if text == "你a" {
			if err != nil || !result.Completion.Valid(emitted.String()) {
				t.Fatal(result, err)
			}
		} else if err == nil || result.Text != "你" || emitted.String() != "你" || result.Completion.Reason != completion.ContentLimit {
			t.Fatal(result, err, emitted.String())
		}
	}
	for n := 0; n < 24; n++ {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			t.Parallel()
			text := fmt.Sprintf("request-%d", n)
			wire := textSnapshot(text) + "data: [DONE]\n\n"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			emitted := ""
			result, err := parseTextSSE(ctx, strings.NewReader(wire), func(d TextDelta) error {
				emitted += d.Text
				if n%2 == 0 {
					cancel()
				}
				return nil
			})
			if result.Text != text || emitted != text || result.Completion.TextSHA256 != completion.Hash(text) {
				t.Fatal(result, emitted, err)
			}
			if n%2 == 0 {
				if !errors.Is(err, context.Canceled) || result.Done {
					t.Fatal(result, err)
				}
			} else if err != nil || !result.Completion.Valid(text) {
				t.Fatal(result, err)
			}
		})
	}
}

func TestTextInvalidRequestKeepsUnsentEvidence(t *testing.T) {
	doer := &textDoer{}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", doer)
	messages := make([]TextMessage, 65)
	for i := range messages {
		messages[i] = TextMessage{Role: "user", Content: "hi"}
	}
	result, err := client.CompleteText(context.Background(), TextRequest{Messages: messages})
	var failure *Error
	if !errors.As(err, &failure) || failure.Class != InvalidRequest || result.ErrorOperation != "text_validate" || !result.GenerationNotSent || doer.conversationRequest != nil {
		t.Fatal(result, err)
	}
}
