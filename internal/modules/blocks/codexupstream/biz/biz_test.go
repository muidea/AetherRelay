package biz

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/gorilla/websocket"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestWebsocketSessionUsesVersionedIdentityAndBackgroundReader(t *testing.T) {
	headers := make(chan http.Header, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		conn, err := upgrader.Upgrade(w, r, http.Header{"X-Codex-Primary-Used-Percent": []string{"42"}})
		if err != nil {
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err == nil {
			_ = conn.WriteMessage(websocket.TextMessage, append([]byte(`{"type":"response.completed","echo":`), append(payload, '}')...))
		}
	}))
	defer server.Close()
	previous := responsesWebsocketURL
	responsesWebsocketURL = "ws" + strings.TrimPrefix(server.URL, "http")
	t.Cleanup(func() { responsesWebsocketURL = previous })
	hub := event.NewHub(32)
	background := task.NewBackgroundRoutine(8)
	upstream := New(hub, background)
	t.Cleanup(func() {
		upstream.Teardown(context.Background())
		background.Shutdown(context.Background())
		hub.Terminate(context.Background())
	})
	openResult := event.NewResult(events.TopicWSOpen, "test", "upstream")
	upstream.handleWSOpen(event.NewEventWithContext(events.TopicWSOpen, "test", "upstream", nil, context.Background(), events.WSOpenCommand{AccessToken: "secret-token", AccountIDHeader: "account-header", MaxMessageBytes: 1024, SessionHash: "session-hash", TurnState: "opaque-state", Fingerprint: events.CodexFingerprint{Mode: "device", InstallationID: "install-id"}}), openResult)
	value, cdErr := openResult.Get()
	if cdErr != nil {
		t.Fatal(cdErr)
	}
	opened := value.(events.WSOpenResult)
	if opened.SessionID == "" || opened.ErrorClass != "" {
		t.Fatalf("CP-WS-002 opened=%+v", opened)
	}
	if len(opened.Headers) == 0 {
		t.Fatalf("CP-WS-011 handshake headers=%+v", opened.Headers)
	}
	gotHeaders := <-headers
	if gotHeaders.Get("Authorization") != "Bearer secret-token" || gotHeaders.Get("ChatGPT-Account-ID") != "account-header" || gotHeaders.Get("OpenAI-Beta") != currentIdentity.WebsocketBeta || gotHeaders.Get("User-Agent") != currentIdentity.UserAgent || gotHeaders.Get("Originator") != currentIdentity.Originator {
		t.Fatalf("CP-HDR/CP-WS-002 headers=%v", gotHeaders)
	}
	assertCodexSessionHeaders(t, gotHeaders, "session-hash")
	if gotHeaders.Get("X-Codex-Installation-Id") != "install-id" || gotHeaders.Get("X-Codex-Turn-State") != "opaque-state" || gotHeaders.Get("X-Codex-Beta-Features") != defaultCodexBetaFeatures {
		t.Fatalf("Codex websocket profile headers=%v", gotHeaders)
	}
	sendResult := event.NewResult(events.TopicWSSend, "test", "upstream")
	upstream.handleWSSend(event.NewEvent(events.TopicWSSend, "test", "upstream", nil, events.WSSendCommand{SessionID: opened.SessionID, Payload: []byte(`{"type":"response.create"}`), Fingerprint: events.CodexFingerprint{Mode: "session", InstallationID: "install-id", SessionID: "session-id", ThreadID: "thread-id", TurnID: "turn-id", WindowID: "thread-id:0"}}), sendResult)
	if _, err := sendResult.Get(); err != nil {
		t.Fatal(err)
	}
	pullResult := event.NewResult(events.TopicWSPull, "test", "upstream")
	upstream.handleWSPull(event.NewEventWithContext(events.TopicWSPull, "test", "upstream", nil, context.Background(), events.WSPullCommand{SessionID: opened.SessionID, TimeoutMillis: 1000}), pullResult)
	pulled, err := event.GetAs[events.WSPullResult](pullResult)
	if err != nil || !strings.Contains(string(pulled.Payload), "response.completed") || !strings.Contains(string(pulled.Payload), `"turn_id":"turn-id"`) {
		t.Fatalf("CP-WS-004 pulled=%s err=%v", pulled.Payload, err)
	}
	upstream.Teardown(context.Background())
	if len(upstream.websockets) != 0 {
		t.Fatal("CP-ARCH-004 websocket survived teardown")
	}
}

// CP-WS-010: a rejected handshake carries the same quota semantics as HTTP/SSE.
func TestWebsocketHandshakeRejectionPreservesQuotaSemantics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "17")
		w.Header().Set("X-Codex-Primary-Used-Percent", "100")
		w.Header().Set("X-Codex-Primary-Reset-After-Seconds", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","code":"usage_limit_reached","message":"quota exhausted","resets_in_seconds":120}}`))
	}))
	defer server.Close()
	previous := responsesWebsocketURL
	responsesWebsocketURL = "ws" + strings.TrimPrefix(server.URL, "http")
	t.Cleanup(func() { responsesWebsocketURL = previous })

	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(2)
	upstream := New(hub, background)
	t.Cleanup(func() {
		upstream.Teardown(context.Background())
		background.Shutdown(context.Background())
		hub.Terminate(context.Background())
	})
	openResult := event.NewResult(events.TopicWSOpen, "test", "upstream")
	upstream.handleWSOpen(event.NewEventWithContext(events.TopicWSOpen, "test", "upstream", nil, context.Background(), events.WSOpenCommand{AccessToken: "secret-token"}), openResult)
	value, err := openResult.Get()
	if err != nil {
		t.Fatal(err)
	}
	opened := value.(events.WSOpenResult)
	if opened.HTTPStatus != http.StatusTooManyRequests || opened.ErrorClass != events.ErrorRateLimit || !opened.RateLimit.UsageLimited || opened.RateLimit.ResetAt == "" || opened.RetryAfterSeconds < 17 {
		t.Fatalf("CP-WS-010/011 opened=%+v", opened)
	}
	if opened.SafeError.Type != "usage_limit_reached" || opened.SafeError.Code != "usage_limit_reached" || opened.SafeError.Message != "quota exhausted" || len(opened.Headers) == 0 {
		t.Fatalf("CP-FAIL-013 opened=%+v", opened)
	}
}

func TestWebsocketDialerConfiguresHTTPSProxyTLS(t *testing.T) {
	httpsDialer, err := newWebsocketDialer("https://proxy.invalid:8443")
	if err != nil || httpsDialer.Proxy == nil || httpsDialer.NetDialTLSContext == nil {
		t.Fatalf("CP-SEC-004 HTTPS dialer=%+v err=%v", httpsDialer, err)
	}
	httpDialer, err := newWebsocketDialer("http://proxy.invalid:8080")
	if err != nil || httpDialer.Proxy == nil || httpDialer.NetDialTLSContext != nil {
		t.Fatalf("CP-SEC-004 HTTP dialer=%+v err=%v", httpDialer, err)
	}
}

