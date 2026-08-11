package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

type searchDoer struct {
	prepare     *http.Request
	prepareBody string
	run         *http.Request
	runBody     string
	poll        *http.Request
}

func jsonStringField(t *testing.T, body, field string) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode %s payload: %v", field, err)
	}
	value, ok := payload[field].(string)
	if !ok {
		t.Fatalf("%s field missing or not string in %s", field, body)
	}
	return value
}

func (d *searchDoer) Do(request *http.Request) (*http.Response, error) {
	body := []byte(nil)
	if request.Body != nil {
		var err error
		body, err = io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
	}
	switch request.URL.Path {
	case "/":
		return textResponse(`<html></html>`), nil
	case "/backend-api/f/conversation/prepare":
		d.prepare, d.prepareBody = request, string(body)
		return textResponse(`{"conduit_token":"conduit-1"}`), nil
	case "/backend-api/sentinel/chat-requirements/prepare":
		return textResponse(`{"prepare_token":"prepare"}`), nil
	case "/backend-api/sentinel/chat-requirements/finalize":
		return textResponse(`{"token":"requirements","so_token":"so"}`), nil
	case "/backend-api/f/conversation":
		d.run, d.runBody = request, string(body)
		return textResponse("data: {\"conversation_id\":\"search-1\"}\n\ndata: [DONE]\n\n"), nil
	case "/backend-api/conversation/search-1":
		d.poll = request
		return textResponse(`{"mapping":{"node":{"message":{"id":"assistant-1","create_time":1,"author":{"role":"assistant"},"content":{"parts":["Current answer \ue200cite\ue201"]},"metadata":{"model_slug":"gpt-5-search","finish_details":{"type":"stop"},"sources":[{"title":"Example","url":"https://example.test/a","snippet":"Source excerpt"}]}}}}}`), nil
	default:
		return nil, fmt.Errorf("unexpected path %s", request.URL.Path)
	}
}

