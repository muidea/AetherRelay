package proxy

import (
	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"context"
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
	if len(events) != 1 || events[0].Outcome != "request_timeout" || events[0].ErrorCode != "timeout" || events[0].UpstreamStatus != 200 {
		t.Fatalf("events=%+v", events)
	}
	if streamFailFromCodex(failure).CountUpstream {
		t.Fatal("local request budget counted as upstream failure")
	}
	if streamFailFromCodex(codexresponses.NewFailure(codexresponses.KindTimeout, 0, nil)).Kind != streamKindUpstreamFailed {
		t.Fatal("upstream timeout lost classification")
	}
}
