package client

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	generation "aetherrelay/internal/pkg/aetherrelaygeneration"
	http "github.com/bogdanfinn/fhttp"
)

const observedUsageLimit = `{"message":null,"conversation_id":"search-1","error":"你已达到限额。请稍后重试。","error_code":"usage_limit"}`

func requireSSEFailure(t *testing.T, err error, operation, code string, class ErrorClass) {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || failure.Operation != operation || failure.Class != class || failure.Code != code || failure.StatusCode != 0 {
		t.Fatalf("failure=%+v err=%v", failure, err)
	}
	if !strings.Contains(err.Error(), code) || strings.Contains(err.Error(), "<nil>") {
		t.Fatalf("lost SSE error details: %v", err)
	}
}

func TestSSEErrorEnvelopes(t *testing.T) {
	for _, tc := range []struct {
		name, payload, code, detail string
		class                       ErrorClass
	}{
		{"observed", observedUsageLimit, "usage_limit", "你已达到限额。请稍后重试。", RateLimit},
		{"nested", `{"error":{"code":"rate_limit_exceeded","message":"try later"}}`, "rate_limit_exceeded", "try later", RateLimit},
		{"typed", `{"type":"error","code":"quota_exceeded","message":"quota reached"}`, "quota_exceeded", "quota reached", RateLimit},
		{"delta", `{"p":"","o":"add","v":` + observedUsageLimit + `}`, "usage_limit", "你已达到限额。请稍后重试。", RateLimit},
		{"unknown", `{"error_code":"new_upstream_error","error":"specific upstream failure"}`, "new_upstream_error", "specific upstream failure", Upstream},
		{"auth_is_not_http_challenge", `{"error_code":"invalid_token","error":"session issue"}`, "invalid_token", "session issue", Upstream},
		{"code_only", `{"error_code":"usage_limit","error":null}`, "usage_limit", "upstream SSE error", RateLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := parseSSEError("text_conversation", tc.payload)
			requireSSEFailure(t, err, "text_conversation", tc.code, tc.class)
			if !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("lost upstream message: %v", err)
			}
		})
	}
	for _, payload := range []string{
		`{"error":null,"error_code":null,"conversation_id":"c"}`,
		`{"error":false}`, `{"error":""}`, `{"message":{"content":{"parts":["usage_limit"]},"error_code":"usage_limit"}}`,
		`{"p":"/message/content","o":"replace","v":{"error":"fictional","error_code":"usage_limit"}}`,
		`{"type":"server_metadata","code":"usage_limit"}`, `: keepalive`,
	} {
		if err := parseSSEError("text_conversation", payload); err != nil {
			t.Fatalf("ordinary content treated as failure: %s: %v", payload, err)
		}
	}
	err := parseSSEError("text_conversation", `{"error_code":"`+strings.Repeat("c", 256)+`","error":"`+strings.Repeat("m", 1024)+`"}`).(*Error)
	if len(err.Code) != 128 || len(err.Detail) != 512 {
		t.Fatalf("unbounded error: code=%d detail=%d", len(err.Code), len(err.Detail))
	}
}

func TestTextSSEUsageLimitPreservesPartialOutputAndSubmission(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_output", true: "after_output"}[partial], func(t *testing.T) {
			prefix := ""
			if partial {
				prefix = "data: {\"conversation_id\":\"search-1\",\"message\":{\"id\":\"assistant-1\",\"author\":{\"role\":\"assistant\"},\"content\":{\"parts\":[\"partial\"]}}}\n\n"
			}
			// No DONE/EOF is needed after an explicit failure; a further read
			// would produce a different error and fail this assertion.
			body := io.MultiReader(strings.NewReader(prefix+"data: "+observedUsageLimit+"\n\n"), generationTextReadError{})
			emitted := ""
			var frozen generation.Sample
			result, err := parseTextSSEObserved(context.Background(), body, func(d TextDelta) error { emitted += d.Text; return nil }, func(s generation.Sample) { frozen = s })
			requireSSEFailure(t, err, "text_conversation", "usage_limit", RateLimit)
			if result.ConversationID != "search-1" || result.Done {
				t.Fatalf("result=%+v", result)
			}
			if partial && (emitted != "partial" || result.Text != "partial" || result.AssistantMessageID != "assistant-1" || !result.Generation.Partial || result.Generation.FirstOutputAt.IsZero()) {
				t.Fatalf("partial state lost: %+v emitted=%q", result, emitted)
			}
			if !partial && (emitted != "" || result.Text != "" || !result.Generation.FirstOutputAt.IsZero()) {
				t.Fatalf("invented output: %+v", result)
			}
			if frozen != result.Generation {
				t.Fatal("generation observation changed during cleanup")
			}
		})
	}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", textBoundaryDoer{base: &textDoer{}, run: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/backend-api/conversation" {
			return textResponse("data: " + observedUsageLimit + "\n\n"), nil
		}
		return nil, nil
	}})
	out, err := client.CompleteText(context.Background(), TextRequest{Model: "gpt-6", Messages: []TextMessage{{Role: "user", Content: "hello"}}})
	requireSSEFailure(t, err, "text_conversation", "usage_limit", RateLimit)
	if out.UpstreamStatus != 200 || out.GenerationNotSent || out.ErrorOperation != "text_conversation" || out.ConversationID != "search-1" {
		t.Fatalf("submission evidence=%+v", out)
	}
}