func TestSearchUsesPrepareRequirementsForcedConversationAndBoundedPoll(t *testing.T) {
	doer := &searchDoer{}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", doer)
	result, err := client.Search(context.Background(), SearchRequest{Model: "gpt-5", Query: "latest news"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ConversationID != "search-1" || result.ActualModel != "gpt-5-search" || !strings.Contains(result.Text, "Current answer") || len(result.Sources) != 1 || result.Sources[0].URL != "https://example.test/a" {
		t.Fatalf("result=%+v", result)
	}
	if doer.prepare == nil || doer.prepare.Header.Get("X-Conduit-Token") != "no-token" || doer.prepare.Header.Get("Openai-Sentinel-Chat-Requirements-Token") != "" || !strings.Contains(doer.prepareBody, `"system_hints":["search"]`) || !strings.Contains(doer.prepareBody, "latest news") {
		t.Fatalf("prepare headers=%v body=%s", doer.prepare.Header, doer.prepareBody)
	}
	prepareRoot := jsonStringField(t, doer.prepareBody, "parent_message_id")
	if prepareRoot == "" || strings.Contains(doer.prepareBody, `"parent_message_id":"client-created-root"`) {
		t.Fatalf("search prepare reused a static root: %s", doer.prepareBody)
	}
	var preparePayload map[string]any
	if err := json.Unmarshal([]byte(doer.prepareBody), &preparePayload); err != nil {
		t.Fatal(err)
	}
	if disabled, ok := preparePayload["history_and_training_disabled"].(bool); !ok || !disabled {
		t.Fatalf("search prepare did not disable history/training: %s", doer.prepareBody)
	}
	if doer.run == nil || doer.run.Header.Get("X-Conduit-Token") != "conduit-1" || doer.run.Header.Get("Openai-Sentinel-Chat-Requirements-Token") != "requirements" || doer.run.Header.Get("Openai-Sentinel-So-Token") != "so" || !strings.Contains(doer.runBody, `"force_use_search":true`) {
		t.Fatalf("run headers=%v body=%s", doer.run.Header, doer.runBody)
	}
	runRoot := jsonStringField(t, doer.runBody, "parent_message_id")
	if runRoot != prepareRoot {
		t.Fatalf("prepare/start roots differ: prepare=%q run=%q", prepareRoot, runRoot)
	}
	var runPayload map[string]any
	if err := json.Unmarshal([]byte(doer.runBody), &runPayload); err != nil {
		t.Fatal(err)
	}
	if disabled, ok := runPayload["history_and_training_disabled"].(bool); !ok || !disabled {
		t.Fatalf("search start did not disable history/training: %s", doer.runBody)
	}
	if doer.poll == nil || doer.poll.Header.Get("Authorization") != "Bearer token" {
		t.Fatalf("poll request=%v", doer.poll)
	}
}

func TestSearchUsesDifferentRootForEachRequest(t *testing.T) {
	first, second := &searchDoer{}, &searchDoer{}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", &multiSearchDoer{doers: []*searchDoer{first, second}})
	if _, err := client.Search(context.Background(), SearchRequest{Model: "gpt-5", Query: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Search(context.Background(), SearchRequest{Model: "gpt-5", Query: "second"}); err != nil {
		t.Fatal(err)
	}
	firstRoot := jsonStringField(t, first.prepareBody, "parent_message_id")
	secondRoot := jsonStringField(t, second.prepareBody, "parent_message_id")
	if firstRoot == secondRoot {
		t.Fatalf("separate searches reused root %q", firstRoot)
	}
}

func TestSearchRetriesPrepareWithFreshClient(t *testing.T) {
	first := &prepareEOFDoer{}
	second := &searchDoer{}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", first)
	created := 0
	client.newSearchClient = func() (*Client, error) {
		created++
		return newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", second), nil
	}

	result, err := client.Search(context.Background(), SearchRequest{Model: "gpt-5", Query: "retry once"})
	if err != nil || result.Text == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if first.prepareCalls != 1 || created != 1 || second.prepare == nil {
		t.Fatalf("first_prepare=%d fresh_clients=%d second_prepare=%v", first.prepareCalls, created, second.prepare != nil)
	}
}

func TestSearchDoesNotRetryAfterConversationStart(t *testing.T) {
	doer := &conversationEOFDoer{searchDoer: searchDoer{}}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", doer)
	created := 0
	client.newSearchClient = func() (*Client, error) {
		created++
		return newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", &searchDoer{}), nil
	}

	_, err := client.Search(context.Background(), SearchRequest{Model: "gpt-5", Query: "do not duplicate"})
	if err == nil || created != 0 || doer.conversationCalls != 1 {
		t.Fatalf("err=%v fresh_clients=%d conversation_calls=%d", err, created, doer.conversationCalls)
	}
}

type prepareEOFDoer struct{ prepareCalls int }

func (d *prepareEOFDoer) Do(request *http.Request) (*http.Response, error) {
	if request.URL.Path == "/backend-api/f/conversation/prepare" {
		d.prepareCalls++
		return nil, io.EOF
	}
	return nil, fmt.Errorf("unexpected request before prepare retry: %s", request.URL.Path)
}

type conversationEOFDoer struct {
	searchDoer
	conversationCalls int
}

func (d *conversationEOFDoer) Do(request *http.Request) (*http.Response, error) {
	if request.URL.Path == "/backend-api/f/conversation" {
		d.conversationCalls++
		return nil, io.EOF
	}
	return d.searchDoer.Do(request)
}

type multiSearchDoer struct {
	doers []*searchDoer
	index int
}

func (d *multiSearchDoer) Do(request *http.Request) (*http.Response, error) {
	if d.index >= len(d.doers) {
		return nil, fmt.Errorf("unexpected extra search request %s", request.URL.Path)
	}
	// Each Search performs the same deterministic sequence of setup calls. A
	// small routing wrapper keeps the test focused on the generated roots.
	current := d.doers[d.index]
	response, err := current.Do(request)
	if request.URL.Path == "/backend-api/conversation/search-1" {
		d.index++
	}
	return response, err
}

func TestParseSearchDocumentDeduplicatesAndBoundsSources(t *testing.T) {
	sources := make([]string, 0, maxSearchSourceCount+2)
	for i := 0; i < maxSearchSourceCount+2; i++ {
		sources = append(sources, fmt.Sprintf(`{"url":"https://example.test/%d","title":"%s"}`, i, strings.Repeat("x", 800)))
	}
	document := []byte(`{"mapping":{"node":{"message":{"author":{"role":"assistant"},"content":{"parts":["ok"]},"metadata":{"finish_details":{"type":"stop"},"sources":[` + strings.Join(sources, ",") + `]}}}}}`)
	result, terminal, err := parseSearchDocument("conversation", document)
	if err != nil || !terminal || len(result.Sources) != maxSearchSourceCount {
		t.Fatalf("result=%+v terminal=%v err=%v", result, terminal, err)
	}
	if len(result.Sources[0].Title) != 512 {
		t.Fatalf("title was not bounded: %d", len(result.Sources[0].Title))
	}
}

func TestParseSearchDocumentKeepsAnswerWhenNewerAssistantNodeIsEmpty(t *testing.T) {
	document := []byte(`{"mapping":{
		"answer":{"message":{"create_time":2,"author":{"role":"assistant"},"content":{"parts":["answer text"]},"metadata":{"status":"completed"}}},
		"bookkeeping":{"message":{"create_time":3,"author":{"role":"assistant"},"content":{"parts":[]},"metadata":{}}}
	}}`)
	result, terminal, err := parseSearchDocument("conversation", document)
	if err != nil || !terminal || result.Text != "answer text" {
		t.Fatalf("result=%+v terminal=%v err=%v", result, terminal, err)
	}
}

func TestParseSearchDocumentDoesNotTreatSearchInvocationAsAnswer(t *testing.T) {
	document := []byte(`{"mapping":{"tool":{"message":{"create_time":3,"author":{"role":"assistant"},"content":{"parts":["search(\"latest news\")"]},"metadata":{"model_slug":"gpt-5-search","status":"completed"}}}}}`)
	result, terminal, err := parseSearchDocument("conversation", document)
	if err != nil {
		t.Fatal(err)
	}
	if terminal || result.Text != "" {
		t.Fatalf("search invocation was surfaced as answer: result=%+v terminal=%v", result, terminal)
	}
}

func TestParseSearchDocumentPrefersAnswerOverNewerSearchInvocation(t *testing.T) {
	document := []byte(`{"mapping":{
		"answer":{"message":{"create_time":2,"author":{"role":"assistant"},"content":{"parts":["final answer"]},"metadata":{"model_slug":"gpt-5-search","status":"completed"}}},
		"tool":{"message":{"create_time":3,"author":{"role":"assistant"},"content":{"parts":["search(\"latest news\")"]},"metadata":{"model_slug":"gpt-5-search","status":"completed"}}}
	}}`)
	result, terminal, err := parseSearchDocument("conversation", document)
	if err != nil || !terminal || result.Text != "final answer" {
		t.Fatalf("search invocation hid answer: result=%+v terminal=%v err=%v", result, terminal, err)
	}
}

func TestIsSearchInvocationPlaceholder(t *testing.T) {
	if !isSearchInvocationPlaceholder(`search("为我搜索\\n最新消息")`) {
		t.Fatal("expected escaped search invocation to be recognized")
	}
	if isSearchInvocationPlaceholder(`search(latest news)`) {
		t.Fatal("unquoted search text must not be treated as an invocation placeholder")
	}
}
