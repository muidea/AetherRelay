package proxy

import (
	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCodexCompletionDeadlineReservesDeliveryAndNeverExtendsBudget(t *testing.T) {
	h, _ := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{})
	h.cfg.RequestTimeout = 300 * time.Second
	started := time.Now().Add(-time.Second)
	for _, tc := range []struct {
		hint string
		want time.Duration
	}{
		{"300", 295 * time.Second}, {"30", 28500 * time.Millisecond}, {"900", 300 * time.Second},
		{"", 300 * time.Second}, {"bad", 300 * time.Second}, {"NaN", 300 * time.Second},
		{"+Inf", 300 * time.Second}, {"-1", 300 * time.Second}, {"0", 300 * time.Second}, {"1e99", 300 * time.Second},
	} {
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		r.Header.Set("X-Stainless-Timeout", tc.hint)
		if got := h.codexCompletionDeadline(r, started); !got.Equal(started.Add(tc.want)) {
			t.Errorf("hint %q: budget=%s want=%s", tc.hint, got.Sub(started), tc.want)
		}
	}
}

func TestLocalCodexCompletionTimeoutSettles504WithoutProviderPenalty(t *testing.T) {
	failure := codexresponses.NewFailure(codexresponses.KindTimeout, 0, context.DeadlineExceeded)
	failure.RequestBudgetExceeded = true
	retryable := false
	failure.Retryable = &retryable
	failure.Attempt = codexArchiveTestAttempt()
	h, _ := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{complete: func(_ context.Context, req codexresponses.Request) (codexresponses.Result, error) {
		if req.Deadline.IsZero() {
			t.Error("adapter omitted total request deadline")
		}
		return codexresponses.Result{}, failure
	}})
	h.cfg.RequestTimeout = time.Second
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"gpt-5.2-codex","stream":false,"messages":[{"role":"user","content":"test"}]}`))
	r.Header.Set("Authorization", "Bearer test-client-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 504 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	events := usageEvents(t, h.usageStore)
	if len(events) != 1 || events[0].Outcome != "request_timeout" || events[0].ErrorCode != "timeout" || events[0].UpstreamStatus != 200 || events[0].FailureClass != "request_budget_exceeded" {
		t.Fatalf("events=%+v", events)
	}
	if !strings.Contains(w.Body.String(), "effective client/gateway request budget") {
		t.Fatalf("response lacks budget explanation: %s", w.Body.String())
	}
	native := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.2-codex","stream":false,"input":"test"}`))
	native.Header.Set("Authorization", "Bearer test-client-key")
	nativeResponse := httptest.NewRecorder()
	h.ServeHTTP(nativeResponse, native)
	if nativeResponse.Code != 504 || !strings.Contains(nativeResponse.Body.String(), "request_budget_exceeded") {
		t.Fatalf("native response lacks budget classification: %s", nativeResponse.Body.String())
	}
	if streamFailFromCodex(failure).CountUpstream {
		t.Fatal("local request budget counted as upstream failure")
	}
	if streamFailFromCodex(codexresponses.NewFailure(codexresponses.KindTimeout, 0, nil)).Kind != streamKindUpstreamFailed {
		t.Fatal("upstream timeout lost classification")
	}
}

func TestCodexCompletionBudgetKeepsClientServerAndContextLimits(t *testing.T) {
	h, _ := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{})
	started := time.Now()
	for _, tc := range []struct {
		server time.Duration
		client string
		parent time.Duration
		want   time.Duration
		source string
	}{
		{600 * time.Second, "300", 0, 295 * time.Second, "client"},
		{600 * time.Second, "600", 0, 595 * time.Second, "client"},
		{300 * time.Second, "600", 0, 300 * time.Second, "server"},
		{600 * time.Second, "600", 200 * time.Second, 200 * time.Second, "context"},
		{0, "", 0, 0, "unbounded"},
	} {
		h.cfg.RequestTimeout = tc.server
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		r.Header.Set("X-Stainless-Timeout", tc.client)
		if tc.parent > 0 {
			ctx, cancel := context.WithDeadline(r.Context(), started.Add(tc.parent))
			defer cancel()
			r = r.WithContext(ctx)
		}
		got := h.codexCompletionBudget(r, started)
		if got.source != tc.source {
			t.Errorf("source=%s want=%s", got.source, tc.source)
		}
		if tc.want == 0 {
			if !got.deadline.IsZero() {
				t.Error("unbounded deadline was set")
			}
		} else if !got.deadline.Equal(started.Add(tc.want)) {
			t.Errorf("budget=%s want=%s", got.deadline.Sub(started), tc.want)
		}
	}
}

func TestCodexCompletionBudgetDiagnosticsPreserveCompleteRequest(t *testing.T) {
	h, _ := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{})
	h.cfg.RequestTimeout = 300 * time.Second
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer private-header-sentinel")
	r.Header.Set("X-Stainless-Timeout", "300")
	var bodies []string
	for _, effort := range []string{"high", "custom-effort-value"} {
		body := `{"input":[{"content":"` + strings.Repeat("complete-prompt-content-", 4096) + `end-of-prompt"}],"tools":[{"name":"actual-tool-name","parameters":{"description":"complete tool schema"}}],"reasoning":{"effort":"` + effort + `"}}`
		bodies = append(bodies, body)
		request := codexresponses.Request{Body: []byte(body)}
		h.prepareCodexCompletion(r, time.Now(), &request)
		if request.Deadline.IsZero() {
			t.Error("deadline omitted")
		}
	}
	if strings.Contains(logs.String(), "private-header-sentinel") {
		t.Error("authentication header was logged")
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != len(bodies) {
		t.Fatalf("log entries=%d want=%d", len(lines), len(bodies))
	}
	for i, line := range lines {
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatal(err)
		}
		if fields["request_body"] != bodies[i] {
			t.Error("request body was masked or truncated")
		}
		if fields["request_budget_ms"] != float64(295000) || fields["request_budget_source"] != "client" || fields["input_items"] != float64(1) || fields["tool_count"] != float64(1) || fields["client_timeout"] != "300" {
			t.Errorf("missing budget/context fields")
		}
	}
	if !strings.Contains(logs.String(), `"reasoning_effort":"custom-effort-value"`) {
		t.Error("actual effort was replaced")
	}

}