func TestForceStreamPreservesNativeResponseFields(t *testing.T) {
	value, err := forceStream([]byte(`{"model":"gpt-5.2","input":"hello","stream":false,"tools":[{"type":"function"}],"metadata":{"tenant":"alpha"}}`))
	if err != nil {
		t.Fatal(err)
	}
	text := string(value)
	for _, required := range []string{`"stream":true`, `"store":false`, `"tools"`, `"metadata"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("forced request lost native field %s: %s", required, text)
		}
	}
	if !strings.Contains(text, `"content":[{"text":"hello","type":"input_text"}]`) || !strings.Contains(text, `"role":"user"`) {
		t.Fatalf("string input was not normalized for Codex: %s", text)
	}
}

func TestCompletedResponseSupportsJSONAndSSE(t *testing.T) {
	jsonResponse := &http.Response{Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"response","id":"resp_1"}`))}
	value, class, observation, _, err := completedResponse(jsonResponse, 1024)
	if err != nil || class != "" || observation.UsageLimited || string(value) != `{"object":"response","id":"resp_1"}` {
		t.Fatalf("json completed response = %s class=%s observation=%+v err=%v", value, class, observation, err)
	}
	sseResponse := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello \"}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"world\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"id\":\"resp_2\"}}\n\n"))}
	value, class, observation, _, err = completedResponse(sseResponse, 1024)
	if err != nil || class != "" || observation.UsageLimited || !strings.Contains(string(value), `"resp_2"`) || !strings.Contains(string(value), `"output_text":"hello world"`) {
		t.Fatalf("sse completed response = %s class=%s observation=%+v err=%v", value, class, observation, err)
	}
}

// CP-STREAM-008: buffered and streaming terminal errors share one classifier.
func TestCompletedResponseUsesTerminalFailureClassifier(t *testing.T) {
	response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		`data: {"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"context_length_exceeded","param":"input","message":"context is too long"}}}` + "\n\n"))}
	_, class, observation, safeError, err := completedResponse(response, 4096)
	if err == nil || class != events.ErrorInvalidRequest || observation.UsageLimited {
		t.Fatalf("class=%q observation=%+v err=%v", class, observation, err)
	}
	if safeError.Type != "invalid_request_error" || safeError.Code != "context_length_exceeded" || safeError.Param != "input" || safeError.Message != "context is too long" {
		t.Fatalf("safe error=%+v", safeError)
	}

	contentPolicy := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		`data: {"type":"response.failed","error":{"type":"content_policy_violation","message":"blocked by policy"}}` + "\n\n"))}
	_, class, _, _, err = completedResponse(contentPolicy, 4096)
	if err == nil || class != events.ErrorInvalidRequest {
		t.Fatalf("content policy class=%q err=%v", class, err)
	}

	capacity := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		`data: {"type":"response.failed","error":{"type":"invalid_request_error","message":"selected model is at capacity"}}` + "\n\n"))}
	_, class, _, _, err = completedResponse(capacity, 4096)
	if err == nil || class != events.ErrorUpstream {
		t.Fatalf("capacity class=%q err=%v", class, err)
	}
}

func TestCompletedResponseRecoversCompactionFromAddedEvent(t *testing.T) {
	response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"id\":\"cmp_added\",\"type\":\"compaction\",\"encrypted_content\":\"safe\"}}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_added\",\"object\":\"response\",\"output\":[{\"type\":\"message\",\"content\":[]}]}}\n\n"))}
	payload, class, _, _, err := completedResponse(response, 4096)
	if err != nil || class != "" || !strings.Contains(string(payload), `"id":"cmp_added"`) {
		t.Fatalf("added compaction payload=%s class=%q err=%v", payload, class, err)
	}
	if _, supported, err := nativeCompactResponse(payload); err != nil || !supported {
		t.Fatalf("native compact supported=%v err=%v payload=%s", supported, err, payload)
	}
}

func TestCompletedResponseAddedOnlyCompactionDoesNotInventMessage(t *testing.T) {
	response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"cmp_only\",\"type\":\"compaction\",\"encrypted_content\":\"safe\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_only\",\"object\":\"response\",\"output\":[]}}\n\n"))}
	payload, class, _, _, err := completedResponse(response, 4096)
	if err != nil || class != "" || !strings.Contains(string(payload), `"id":"cmp_only"`) || strings.Contains(string(payload), `"type":"message"`) {
		t.Fatalf("added-only payload=%s class=%q err=%v", payload, class, err)
	}
}

func TestCompletedResponseRejectsEmptyCompletedWithoutUsageOrOutput(t *testing.T) {
	response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_empty\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_empty\",\"status\":\"completed\",\"output\":[]}}\n\n"))}
	_, class, _, _, err := completedResponse(response, 4096)
	if err == nil || class != events.ErrorUpstream {
		t.Fatalf("CP-STREAM-006 class=%q err=%v", class, err)
	}

	usageOnly := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_usage\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"))}
	value, class, _, _, err := completedResponse(usageOnly, 4096)
	if err != nil || class != "" || !strings.Contains(string(value), `"input_tokens":3`) {
		t.Fatalf("CP-STREAM-006 usage-only value=%s class=%q err=%v", value, class, err)
	}
}

func TestCompletedResponsePreservesIncompletePartialOutputAndUsage(t *testing.T) {
	response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n" +
			"data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_partial\",\"status\":\"incomplete\",\"output\":[],\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n"))}
	value, class, _, _, err := completedResponse(response, 4096)
	if err != nil || class != "" || !strings.Contains(string(value), `"status":"incomplete"`) || !strings.Contains(string(value), `"output_text":"partial"`) || !strings.Contains(string(value), `"output_tokens":2`) {
		t.Fatalf("CP-STREAM-001/007 value=%s class=%q err=%v", value, class, err)
	}
}

func TestCompletedResponseRejectsEmptyIncomplete(t *testing.T) {
	response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_empty\"}}\n\n" +
			"data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_empty\",\"status\":\"incomplete\",\"output\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n"))}
	_, class, _, _, err := completedResponse(response, 4096)
	if err == nil || class != events.ErrorUpstream {
		t.Fatalf("CP-STREAM-014 class=%q err=%v", class, err)
	}

	unknownUsage := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_unknown\",\"status\":\"incomplete\",\"output\":[]}}\n\n"))}
	if _, class, _, _, err := completedResponse(unknownUsage, 4096); err != nil || class != "" {
		t.Fatalf("missing usage must remain a legitimate incomplete: class=%q err=%v", class, err)
	}

	native := &http.Response{Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(
		`{"object":"response","status":"incomplete","output":[],"usage":{"input_tokens":7,"output_tokens":0}}`))}
	if _, class, _, _, err := completedResponse(native, 4096); err == nil || class != events.ErrorUpstream {
		t.Fatalf("native CP-STREAM-014 class=%q err=%v", class, err)
	}
}

