package client

import (
	"context"
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
	if doer.run == nil || doer.run.Header.Get("X-Conduit-Token") != "conduit-1" || doer.run.Header.Get("Openai-Sentinel-Chat-Requirements-Token") != "requirements" || doer.run.Header.Get("Openai-Sentinel-So-Token") != "so" || !strings.Contains(doer.runBody, `"force_use_search":true`) {
		t.Fatalf("run headers=%v body=%s", doer.run.Header, doer.runBody)
	}
	if doer.poll == nil || doer.poll.Header.Get("Authorization") != "Bearer token" {
		t.Fatalf("poll request=%v", doer.poll)
	}
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
