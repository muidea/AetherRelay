package biz

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	value, class, observation, err := completedResponse(jsonResponse, 1024)
	if err != nil || class != "" || observation.UsageLimited || string(value) != `{"object":"response","id":"resp_1"}` {
		t.Fatalf("json completed response = %s class=%s observation=%+v err=%v", value, class, observation, err)
	}
	sseResponse := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"id\":\"resp_2\"}}\n\n"))}
	value, class, observation, err = completedResponse(sseResponse, 1024)
	if err != nil || class != "" || observation.UsageLimited || !strings.Contains(string(value), `"resp_2"`) {
		t.Fatalf("sse completed response = %s class=%s observation=%+v err=%v", value, class, observation, err)
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

func TestListModelsUsesAccountHeadersAndProjectsSafeModelIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("client_version") != "0.135.0" {
			t.Fatalf("request=%s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer access-token" || r.Header.Get("ChatGPT-Account-ID") != "account-header" {
			t.Fatalf("account headers=%v", r.Header)
		}
		if r.Header.Get("User-Agent") != codexUserAgent || r.Header.Get("Originator") != codexOriginator {
			t.Fatalf("Codex identity headers=%v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.2-codex","created":7,"owned_by":"openai"},{"id":"gpt-5.3-codex"},{"slug":"gpt-5.2-codex"},{"slug":"bad\nmodel"}]}`))
	}))
	defer server.Close()
	previousURL := modelsURL
	modelsURL = server.URL + "?client_version=0.135.0"
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
		if r.Header.Get("User-Agent") != codexUserAgent || r.Header.Get("Originator") != codexOriginator || r.Header.Get("Content-Type") != "application/json" {
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