func TestSafeUpstreamErrorIsBoundedAndRedacted(t *testing.T) {
	safe := safeUpstreamError([]byte(`{"error":{"type":"invalid_request_error","code":"invalid_function_parameters","param":"input[1].tools[2]","message":"Invalid schema"}}`))
	if safe.Type != "invalid_request_error" || safe.Code != "invalid_function_parameters" || safe.Param != "input[1].tools[2]" || safe.Message != "Invalid schema" {
		t.Fatalf("CP-FAIL-013 safe=%+v", safe)
	}
	redacted := safeUpstreamError([]byte(`{"error":{"message":"Authorization: Bearer secret"}}`))
	if redacted.Message != "" {
		t.Fatalf("CP-FAIL-013 secret message=%q", redacted.Message)
	}
}

func TestCompletedResponseRebuildsFunctionCallOutputItem(t *testing.T) {
	stream := "event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"arguments\":\"{\\\"city\\\":\\\"Shanghai\\\"}\",\"call_id\":\"call_1\",\"name\":\"lookup_city\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"id\":\"resp_1\",\"output\":[]}}\n\n"
	response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
	value, class, _, _, err := completedResponse(response, 4096)
	if err != nil || class != "" {
		t.Fatalf("completed class=%q err=%v", class, err)
	}
	var completed struct {
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			CallID    string `json:"call_id"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(value, &completed); err != nil {
		t.Fatal(err)
	}
	if len(completed.Output) != 1 || completed.Output[0].Type != "function_call" || completed.Output[0].Name != "lookup_city" || completed.Output[0].CallID != "call_1" || completed.Output[0].Arguments != `{"city":"Shanghai"}` {
		t.Fatalf("function output=%s", value)
	}
}

func TestResponseWithOutputTextBuildsStandardOutputItem(t *testing.T) {
	value := responseWithOutputText(json.RawMessage(`{"object":"response","id":"resp_1","output":[]}`), "hello")
	var response struct {
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(value, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 1 || response.Output[0].Type != "message" || response.Output[0].Role != "assistant" || len(response.Output[0].Content) != 1 || response.Output[0].Content[0].Type != "output_text" || response.Output[0].Content[0].Text != "hello" {
		t.Fatalf("standard output=%s", value)
	}
}

func TestClassifyStatusTreatsClientErrorsAsInvalidRequest(t *testing.T) {
	if got := classifyStatus(http.StatusBadRequest); got != events.ErrorInvalidRequest {
		t.Fatalf("400 class=%q", got)
	}
	if got := classifyStatus(http.StatusUnprocessableEntity); got != events.ErrorInvalidRequest {
		t.Fatalf("422 class=%q", got)
	}
}

func TestTerminalClass(t *testing.T) {
	if done, class := terminalClass([]byte("data: {\"type\":\"response.completed\"}\n")); !done || class != "" {
		t.Fatalf("completed terminal = done=%v class=%q", done, class)
	}
	if done, class := terminalClass([]byte("data: {\"type\":\"response.failed\"}\n")); !done || class != events.ErrorUpstream {
		t.Fatalf("failed terminal = done=%v class=%q", done, class)
	}
	if done, class := terminalClass([]byte("data: {\"type\":\"response.incomplete\"}\n")); !done || class != "" {
		t.Fatalf("incomplete terminal = done=%v class=%q", done, class)
	}
}

func TestCodexStreamSemanticsRequiresActualOutput(t *testing.T) {
	for _, line := range []string{
		`data: {"type":"response.output_text.delta","delta":""}`,
		`data: {"type":"response.output_item.added","item":{"type":"message","content":[]}}`,
	} {
		if semantic, _ := codexStreamSemantics([]byte(line), false); semantic {
			t.Fatalf("CP-STREAM-008 empty event became output: %s", line)
		}
	}
	if semantic, _ := codexStreamSemantics([]byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), false); !semantic {
		t.Fatal("CP-STREAM-008 non-empty delta was not output")
	}
	if semantic, _ := codexStreamSemantics([]byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"lookup"}}`), false); !semantic {
		t.Fatal("CP-STREAM-014 function call item was not output")
	}
}

func TestCodexStreamSemanticsRejectsEmptyIncomplete(t *testing.T) {
	line := []byte(`data: {"type":"response.incomplete","response":{"output":[],"usage":{"output_tokens":0}}}`)
	if semantic, empty := codexStreamSemanticsWithOutput(line, false, false); semantic || !empty {
		t.Fatalf("CP-STREAM-014 semantic=%v empty=%v", semantic, empty)
	}
	if semantic, empty := codexStreamSemanticsWithOutput(line, true, true); !semantic || empty {
		t.Fatalf("output evidence lost: semantic=%v empty=%v", semantic, empty)
	}
}

func TestConfigureCodexHTTP2Keepalive(t *testing.T) {
	transport := &http.Transport{}
	h2, err := configureCodexHTTP2Keepalive(transport)
	if err != nil {
		t.Fatal(err)
	}
	if h2.ReadIdleTimeout != codexHTTP2ReadIdleTimeout || h2.PingTimeout != codexHTTP2PingTimeout {
		t.Fatalf("CP-STREAM-016 h2=%+v transport=%+v", h2, transport)
	}
	client, err := newHTTPClient("")
	if err != nil || !client.Transport.(*http.Transport).ForceAttemptHTTP2 {
		t.Fatalf("Codex client did not force HTTP/2: client=%+v err=%v", client, err)
	}
}

func TestRunStreamKeepsRetryableTerminalBeforeOutputBuffered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream := &responseStream{cancel: cancel, updates: make(chan streamUpdate, 8)}
	upstream := &Upstream{streams: map[string]*responseStream{"capacity": stream}}
	body := "data: {\"type\":\"response.created\"}\n\n" +
		"data: {\"type\":\"error\",\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":120}}\n\n"
	upstream.runStream(ctx, "capacity", stream, io.NopCloser(strings.NewReader(body)), 4096)
	dataFrames := 0
	var terminal streamUpdate
	for update := range stream.updates {
		if len(update.data) > 0 {
			dataFrames++
		}
		if update.done {
			terminal = update
		}
	}
	if dataFrames != 0 || terminal.errorClass != events.ErrorRateLimit || !terminal.rateLimit.UsageLimited {
		t.Fatalf("CP-STREAM-008 data=%d terminal=%+v", dataFrames, terminal)
	}
}

