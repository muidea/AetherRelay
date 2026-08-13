package biz

import (
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
		conn, err := upgrader.Upgrade(w, r, nil)
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
	upstream.handleWSOpen(event.NewEventWithContext(events.TopicWSOpen, "test", "upstream", nil, context.Background(), events.WSOpenCommand{AccessToken: "secret-token", AccountIDHeader: "account-header", MaxMessageBytes: 1024, SessionHash: "session-hash"}), openResult)
	value, cdErr := openResult.Get()
	if cdErr != nil {
		t.Fatal(cdErr)
	}
	opened := value.(events.WSOpenResult)
	if opened.SessionID == "" || opened.ErrorClass != "" {
		t.Fatalf("CP-WS-002 opened=%+v", opened)
	}
	gotHeaders := <-headers
	if gotHeaders.Get("Authorization") != "Bearer secret-token" || gotHeaders.Get("ChatGPT-Account-ID") != "account-header" || gotHeaders.Get("OpenAI-Beta") != currentIdentity.WebsocketBeta || gotHeaders.Get("User-Agent") != currentIdentity.UserAgent || gotHeaders.Get("Originator") != currentIdentity.Originator {
		t.Fatalf("CP-HDR/CP-WS-002 headers=%v", gotHeaders)
	}
	assertCodexSessionHeaders(t, gotHeaders, "session-hash")
	sendResult := event.NewResult(events.TopicWSSend, "test", "upstream")
	upstream.handleWSSend(event.NewEvent(events.TopicWSSend, "test", "upstream", nil, events.WSSendCommand{SessionID: opened.SessionID, Payload: []byte(`{"type":"response.create"}`)}), sendResult)
	if _, err := sendResult.Get(); err != nil {
		t.Fatal(err)
	}
	pullResult := event.NewResult(events.TopicWSPull, "test", "upstream")
	upstream.handleWSPull(event.NewEventWithContext(events.TopicWSPull, "test", "upstream", nil, context.Background(), events.WSPullCommand{SessionID: opened.SessionID, TimeoutMillis: 1000}), pullResult)
	pulled, err := event.GetAs[events.WSPullResult](pullResult)
	if err != nil || !strings.Contains(string(pulled.Payload), "response.completed") {
		t.Fatalf("CP-WS-004 pulled=%s err=%v", pulled.Payload, err)
	}
	upstream.Teardown(context.Background())
	if len(upstream.websockets) != 0 {
		t.Fatal("CP-ARCH-004 websocket survived teardown")
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
	value, class, observation, err := completedResponse(jsonResponse, 1024)
	if err != nil || class != "" || observation.UsageLimited || string(value) != `{"object":"response","id":"resp_1"}` {
		t.Fatalf("json completed response = %s class=%s observation=%+v err=%v", value, class, observation, err)
	}
	sseResponse := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello \"}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"world\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"id\":\"resp_2\"}}\n\n"))}
	value, class, observation, err = completedResponse(sseResponse, 1024)
	if err != nil || class != "" || observation.UsageLimited || !strings.Contains(string(value), `"resp_2"`) || !strings.Contains(string(value), `"output_text":"hello world"`) {
		t.Fatalf("sse completed response = %s class=%s observation=%+v err=%v", value, class, observation, err)
	}
}

func TestCompletedResponseRejectsEmptyCompletedWithoutUsageOrOutput(t *testing.T) {
	response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_empty\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_empty\",\"status\":\"completed\",\"output\":[]}}\n\n"))}
	_, class, _, err := completedResponse(response, 4096)
	if err == nil || class != events.ErrorUpstream {
		t.Fatalf("CP-STREAM-006 class=%q err=%v", class, err)
	}

	usageOnly := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_usage\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"))}
	value, class, _, err := completedResponse(usageOnly, 4096)
	if err != nil || class != "" || !strings.Contains(string(value), `"input_tokens":3`) {
		t.Fatalf("CP-STREAM-006 usage-only value=%s class=%q err=%v", value, class, err)
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
	value, class, _, err := completedResponse(response, 4096)
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
	if done, class := terminalEvent([]byte("event: response.completed\n")); !done || class != "" {
		t.Fatalf("event-only completed terminal = done=%v class=%q", done, class)
	}
	if done, class := terminalEvent([]byte("event: response.failed\n")); !done || class != events.ErrorUpstream {
		t.Fatalf("event-only failed terminal = done=%v class=%q", done, class)
	}
	if got := sseEventName([]byte("event: response.output_item.done\n")); got != "response.output_item.done" {
		t.Fatalf("output done event=%q", got)
	}
}

func TestRunStreamAcceptsCleanEOFAfterOutputItemDoneEvent(t *testing.T) {
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
			if update.errorClass != "" {
				t.Fatalf("terminal class=%q", update.errorClass)
			}
		}
	}
	if !done {
		t.Fatal("clean EOF after output_item.done was not completed")
	}
	upstream.removeStream("stream-1")
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
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	previousURL := responsesURL
	responsesURL = server.URL
	t.Cleanup(func() { responsesURL = previousURL })
	response, class, _, err := perform(context.Background(), "access-token", "chatgpt-account-id", "", []byte(`{"model":"gpt-5.2-codex","stream":true}`), "", false, false)
	if err != nil || class != "" || response == nil {
		t.Fatalf("perform response=%v class=%q err=%v", response, class, err)
	}
	_ = response.Body.Close()
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
	response, class, _, err := perform(context.Background(), "access", "account", "", []byte(`{"model":"gpt-test"}`), "session", true, true)
	if err != nil || class != "" {
		t.Fatalf("perform class=%q err=%v", class, err)
	}
	_ = response.Body.Close()
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
	headers.Set("Set-Cookie", "secret")
	projected := responseHeaders(headers)
	if len(projected) != 2 || projected[0].Name != "X-Codex-Primary-Used-Percent" || projected[1].Name != "X-Codex-Secondary-Window-Minutes" {
		t.Fatalf("projected=%+v", projected)
	}
}

func TestHandleCompactUsesUnaryEndpointAndFixedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json" {
			t.Fatalf("CP-COMPACT-001 request=%s accept=%q", r.Method, r.Header.Get("Accept"))
		}
		if r.Header.Get("Authorization") != "Bearer access-token" || r.Header.Get("ChatGPT-Account-ID") != "account-header" {
			t.Fatalf("CP-HDR account headers=%v", r.Header)
		}
		if r.Header.Get("User-Agent") != currentIdentity.UserAgent || r.Header.Get("Originator") != currentIdentity.Originator {
			t.Fatalf("CP-HDR identity=%v", r.Header)
		}
		assertCodexSessionHeaders(t, r.Header, "compact-session-hash")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_compact","object":"response.compaction"}`))
	}))
	defer server.Close()
	previousURL := compactURL
	compactURL = server.URL
	t.Cleanup(func() { compactURL = previousURL })

	upstream := &Upstream{}
	result := event.NewResult(events.TopicCompact, "test", "test")
	upstream.handleCompact(event.NewEventWithContext(events.TopicCompact, "test", "test", nil, context.Background(), events.CompactCommand{
		AccessToken: "access-token", AccountIDHeader: "account-header", Body: []byte(`{"model":"gpt-5.4","input":[]}`), MaxResponseBytes: 1024, SessionHash: "compact-session-hash",
	}), result)
	value, resultErr := result.Get()
	completed, ok := value.(events.CompactResult)
	if resultErr != nil || !ok || completed.ErrorClass != "" || !strings.Contains(string(completed.Body), "resp_compact") {
		t.Fatalf("CP-COMPACT result=%#v err=%v", value, resultErr)
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
