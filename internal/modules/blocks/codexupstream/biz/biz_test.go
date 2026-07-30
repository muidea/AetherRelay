package biz

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	events "ai-proxy/internal/modules/blocks/codexupstream/pkg/events"
)

func TestForceStreamPreservesNativeResponseFields(t *testing.T) {
	value, err := forceStream([]byte(`{"model":"gpt-5.2","stream":false,"tools":[{"type":"function"}],"metadata":{"tenant":"alpha"}}`))
	if err != nil {
		t.Fatal(err)
	}
	text := string(value)
	for _, required := range []string{`"stream":true`, `"tools"`, `"metadata"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("forced request lost native field %s: %s", required, text)
		}
	}
}

func TestCompletedResponseSupportsJSONAndSSE(t *testing.T) {
	jsonResponse := &http.Response{Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"response","id":"resp_1"}`))}
	value, class, err := completedResponse(jsonResponse, 1024)
	if err != nil || class != "" || string(value) != `{"object":"response","id":"resp_1"}` {
		t.Fatalf("json completed response = %s class=%s err=%v", value, class, err)
	}
	sseResponse := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"id\":\"resp_2\"}}\n\n"))}
	value, class, err = completedResponse(sseResponse, 1024)
	if err != nil || class != "" || !strings.Contains(string(value), `"resp_2"`) {
		t.Fatalf("sse completed response = %s class=%s err=%v", value, class, err)
	}
}

func TestTerminalClass(t *testing.T) {
	if done, class := terminalClass([]byte("data: {\"type\":\"response.completed\"}\n")); !done || class != "" {
		t.Fatalf("completed terminal = done=%v class=%q", done, class)
	}
	if done, class := terminalClass([]byte("data: {\"type\":\"response.failed\"}\n")); !done || class != events.ErrorUpstream {
		t.Fatalf("failed terminal = done=%v class=%q", done, class)
	}
}

func TestPerformUsesFixedCodexHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("User-Agent") != codexUserAgent || r.Header.Get("Originator") != codexOriginator {
			t.Fatalf("Codex identity headers user-agent=%q originator=%q", r.Header.Get("User-Agent"), r.Header.Get("Originator"))
		}
		if r.Header.Get("ChatGPT-Account-ID") != "chatgpt-account-id" || r.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("Codex account headers=%v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	previousURL := responsesURL
	responsesURL = server.URL
	t.Cleanup(func() { responsesURL = previousURL })
	response, class, _, err := perform(context.Background(), "access-token", "chatgpt-account-id", "", []byte(`{"model":"gpt-5.2-codex","stream":true}`))
	if err != nil || class != "" || response == nil {
		t.Fatalf("perform response=%v class=%q err=%v", response, class, err)
	}
	_ = response.Body.Close()
}