func TestWebsocketTerminalFailureClassifiesQuotaReset(t *testing.T) {
	class, observation := websocketTerminalFailure([]byte(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}}`))
	if class != events.ErrorRateLimit || !observation.UsageLimited || observation.ResetAt == "" {
		t.Fatalf("CP-WS-010 class=%q observation=%+v", class, observation)
	}
}

func TestTerminalOutcomeClassifiesStreamErrorsBeforeOutput(t *testing.T) {
	done, class, _ := terminalOutcome([]byte(`data: {"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded"}}`))
	if !done || class != events.ErrorInvalidRequest {
		t.Fatalf("CP-STREAM-008 invalid request done=%v class=%q", done, class)
	}
	done, class, _ = terminalOutcome([]byte(`data: {"type":"error","error":{"code":"server_is_overloaded"}}`))
	if !done || class != events.ErrorUpstream {
		t.Fatalf("CP-STREAM-008 capacity done=%v class=%q", done, class)
	}
}

func TestRunStreamRejectsEOFAfterOutputItemDoneEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream := &responseStream{cancel: cancel, updates: make(chan streamUpdate, 8)}
	upstream := &Upstream{streams: map[string]*responseStream{"stream-1": stream}}
	upstream.runStream(ctx, "stream-1", stream, io.NopCloser(strings.NewReader("event: response.output_item.done\n")), 1024)
	if upstream.stream("stream-1") == nil {
		t.Fatal("producer removed stream before the consumer drained terminal state")
	}
	done := false
	for update := range stream.updates {
		if update.done {
			done = true
			if update.errorClass != events.ErrorProtocol {
				t.Fatalf("terminal class=%q", update.errorClass)
			}
		}
	}
	if !done {
		t.Fatal("CP-STREAM-002 EOF after output_item.done was not reported")
	}
	upstream.removeStream("stream-1")
}

// CP-STREAM-011: a data line is not a delivered SSE event until its blank line
// arrives. Parse only complete frames, as a client does, before checking done.
func TestRunStreamDeliversCompleteTerminalEvents(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		class         events.ErrorClass
	}{
		{"response.completed", `{"type":"response.completed","response":{"status":"completed"}}`, ""},
		{"response.incomplete", `{"type":"response.incomplete","response":{"status":"incomplete"}}`, ""},
		{"response.failed", `{"type":"response.failed","response":{"error":{"type":"server_error"}}}`, events.ErrorUpstream},
		{"error", `{"type":"error","error":{"type":"usage_limit_reached","resets_in_seconds":120}}`, events.ErrorRateLimit},
	} {
		for _, ending := range []struct{ name, newline, suffix string }{
			{"LF", "\n", "\n\n"},
			{"CRLF", "\r\n", "\r\n\r\n"},
			{"EOF after line", "\n", "\n"},
			{"EOF after JSON", "\n", ""},
			{"concatenated JSON", "\n", "\n\n"},
		} {
			t.Run(tc.name+"/"+ending.name, func(t *testing.T) {
				delta := `{"type":"response.output_text.delta","delta":"hello"}`
				body := "data: " + delta + ending.newline + ending.newline + "event: " + tc.name + ending.newline + "data: " + tc.payload + ending.suffix
				if ending.name == "concatenated JSON" {
					body = "data: " + delta + tc.payload + ending.suffix
				}
				stream := &responseStream{updates: make(chan streamUpdate, 32)}
				upstream := &Upstream{}
				upstream.runStream(context.Background(), "test", stream, io.NopCloser(strings.NewReader(body)), 4096)
				var wire strings.Builder
				doneCount := 0
				for update := range stream.updates {
					if doneCount != 0 {
						t.Fatal("data/update after done")
					}
					wire.Write(update.data)
					if !update.done {
						continue
					}
					doneCount++
					if update.errorClass != tc.class {
						t.Fatalf("class=%q want=%q", update.errorClass, tc.class)
					}
					if tc.class == events.ErrorRateLimit && (!update.rateLimit.UsageLimited || update.retryAfterSeconds <= 0) {
						t.Fatalf("quota classification lost: %+v", update)
					}
					// EOF alone must not dispatch the last event: only blank lines do.
					scanner := bufio.NewScanner(strings.NewReader(wire.String()))
					var data string
					var types []string
					for scanner.Scan() {
						line := scanner.Text()
						if strings.HasPrefix(line, "data: ") {
							data = strings.TrimPrefix(line, "data: ")
						}
						if line == "" && data != "" {
							var event struct {
								Type string `json:"type"`
							}
							if err := json.Unmarshal([]byte(data), &event); err != nil {
								t.Fatal(err)
							}
							types = append(types, event.Type)
							data = ""
						}
					}
					if err := scanner.Err(); err != nil {
						t.Fatal(err)
					}
					if len(types) != 2 || types[0] != "response.output_text.delta" || types[1] != tc.name || data != "" {
						t.Fatalf("CP-STREAM-011 terminal not dispatched exactly once: events=%v pending=%q", types, data)
					}
					if !strings.HasSuffix(wire.String(), ending.newline+ending.newline) {
						t.Fatal("terminal newline style changed")
					}
				}
				if doneCount != 1 {
					t.Fatalf("done count=%d", doneCount)
				}
			})
		}
	}
}

// CP-STREAM-002/011: event names or item completion cannot replace a valid
// response terminal payload, even when earlier text was delivered.
func TestRunStreamRejectsTruncatedTerminals(t *testing.T) {
	for _, tail := range []string{
		"", "event: response.completed\n", "event: response.failed\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\"\n\n",
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n\n",
	} {
		t.Run(tail, func(t *testing.T) {
			stream := &responseStream{updates: make(chan streamUpdate, 32)}
			upstream := &Upstream{}
			body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" + tail
			upstream.runStream(context.Background(), "test", stream, io.NopCloser(strings.NewReader(body)), 4096)
			done := false
			for update := range stream.updates {
				if update.done {
					done = true
					if update.errorClass != events.ErrorProtocol {
						t.Fatalf("truncated stream class=%q", update.errorClass)
					}
				}
			}
			if !done {
				t.Fatal("missing terminal result")
			}
		})
	}
}

func TestRateLimitObservationReadsCodexUsageReset(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	observation := rateLimitObservation([]byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`), now)
	if !observation.UsageLimited || observation.ResetAt != now.Add(120*time.Second).Format(time.RFC3339) {
		t.Fatalf("observation=%+v", observation)
	}
	done, class, streamObservation := terminalOutcome([]byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":120}}}\n"))
	if !done || class != events.ErrorRateLimit || !streamObservation.UsageLimited {
		t.Fatalf("terminal outcome done=%v class=%q observation=%+v", done, class, streamObservation)
	}
	isoObservation := rateLimitObservation([]byte(`{"error":{"type":"usage_limit_reached","resets_at":"2023-11-14T22:16:40Z"}}`), now)
	if !isoObservation.UsageLimited || isoObservation.ResetAt != "2023-11-14T22:16:40Z" {
		t.Fatalf("ISO reset observation=%+v", isoObservation)
	}
	if class := errorClassWithRateLimit(http.StatusInternalServerError, isoObservation); class != events.ErrorRateLimit {
		t.Fatalf("usage limit class=%q", class)
	}
	topLevel := rateLimitObservation([]byte(`{"type":"USAGE_LIMIT_REACHED","resets_at":1700000300000}`), now)
	if !topLevel.UsageLimited || topLevel.ResetAt != now.Add(300*time.Second).Format(time.RFC3339) {
		t.Fatalf("CP-FAIL-017 top-level observation=%+v", topLevel)
	}
	websocketBody := rateLimitObservation([]byte(`{"type":"error","body":{"error":{"type":"usage_limit_reached","resets_in_seconds":45}}}`), now)
	if !websocketBody.UsageLimited || websocketBody.ResetAt != now.Add(45*time.Second).Format(time.RFC3339) {
		t.Fatalf("CP-FAIL-017 websocket body observation=%+v", websocketBody)
	}
}