func TestSearchSSEConsumesFailuresAfterConversationID(t *testing.T) {
	for _, prefix := range []string{"", "data: {\"conversation_id\":\"search-1\"}\n\n"} {
		id, err := parseSearchConversationID(context.Background(), io.MultiReader(strings.NewReader(prefix+"data: "+observedUsageLimit+"\n\n"), generationTextReadError{}))
		requireSSEFailure(t, err, "search_conversation", "usage_limit", RateLimit)
		if id != "search-1" {
			t.Fatalf("lost conversation: %s", id)
		}
	}
	stream := "data: {\"conversation_id\":\"search-1\",\"error\":null}\n\ndata: [DONE]\n\n"
	id, err := parseSearchConversationID(context.Background(), io.MultiReader(strings.NewReader(stream), generationTextReadError{}))
	if err != nil || id != "search-1" {
		t.Fatalf("normal stream: id=%s err=%v", id, err)
	}
	_, err = parseTextSSE(context.Background(), strings.NewReader("data: {\"type\":\"error\",\"code\":\"usage_limit\",\"message\":\"try later\"}"), nil)
	requireSSEFailure(t, err, "text_conversation", "usage_limit", RateLimit)
}

type sseSearchFailureDoer struct {
	searchDoer
	pollFailure          bool
	pollStatus           int
	polls, conversations int
}

func (d *sseSearchFailureDoer) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/backend-api/f/conversation" {
		d.conversations++
		if !d.pollFailure {
			return textResponse("data: {\"conversation_id\":\"search-1\"}\n\ndata: " + observedUsageLimit + "\n\n"), nil
		}
	}
	if req.URL.Path == "/backend-api/conversation/search-1" {
		d.polls++
		response := textResponse(`{"error":{"code":"usage_limit","message":"quota reached"}}`)
		response.StatusCode = d.pollStatus
		return response, nil
	}
	return d.searchDoer.Do(req)
}
func TestSearchExplicitLimitsDoNotPollOrResubmit(t *testing.T) {
	for _, status := range []int{0, 200, 429} {
		duringPoll := status != 0
		d := &sseSearchFailureDoer{pollFailure: duringPoll, pollStatus: status}
		c := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", d)
		out, err := c.Search(context.Background(), SearchRequest{Model: "gpt-6", Query: "search"})
		var failure *Error
		if !errors.As(err, &failure) || failure.Class != RateLimit || out.ConversationID != "search-1" || d.conversations != 1 {
			t.Fatalf("out=%+v err=%v calls=%d", out, err, d.conversations)
		}
		if !duringPoll {
			requireSSEFailure(t, err, "search_conversation", "usage_limit", RateLimit)
		}
		wantPolls := 0
		if duringPoll {
			wantPolls = 1
		}
		if d.polls != wantPolls {
			t.Fatalf("polls=%d want=%d", d.polls, wantPolls)
		}
	}
}

func TestImageSSELimitKeepsConversationAndStops(t *testing.T) {
	out, err := ParseImageSSE(io.MultiReader(strings.NewReader("data: {\"conversation_id\":\"search-1\",\"file_id\":\"file_partial\"}\n\ndata: "+observedUsageLimit+"\n\n"), generationTextReadError{}))
	requireSSEFailure(t, err, "image_conversation", "usage_limit", RateLimit)
	if out.ConversationID != "search-1" || len(out.FileIDs) != 1 || out.Done {
		t.Fatalf("partial image state lost: %+v", out)
	}
}

type sseImageFailureDoer struct {
	generationDoer
	conversations, polls int
}

func (d *sseImageFailureDoer) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/backend-api/f/conversation" {
		d.conversations++
		return textResponse("data: " + observedUsageLimit + "\n\n"), nil
	}
	if strings.HasPrefix(req.URL.Path, "/backend-api/conversation/") {
		d.polls++
	}
	return d.generationDoer.Do(req)
}

func TestImageGenerationSSELimitDoesNotPoll(t *testing.T) {
	d := &sseImageFailureDoer{}
	c := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", d)
	out, err := c.GenerateImage(context.Background(), ImageRequest{Model: "gpt-6", Prompt: "a circle"}, ImagePollOptions{})
	requireSSEFailure(t, err, "image_conversation", "usage_limit", RateLimit)
	if out.ConversationID != "search-1" || len(out.Images) != 0 || d.polls != 0 || d.conversations != 1 {
		t.Fatalf("out=%+v polls=%d submissions=%d", out, d.polls, d.conversations)
	}
}

type oversizedSearchDoer struct {
	searchDoer
	polls int
}

func (d *oversizedSearchDoer) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/backend-api/f/conversation" {
		return textResponse("data: {\"conversation_id\":\"search-1\"}\n\n" + strings.Repeat(": keepalive\n", maxSearchSSEBytes/12+100)), nil
	}
	if strings.HasPrefix(req.URL.Path, "/backend-api/conversation/") {
		d.polls++
	}
	return d.searchDoer.Do(req)
}

func TestSearchSSELimitCannotSilentlyBecomeSuccessfulSubmission(t *testing.T) {
	d := &oversizedSearchDoer{}
	c := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", d)
	out, err := c.Search(context.Background(), SearchRequest{Model: "gpt-6", Query: "search"})
	if err == nil || !strings.Contains(err.Error(), "response exceeds") || out.ConversationID != "search-1" || d.polls != 0 {
		t.Fatalf("out=%+v err=%v polls=%d", out, err, d.polls)
	}
}
