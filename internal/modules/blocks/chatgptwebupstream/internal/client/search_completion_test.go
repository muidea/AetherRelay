package client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	completion "aetherrelay/internal/pkg/aetherrelaycompletion"
	http "github.com/bogdanfinn/fhttp"
)

func searchDocument(text string, metadata map[string]any) []byte {
	raw, _ := json.Marshal(map[string]any{"mapping": map[string]any{"answer": map[string]any{"message": map[string]any{"author": map[string]any{"role": "assistant"}, "content": map[string]any{"parts": []any{text}}, "metadata": metadata}}}})
	return raw
}

func TestSearchCompletionDistinguishesTerminalReasons(t *testing.T) {
	for _, tc := range []struct {
		terminal                         string
		completed, incomplete, wantError bool
	}{
		{"stop", true, false, false}, {"completed", true, false, false}, {"finished_successfully", true, false, false},
		{"length", false, true, false}, {"max_tokens", false, true, false}, {"in_progress", false, false, false},
		{"unknown_finish", false, false, true}, {"error", false, false, true},
	} {
		t.Run(tc.terminal, func(t *testing.T) {
			metadata := map[string]any{"finish_details": map[string]any{"type": tc.terminal}}
			result, terminal, err := parseSearchDocument("conversation", searchDocument("answer", metadata))
			if (err != nil) != tc.wantError || result.Completion.Valid(result.Text) != tc.completed || result.Completion.Incomplete(result.Text) != tc.incomplete || terminal != (tc.completed || tc.incomplete) {
				t.Fatalf("result=%+v terminal=%v err=%v", result, terminal, err)
			}
		})
	}
}

type completionSearchDoer struct {
	calls    int
	cancel   context.CancelFunc
	finishAt int
}

func (d *completionSearchDoer) Do(*http.Request) (*http.Response, error) {
	d.calls++
	metadata := map[string]any{"status": "in_progress"}
	if d.calls == d.finishAt {
		metadata["status"] = "completed"
	}
	if d.calls == 4 && d.cancel != nil {
		d.cancel()
	}
	return textResponse(string(searchDocument("unchanged partial answer", metadata))), nil
}

func TestSearchPollDoesNotCompleteStableSnapshots(t *testing.T) {
	for _, finish := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		doer := &completionSearchDoer{cancel: cancel}
		if finish {
			doer.finishAt = 4
			doer.cancel = nil
		}
		c := newWithDoer(Config{AccessToken: "test"}, "https://chatgpt.com", doer)
		result, err := c.pollSearchWithPolicy(ctx, "conversation", time.Second, time.Millisecond)
		if doer.calls != 4 || result.Text != "unchanged partial answer" || (err == nil) != finish || result.Completion.Valid(result.Text) != finish {
			t.Fatalf("finish=%v calls=%d result=%+v err=%v", finish, doer.calls, result, err)
		}
	}
}

func TestSearchPollTimeoutKeepsPartialWithoutSuccess(t *testing.T) {
	doer := &completionSearchDoer{}
	c := newWithDoer(Config{AccessToken: "test"}, "https://chatgpt.com", doer)
	result, err := c.pollSearchWithPolicy(context.Background(), "conversation", 10*time.Millisecond, time.Millisecond)
	var failure *Error
	if !errors.As(err, &failure) || failure.Class != Timeout || result.Text == "" || result.Completion.Valid(result.Text) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestSearchDocumentRejectsOutputBudgetWithoutTruncating(t *testing.T) {
	result, terminal, err := parseSearchDocument("conversation", searchDocument(strings.Repeat("x", maxSearchTextBytes+1), map[string]any{"status": "completed"}))
	var failure *textCompletionError
	if !errors.As(err, &failure) || failure.reason != completion.OutputLimit || terminal || result.Text != "" {
		t.Fatalf("textbytes=%d terminal=%v err=%v", len(result.Text), terminal, err)
	}
}

func TestSearchDocumentRejectsMalformedTerminalEvidence(t *testing.T) {
	for _, finish := range []any{true, "stop", map[string]any{"type": []any{"stop"}}} {
		result, terminal, err := parseSearchDocument("conversation", searchDocument("partial", map[string]any{"status": "completed", "finish_details": finish}))
		var failure *textCompletionError
		if !errors.As(err, &failure) || failure.reason != completion.Protocol || terminal || result.Text != "partial" {
			t.Fatalf("finish=%v result=%+v terminal=%v err=%v", finish, result, terminal, err)
		}
	}
}

func TestSearchDocumentFinishCannotOverridePendingOrFailedStatus(t *testing.T) {
	for _, status := range []string{"in_progress", "incomplete", "error", "unknown"} {
		result, terminal, err := parseSearchDocument("conversation", searchDocument("partial", map[string]any{"status": status, "finish_details": map[string]any{"type": "stop"}}))
		wantError := status == "error" || status == "unknown"
		if (err != nil) != wantError || terminal || result.Completion.Valid(result.Text) || result.Text != "partial" {
			t.Fatalf("status=%s result=%+v terminal=%v err=%v", status, result, terminal, err)
		}
	}
}