func TestRateLimitObservationDoesNotTreatGeneric429AsQuotaExhaustion(t *testing.T) {
	observation := rateLimitObservation([]byte(`{"error":{"type":"rate_limit_exceeded","resets_in_seconds":120}}`), time.Now().UTC())
	if observation.UsageLimited || observation.ResetAt != "" {
		t.Fatalf("generic rate limit observation=%+v", observation)
	}
}

func TestPerformUsesFixedCodexHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("User-Agent") != currentIdentity.UserAgent || r.Header.Get("Originator") != currentIdentity.Originator {
			t.Fatalf("Codex identity headers user-agent=%q originator=%q", r.Header.Get("User-Agent"), r.Header.Get("Originator"))
		}
		if r.Header.Get("ChatGPT-Account-ID") != "chatgpt-account-id" || r.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("Codex account headers=%v", r.Header)
		}
		// CP-HDR-023: the proxy may fill the turn state, and it still reaches the
		// upstream unchanged.
		if r.Header.Get("X-Codex-Turn-State") != "opaque-turn-state" {
			t.Fatalf("Codex turn state=%q", r.Header.Get("X-Codex-Turn-State"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "upstream-secret")
		w.Header().Set("X-Debug-Trace", "trace-1")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	previousURL := responsesURL
	responsesURL = server.URL
	t.Cleanup(func() { responsesURL = previousURL })
	response, attempt, class, _, err := perform(context.Background(), "access-token", "chatgpt-account-id", "", []byte(`{"model":"gpt-5.2-codex","stream":true}`), codexRequestProfile{turnState: "opaque-turn-state"})
	if err != nil || class != "" || response == nil {
		t.Fatalf("perform response=%v class=%q err=%v", response, class, err)
	}
	_ = response.Body.Close()
	requestHeaders := eventHeaderMap(attempt.Request.Headers)
	responseHeaders := eventHeaderMap(attempt.Response.Headers)
	if requestHeaders.Get("Authorization") != "<redacted>" || requestHeaders.Get("ChatGPT-Account-ID") != "<redacted>" {
		t.Fatalf("credential headers were not redacted: %v", requestHeaders)
	}
	// CP-HDR-023: the archived attempt keeps only the header's presence.
	if requestHeaders.Get("X-Codex-Turn-State") != "<redacted>" {
		t.Fatalf("turn state was archived: %v", requestHeaders)
	}
	if requestHeaders.Get("User-Agent") != currentIdentity.UserAgent || attempt.Request.Method != http.MethodPost || attempt.Request.URL != server.URL || attempt.Request.BodyBytes == 0 {
		t.Fatalf("request observation=%+v headers=%v", attempt.Request, requestHeaders)
	}
	if !attempt.Response.Observed || attempt.Response.Status != http.StatusOK || responseHeaders.Get("Set-Cookie") != "<redacted>" || responseHeaders.Get("X-Debug-Trace") != "trace-1" {
		t.Fatalf("response observation=%+v headers=%v", attempt.Response, responseHeaders)
	}
}

// CP-OBS-009: the archived attempt keeps credentials verbatim only when the
// inbound command explicitly asked for the archive fidelity switch; every other
// caller keeps the redacted projection.
func TestPerformKeepsCredentialHeadersWhenArchiveIsUnredacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Turn-State", "response-turn-state")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	previousURL := responsesURL
	responsesURL = server.URL
	t.Cleanup(func() { responsesURL = previousURL })

	for name, testCase := range map[string]struct {
		unredacted bool
		want       string
	}{
		"archive fidelity": {unredacted: true, want: "Bearer access-token"},
		"default":          {unredacted: false, want: "<redacted>"},
	} {
		t.Run(name, func(t *testing.T) {
			profile := codexRequestProfile{turnState: "opaque-turn-state", archiveUnredacted: testCase.unredacted}
			response, attempt, class, _, err := perform(context.Background(), "access-token", "chatgpt-account-id", "", []byte(`{"model":"gpt-test"}`), profile)
			if err != nil || class != "" {
				t.Fatalf("perform class=%q err=%v", class, err)
			}
			_ = response.Body.Close()
			requestHeaders := eventHeaderMap(attempt.Request.Headers)
			if requestHeaders.Get("Authorization") != testCase.want {
				t.Fatalf("CP-OBS-009 request authorization=%q", requestHeaders.Get("Authorization"))
			}
			turnState := "response-turn-state"
			if !testCase.unredacted {
				turnState = "<redacted>"
			}
			responseHeaders := eventHeaderMap(attempt.Response.Headers)
			if responseHeaders.Get("X-Codex-Turn-State") != turnState {
				t.Fatalf("CP-OBS-009 response turn state=%q", responseHeaders.Get("X-Codex-Turn-State"))
			}
		})
	}
}

// CP-HDR-003/004: the inference path reuses the downstream client's bounded
// identity and falls back to the versioned profile for anything rejected.
func TestPerformReusesClientIdentity(t *testing.T) {
	const clientAgent = "codex-tui/0.154.0 (Ubuntu 24.4.0; x86_64) WindowsTerminal (codex-tui; 0.154.0)"
	seen := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	previousURL := responsesURL
	responsesURL = server.URL
	t.Cleanup(func() { responsesURL = previousURL })

	for name, testCase := range map[string]struct {
		identity     events.ClientIdentity
		wantAgent    string
		wantOriginat string
	}{
		"client identity": {
			identity:     events.ClientIdentity{UserAgent: clientAgent, Originator: "codex-tui"},
			wantAgent:    clientAgent,
			wantOriginat: "codex-tui",
		},
		"absent": {
			identity:     events.ClientIdentity{},
			wantAgent:    currentIdentity.UserAgent,
			wantOriginat: currentIdentity.Originator,
		},
		"oversized": {
			identity:     events.ClientIdentity{UserAgent: strings.Repeat("x", maxClientUserAgentBytes+1), Originator: strings.Repeat("y", maxClientOriginatorBytes+1)},
			wantAgent:    currentIdentity.UserAgent,
			wantOriginat: currentIdentity.Originator,
		},
		"control characters": {
			identity:     events.ClientIdentity{UserAgent: "codex-tui\r\nX-Injected: 1", Originator: "codex\ttui"},
			wantAgent:    currentIdentity.UserAgent,
			wantOriginat: currentIdentity.Originator,
		},
	} {
		t.Run(name, func(t *testing.T) {
			response, _, class, _, err := perform(context.Background(), "access-token", "chatgpt-account-id", "", []byte(`{"model":"gpt-test"}`), codexRequestProfile{clientIdentity: testCase.identity})
			if err != nil || class != "" {
				t.Fatalf("perform class=%q err=%v", class, err)
			}
			_ = response.Body.Close()
			headers := <-seen
			if headers.Get("User-Agent") != testCase.wantAgent || headers.Get("Originator") != testCase.wantOriginat {
				t.Fatalf("CP-HDR-003/004 user-agent=%q originator=%q", headers.Get("User-Agent"), headers.Get("Originator"))
			}
			if headers.Get("Authorization") != "Bearer access-token" || headers.Get("ChatGPT-Account-ID") != "chatgpt-account-id" {
				t.Fatalf("CP-CLIENT-004 identity leaked into credentials: %v", headers)
			}
		})
	}
}

