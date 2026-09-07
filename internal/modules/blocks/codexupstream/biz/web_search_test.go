package biz

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
)

const searchItem = `{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"example","sources":[{"type":"url","url":"https://example.com"}]}}`
const searchAnswer = `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Example","annotations":[{"type":"url_citation","url":"https://example.com","start_index":0,"end_index":7}]}]}`

func TestWebSearchStreamSemantics(t *testing.T) {
	for _, tc := range []struct {
		payload string
		want    bool
	}{
		{`{"type":"response.web_search_call.in_progress","item_id":"ws_1"}`, false},
		{`{"type":"response.web_search_call.searching","item_id":"ws_1"}`, true},
		{`{"type":"response.web_search_call.completed","item_id":"ws_1"}`, true},
		{`{"type":"response.output_item.added","item":{"type":"web_search_call","action":{"type":"search","queries":["example"]}}}`, true},
		{`{"type":"response.output_item.added","item":{"type":"web_search_call","action":{"type":"open_page","url":"https://example.com"}}}`, true},
		{`{"type":"response.output_item.added","item":{"type":"web_search_call","action":{"type":"find_in_page","pattern":"example"}}}`, true},
		{`{"type":"response.output_item.added","item":{"type":"web_search_call","action":{"type":"search","sources":[{"type":"url","url":"https://example.com"}]}}}`, true},
		{`{"type":"response.output_item.added","item":{"type":"web_search_call","action":{"type":"search","extension":true}}}`, false},
		{`{"type":"response.output_item.added","item":{"type":"web_search_call","status":"in_progress","action":{"type":"search","queries":[]}}}`, false},
		{`{"type":"response.output_item.added","item":{"type":"web_search_call","status":"in_progress","action":{"type":"search","query":"example"}}}`, true},
		{`{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_1","status":"completed"}}`, true},
		{`{"type":"response.output_item.done","item":` + searchItem + `}`, true},
	} {
		got, _ := codexStreamSemantics([]byte("data: "+tc.payload), false)
		if got != tc.want {
			t.Fatalf("semantic=%t want=%t payload=%s", got, tc.want, tc.payload)
		}
	}
}

func TestWebSearchStreamRequiresTerminalAndPreservesErrors(t *testing.T) {
	for _, tc := range []struct {
		name, terminal string
		class          events.ErrorClass
	}{
		{"complete", `{"type":"response.completed","response":{"status":"completed","output":[` + searchItem + `,` + searchAnswer + `]}}`, ""},
		{"incomplete", `{"type":"response.incomplete","response":{"status":"incomplete","output":[` + searchItem + `]}}`, ""},
		{"quota", `{"type":"error","error":{"type":"usage_limit_reached"}}`, events.ErrorRateLimit},
		{"invalid", `{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"unsupported_tool"}}}`, events.ErrorInvalidRequest},
		{"eof", "", events.ErrorProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "data: {\"type\":\"response.web_search_call.in_progress\",\"item_id\":\"ws_1\"}\n\n" +
				"data: {\"type\":\"response.web_search_call.searching\",\"item_id\":\"ws_1\"}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":" + searchItem + "}\n\n"
			if tc.terminal != "" {
				body += "data: " + tc.terminal + "\n"
			} // terminal delimiter must be completed by proxy
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream := &responseStream{cancel: cancel, updates: make(chan streamUpdate, 32)}
			upstream := &Upstream{streams: map[string]*responseStream{"search": stream}}
			upstream.runStream(ctx, "search", stream, io.NopCloser(strings.NewReader(body)), 4096)
			var output bytes.Buffer
			var terminal streamUpdate
			for update := range stream.updates {
				output.Write(update.data)
				if update.done {
					terminal = update
				}
			}
			if !terminal.done || terminal.errorClass != tc.class {
				t.Fatalf("terminal=%+v want=%s", terminal, tc.class)
			}
			// Only blank-line-dispatched SSE events count as received by clients.
			scanner := bufio.NewScanner(bytes.NewReader(output.Bytes()))
			pending := ""
			var received []string
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "data: ") {
					pending = strings.TrimPrefix(line, "data: ")
				}
				if line == "" && pending != "" {
					received = append(received, pending)
					pending = ""
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			wantCount := 3
			if tc.terminal != "" {
				wantCount++
			}
			if pending != "" || len(received) != wantCount || !strings.Contains(received[1], "searching") || !strings.Contains(received[2], "sources") {
				t.Fatalf("received=%v pending=%s", received, pending)
			}
			if tc.terminal != "" && received[len(received)-1] != tc.terminal {
				t.Fatalf("terminal changed: %v", received)
			}
		})
	}
}

func TestWebSearchBufferedResponsePreservesCallsAndCitations(t *testing.T) {
	for _, terminalOutput := range []string{`[]`, `[` + searchAnswer + `]`, `[` + searchItem + `,` + searchAnswer + `]`} {
		for _, kind := range []string{"completed", "incomplete"} {
			t.Run(kind+"/"+terminalOutput, func(t *testing.T) {
				body := "data: {\"type\":\"response.web_search_call.searching\"}\n\n" +
					"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + searchItem + "}\n\n" +
					"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":" + searchAnswer + "}\n\n" +
					"data: {\"type\":\"response." + kind + "\",\"response\":{\"object\":\"response\",\"status\":\"" + kind + "\",\"output\":" + terminalOutput + ",\"usage\":{\"input_tokens\":100,\"output_tokens\":10}}}\n\n"
				response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
				payload, class, _, _, err := completedResponse(response, 1<<20)
				if err != nil || class != "" {
					t.Fatalf("err=%v class=%s", err, class)
				}
				var got struct {
					Status string            `json:"status"`
					Output []json.RawMessage `json:"output"`
				}
				if err := json.Unmarshal(payload, &got); err != nil {
					t.Fatal(err)
				}
				if got.Status != kind || len(got.Output) != 2 || !bytes.Contains(got.Output[0], []byte("sources")) || !bytes.Contains(got.Output[1], []byte("url_citation")) {
					t.Fatalf("payload=%s", payload)
				}
			})
		}
	}
}

func TestWebSearchBufferedResponseDoesNotInventTerminal(t *testing.T) {
	response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_item.done\",\"item\":" + searchItem + "}\n\n"))}
	_, class, _, _, err := completedResponse(response, 4096)
	if err == nil || class != events.ErrorProtocol {
		t.Fatalf("err=%v class=%s", err, class)
	}
}