func eventHeaderMap(headers []events.Header) http.Header {
	result := http.Header{}
	for _, header := range headers {
		result.Add(header.Name, header.Value)
	}
	return result
}

func TestPerformUsesAllowlistedCodexFeatureHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Codex-Beta-Features") != "remote_compaction_v2" || r.Header.Get("X-OpenAI-Internal-Codex-Responses-Lite") != "true" {
			t.Fatalf("feature headers=%v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
	}))
	defer server.Close()
	previous := responsesURL
	responsesURL = server.URL
	defer func() { responsesURL = previous }()
	response, _, class, _, err := perform(context.Background(), "access", "account", "", []byte(`{"model":"gpt-test"}`), codexRequestProfile{sessionHash: "session", responsesLite: true})
	if err != nil || class != "" {
		t.Fatalf("perform class=%q err=%v", class, err)
	}
	_ = response.Body.Close()
}

// CP-HDR-011: 三个载体（身份 header、X-Codex-Turn-Metadata、body client_metadata）
// 必须自洽——身份取代理值，turn 级与属性取客户端值。
func TestCodexTurnMetadataCarriersStayConsistent(t *testing.T) {
	projection := events.TurnMetadata{
		TurnID: "client-turn", RootTurnID: "client-root-turn", TurnStartedAtMS: 1789711880466,
		Attributes: []byte(`{"request_kind":"turn","sandbox_mode":"read-only","window_number":3}`),
	}
	profile := codexRequestProfile{sessionHash: "session-hash", turnMetadata: projection}

	headers := http.Header{}
	applyCodexRequestIdentity(headers, profile)
	applyCodexTurnMetadata(headers, profile)

	session := headers.Get("Session-Id")
	if session != "session-hash" || headers.Get("Thread-Id") != "session-hash" || headers.Get("X-Codex-Window-Id") != "session-hash:0" {
		t.Fatalf("CP-HDR-007..010 session headers=%v", headers)
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(headers.Get("X-Codex-Turn-Metadata")), &metadata); err != nil {
		t.Fatalf("CP-HDR-011 metadata=%q err=%v", headers.Get("X-Codex-Turn-Metadata"), err)
	}
	if metadata["session_id"] != session || metadata["thread_id"] != session || metadata["window_id"] != session+":0" {
		t.Fatalf("CP-HDR-011 metadata identity disagrees with headers: %v", metadata)
	}
	if metadata["turn_id"] != "client-turn" || metadata["root_turn_id"] != "client-root-turn" || metadata["turn_started_at_unix_ms"] != float64(1789711880466) {
		t.Fatalf("CP-HDR-011 turn level=%v", metadata)
	}
	for key, want := range map[string]any{"request_kind": "turn", "sandbox_mode": "read-only", "window_number": float64(3)} {
		if metadata[key] != want {
			t.Fatalf("CP-HDR-011 attribute %s=%v want %v", key, metadata[key], want)
		}
	}
	// 客户端身份即使出现在投影里也不会被采用（身份键在入站侧已被剥离，这里验证
	// 载体自身不会回灌）。
	if _, found := metadata["installation_id"]; found {
		t.Fatalf("CP-FP-002 off mode claimed an installation id: %v", metadata)
	}

	body, err := applyCodexRequestBody([]byte(`{"model":"gpt-test","client_metadata":{"session_id":"client-leak"}}`), profile)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		ClientMetadata map[string]any `json:"client_metadata"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		t.Fatalf("CP-HDR-011 body=%s", body)
	}
	if envelope.ClientMetadata["session_id"] != session || envelope.ClientMetadata["thread_id"] != session || envelope.ClientMetadata["turn_id"] != "client-turn" {
		t.Fatalf("CP-HDR-011 body identity=%+v", envelope.ClientMetadata)
	}
	if strings.Contains(string(body), "client-leak") {
		t.Fatalf("CP-HDR-011 client identity leaked into body: %s", body)
	}
	// 属性只进内嵌 JSON：顶层只允许已知的字符串键（上游以 invalid_type 拒绝其它类型）。
	embedded, _ := envelope.ClientMetadata["x-codex-turn-metadata"].(string)
	if !strings.Contains(embedded, `"sandbox_mode":"read-only"`) || !strings.Contains(embedded, `"window_number":3`) {
		t.Fatalf("CP-HDR-011 embedded metadata=%s", embedded)
	}
	for key, value := range envelope.ClientMetadata {
		if _, isString := value.(string); !isString {
			t.Fatalf("CP-HDR-011 flat client_metadata value %s is not a string: %v", key, value)
		}
	}
	if _, flattened := envelope.ClientMetadata["sandbox_mode"]; flattened {
		t.Fatalf("CP-HDR-011 attribute was flattened into client_metadata: %+v", envelope.ClientMetadata)
	}
}

func TestCodexJSONDocumentsRepairIsBounded(t *testing.T) {
	documents, repaired := splitCodexJSONDocuments([]byte(`{"type":"response.in_progress"}{"type":"response.done"}`))
	if !repaired || len(documents) != 2 {
		t.Fatalf("documents=%q repaired=%v", documents, repaired)
	}
	lines := expandCodexSSELine([]byte(`data: {"type":"response.in_progress"}{"type":"response.completed"}` + "\n"))
	if len(lines) != 2 || !strings.Contains(string(lines[1]), "response.completed") {
		t.Fatalf("expanded=%q", lines)
	}
}

func TestResponseHeadersProjectsCodexUsageAllowlist(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Codex-Primary-Used-Percent", "25")
	headers.Set("X-Codex-Secondary-Window-Minutes", "300")
	headers.Set("X-Codex-Turn-State", "opaque-state")
	headers.Set("Set-Cookie", "secret")
	projected := responseHeaders(headers)
	if len(projected) != 3 || projected[0].Name != "X-Codex-Turn-State" || projected[0].Value != "opaque-state" || projected[1].Name != "X-Codex-Primary-Used-Percent" || projected[2].Name != "X-Codex-Secondary-Window-Minutes" {
		t.Fatalf("projected=%+v", projected)
	}
	headers.Set("X-Codex-Turn-State", strings.Repeat("x", maxCodexTurnStateBytes+1))
	projected = responseHeaders(headers)
	for _, header := range projected {
		if header.Name == "X-Codex-Turn-State" {
			t.Fatalf("oversized turn state was projected: %+v", projected)
		}
	}
}

func TestCodexFingerprintRewritesHeadersAndBodyTogether(t *testing.T) {
	fingerprint := events.CodexFingerprint{Mode: "session", InstallationID: "install-id", SessionID: "session-id", ThreadID: "thread-id", TurnID: "turn-id", WindowID: "thread-id:0", TurnStartedAtUnixMS: 123456789}
	headers := http.Header{}
	applyCodexRequestIdentity(headers, codexRequestProfile{sessionHash: "isolated-session", fingerprint: fingerprint})
	applyCodexTurnMetadata(headers, codexRequestProfile{sessionHash: "isolated-session", fingerprint: fingerprint})
	if headers.Get("X-Codex-Installation-Id") != "install-id" || headers.Get("Session-Id") != "session-id" || headers.Get("Thread-Id") != "thread-id" || headers.Get("X-Client-Request-Id") != "thread-id" {
		t.Fatalf("fingerprint headers=%v", headers)
	}
	if metadata := headers.Get("X-Codex-Turn-Metadata"); !strings.Contains(metadata, `"turn_started_at_unix_ms":123456789`) || !strings.Contains(metadata, `"turn_id":"turn-id"`) {
		t.Fatalf("fingerprint turn metadata=%q", metadata)
	}
	body, err := applyCodexRequestBody([]byte(`{"model":"gpt-test","input":[],"prompt_cache_key":"client-cache","client_metadata":{"session_id":"client-secret","arbitrary":"must-not-pass"}}`), codexRequestProfile{fingerprint: fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		ClientMetadata map[string]any `json:"client_metadata"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.ClientMetadata["x-codex-installation-id"] != "install-id" || envelope.ClientMetadata["session_id"] != "session-id" || envelope.ClientMetadata["turn_id"] != "turn-id" {
		t.Fatalf("fingerprint body=%s metadata=%+v", body, envelope.ClientMetadata)
	}
	if _, exists := envelope.ClientMetadata["arbitrary"]; exists || !strings.Contains(string(body), `"prompt_cache_key":"client-cache"`) {
		t.Fatalf("fingerprint projection leaked metadata or changed cache key: %s", body)
	}
	embedded, _ := envelope.ClientMetadata["x-codex-turn-metadata"].(string)
	if !strings.Contains(embedded, `"turn_started_at_unix_ms":123456789`) {
		t.Fatalf("embedded turn metadata=%q", embedded)
	}
	// CP-HDR-011: without fingerprint convergence the body still carries the
	// attempt's own session identity, and an attempt with no identity at all
	// stays untouched rather than emitting an empty envelope.
	sessionBody, err := applyCodexRequestBody([]byte(`{"model":"gpt-test"}`), codexRequestProfile{sessionHash: "session-hash"})
	if err != nil {
		t.Fatal(err)
	}
	var sessionEnvelope struct {
		ClientMetadata map[string]any `json:"client_metadata"`
	}
	if json.Unmarshal(sessionBody, &sessionEnvelope) != nil {
		t.Fatalf("CP-HDR-011 session body=%s", sessionBody)
	}
	if sessionEnvelope.ClientMetadata["session_id"] != "session-hash" || sessionEnvelope.ClientMetadata["thread_id"] != "session-hash" || sessionEnvelope.ClientMetadata["x-codex-window-id"] != "session-hash:0" {
		t.Fatalf("CP-HDR-011 session identity=%+v", sessionEnvelope.ClientMetadata)
	}
	if _, found := sessionEnvelope.ClientMetadata["installation_id"]; found {
		t.Fatalf("CP-FP-002 off mode must not claim an installation id: %+v", sessionEnvelope.ClientMetadata)
	}
	offBody, err := applyCodexRequestBody([]byte(`{"model":"gpt-test"}`), codexRequestProfile{})
	if err != nil || strings.Contains(string(offBody), "client_metadata") {
		t.Fatalf("off body=%s err=%v", offBody, err)
	}
}

func TestHandleCompactUsesNativeV2ResponsesAndFixedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("CP-COMPACT-001 request=%s accept=%q", r.Method, r.Header.Get("Accept"))
		}
		if r.Header.Get("Authorization") != "Bearer access-token" || r.Header.Get("ChatGPT-Account-ID") != "account-header" {
			t.Fatalf("CP-HDR account headers=%v", r.Header)
		}
		if r.Header.Get("User-Agent") != currentIdentity.UserAgent || r.Header.Get("Originator") != currentIdentity.Originator {
			t.Fatalf("CP-HDR identity=%v", r.Header)
		}
		assertCodexSessionHeaders(t, r.Header, "compact-session-hash")
		if r.Header.Get("X-Codex-Beta-Features") != defaultCodexBetaFeatures || r.Header.Get("X-Codex-Turn-State") != "compact-state" || r.Header.Get("X-Codex-Installation-Id") != "compact-install" {
			t.Fatalf("CP-COMPACT profile headers=%v", r.Header)
		}
		var body struct {
			Stream         bool           `json:"stream"`
			Store          bool           `json:"store"`
			ClientMetadata map[string]any `json:"client_metadata"`
			Input          []struct {
				Type string `json:"type"`
			} `json:"input"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || !body.Stream || body.Store || len(body.Input) != 1 || body.Input[0].Type != "compaction_trigger" || body.ClientMetadata["x-codex-installation-id"] != "compact-install" {
			t.Fatalf("CP-COMPACT-001 native body=%+v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"safe\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_compact\",\"object\":\"response\",\"output\":[]}}\n\n"))
	}))
	defer server.Close()
	previousURL := responsesURL
	responsesURL = server.URL
	t.Cleanup(func() { responsesURL = previousURL })

	upstream := &Upstream{}
	result := event.NewResult(events.TopicCompact, "test", "test")
	upstream.handleCompact(event.NewEventWithContext(events.TopicCompact, "test", "test", nil, context.Background(), events.CompactCommand{
		AccessToken: "access-token", AccountIDHeader: "account-header", Body: []byte(`{"model":"gpt-5.4","input":[]}`), MaxResponseBytes: 1024, SessionHash: "compact-session-hash",
		TurnState: "compact-state", Fingerprint: events.CodexFingerprint{Mode: "device", InstallationID: "compact-install"},
	}), result)
	value, resultErr := result.Get()
	completed, ok := value.(events.CompactResult)
	if resultErr != nil || !ok || completed.ErrorClass != "" || !strings.Contains(string(completed.Body), "resp_compact") || !strings.Contains(string(completed.Body), "response.compaction") {
		t.Fatalf("CP-COMPACT result=%#v err=%v", value, resultErr)
	}
}

func TestForceNativeCompactCanonicalizesTriggerAtInputTail(t *testing.T) {
	body, err := forceNativeCompact([]byte(`{"model":"gpt-5.4","stream":false,"store":true,"tool_choice":"auto","input":[{"type":"compaction_trigger"},{"type":"message","role":"user"},{"type":"compaction_trigger"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Stream     bool             `json:"stream"`
		Store      bool             `json:"store"`
		ToolChoice *json.RawMessage `json:"tool_choice"`
		Input      []struct {
			Type string `json:"type"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Stream || envelope.Store || envelope.ToolChoice != nil || len(envelope.Input) != 2 || envelope.Input[0].Type != "message" || envelope.Input[1].Type != "compaction_trigger" {
		t.Fatalf("CP-COMPACT-001 canonical body=%s", body)
	}
}

func assertCodexSessionHeaders(t *testing.T, headers http.Header, sessionHash string) {
	t.Helper()
	want := map[string]string{"Session-Id": sessionHash, "Thread-Id": sessionHash, "X-Client-Request-Id": sessionHash, "X-Codex-Window-Id": sessionHash + ":0"}
	for name, value := range want {
		if headers.Get(name) != value {
			t.Fatalf("CP-HDR-007..010 %s=%q want %q headers=%v", name, headers.Get(name), value, headers)
		}
	}
}

// CP-HDR-010: 客户端声明的窗口号必须同时出现在 header、X-Codex-Turn-Metadata 与
// body client_metadata；会话段仍是代理身份，与同一份元数据里的 window_number 不再矛盾。
func TestCodexWindowNumberTravelsToCarriers(t *testing.T) {
	projection := events.TurnMetadata{
		TurnID: "client-turn", RootTurnID: "client-turn", TurnStartedAtMS: 1789711880466,
		WindowNumber: 37,
		Attributes:   []byte(`{"window_number":37,"request_kind":"turn"}`),
	}
	profile := codexRequestProfile{sessionHash: "session-hash", turnMetadata: projection}

	headers := http.Header{}
	applyCodexRequestIdentity(headers, profile)
	applyCodexTurnMetadata(headers, profile)
	if headers.Get("X-Codex-Window-Id") != "session-hash:37" {
		t.Fatalf("CP-HDR-010 header window id=%q", headers.Get("X-Codex-Window-Id"))
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(headers.Get("X-Codex-Turn-Metadata")), &metadata); err != nil {
		t.Fatalf("CP-HDR-010 metadata=%q err=%v", headers.Get("X-Codex-Turn-Metadata"), err)
	}
	if metadata["window_id"] != "session-hash:37" || metadata["window_number"] != float64(37) {
		t.Fatalf("CP-HDR-010 metadata disagrees with the window number: %v", metadata)
	}

	body, err := applyCodexRequestBody([]byte(`{"model":"gpt-test","client_metadata":{"session_id":"client-leak"}}`), profile)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		ClientMetadata map[string]any `json:"client_metadata"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("CP-HDR-010 body=%s", body)
	}
	// body 的扁平投影沿用 CLI 的命名（x-codex-window-id），内嵌 JSON 与它一致。
	embedded, _ := envelope.ClientMetadata["x-codex-turn-metadata"].(string)
	if envelope.ClientMetadata["x-codex-window-id"] != "session-hash:37" || !strings.Contains(embedded, `"x-codex-window-id":"session-hash:37"`) {
		t.Fatalf("CP-HDR-010 body carriers=%v embedded=%s", envelope.ClientMetadata, embedded)
	}

	// 未声明窗口号时保持 0，不猜测。
	plainHeaders := http.Header{}
	applyCodexRequestIdentity(plainHeaders, codexRequestProfile{sessionHash: "session-hash"})
	if plainHeaders.Get("X-Codex-Window-Id") != "session-hash:0" {
		t.Fatalf("CP-HDR-010 undeclared window id=%q", plainHeaders.Get("X-Codex-Window-Id"))
	}
}

func TestListModelsUsesAccountHeadersAndProjectsSafeModelIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("client_version") != currentIdentity.ClientVersion {
			t.Fatalf("request=%s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer access-token" || r.Header.Get("ChatGPT-Account-ID") != "account-header" {
			t.Fatalf("account headers=%v", r.Header)
		}
		if r.Header.Get("User-Agent") != currentIdentity.UserAgent || r.Header.Get("Originator") != currentIdentity.Originator {
			t.Fatalf("Codex identity headers=%v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.2-codex","created":7,"owned_by":"openai"},{"id":"gpt-5.3-codex"},{"slug":"gpt-5.2-codex"},{"slug":"bad\nmodel"}]}`))
	}))
	defer server.Close()
	previousURL := modelsURL
	modelsURL = server.URL + "?client_version=" + currentIdentity.ClientVersion
	t.Cleanup(func() { modelsURL = previousURL })

	models, class, err := listModels(context.Background(), "access-token", "account-header", "")
	if err != nil || class != "" {
		t.Fatalf("list models class=%q err=%v", class, err)
	}
	if len(models) != 2 || models[0].ID != "gpt-5.2-codex" || models[0].CreatedAt != 7 || models[1].ID != "gpt-5.3-codex" {
		t.Fatalf("models=%+v", models)
	}
}

func TestGetUsageUsesAccountHeadersAndProjectsBoundedWindows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/usage" {
			t.Fatalf("request=%s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer access-token" || r.Header.Get("ChatGPT-Account-ID") != "account-header" {
			t.Fatalf("account headers=%v", r.Header)
		}
		if r.Header.Get("User-Agent") != currentIdentity.UserAgent || r.Header.Get("Originator") != currentIdentity.Originator || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("Codex usage headers=%v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
          "plan_type":"pro",
          "rate_limit":{"allowed":true,"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_after_seconds":60},"secondary_window":{"used_percent":"80","limit_window_seconds":604800,"reset_at":2000000000}},
          "code_review_rate_limit":{"allowed":false,"primary_window":{"limit_window_seconds":18000,"reset_after_seconds":120}},
          "additional_rate_limits":[{"limit_name":"extra credits","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000}}}]
        }`))
	}))
	defer server.Close()
	previousURL := usageURL
	usageURL = server.URL + "/usage"
	t.Cleanup(func() { usageURL = previousURL })

	plan, windows, class, err := getUsage(context.Background(), "access-token", "account-header", "")
	if err != nil || class != "" || plan != "pro" || len(windows) != 4 {
		t.Fatalf("usage plan=%q windows=%+v class=%q err=%v", plan, windows, class, err)
	}
	byID := map[string]events.UsageWindow{}
	for _, window := range windows {
		byID[window.ID] = window
	}
	if window := byID["standard-primary"]; !window.UsedPercentKnown || window.UsedPercent != 25 || window.WindowSeconds != 18000 || window.ResetAt == "" || !window.AllowedKnown || !window.Allowed {
		t.Fatalf("standard primary=%+v", window)
	}
	if window := byID["code-review-primary"]; !window.LimitReached || !window.UsedPercentKnown || window.UsedPercent != 100 || window.Allowed {
		t.Fatalf("code-review primary=%+v", window)
	}
	if window := byID["additional-extra-credits-primary"]; window.Label != "extra credits" || !window.UsedPercentKnown || window.UsedPercent != 10 {
		t.Fatalf("additional window=%+v", window)
	}
}

func TestErrorClassifiesHTML403AsEndpoint(t *testing.T) {
	if got := errorClassWithBody(http.StatusForbidden, []byte("<!doctype html><html>blocked</html>"), events.RateLimitObservation{}); got != events.ErrorEndpoint {
		t.Fatalf("class=%q", got)
	}
	if got := errorClassWithBody(http.StatusForbidden, []byte(`{"error":{"type":"forbidden"}}`), events.RateLimitObservation{}); got != events.ErrorUpstream {
		t.Fatalf("structured class=%q", got)
	}
}
