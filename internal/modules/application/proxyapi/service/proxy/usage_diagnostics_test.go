package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

type failedStartStore struct{ usage.Store }

func (failedStartStore) Start(context.Context, usage.StartRecord) error {
	return fmt.Errorf("SQL secret-driver-value: %w", context.DeadlineExceeded)
}

func TestUsageStartFailureLogsCorrelationWithoutSecrets(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint("missing_store=", missing), func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			called := false
			h := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
				called = true
				return codexresponses.Result{}, nil
			}})
			h.usageStore = failedStartStore{h.usageStore}
			wantReason := "deadline_exceeded"
			if missing {
				h.usageStore = nil
				wantReason = "store_unavailable"
			}
			r := httptest.NewRequest("POST", "/v1/responses?private=secret-query", strings.NewReader(`{"model":"gpt-5.2-codex","input":"secret-body"}`))
			r.Header.Set("Authorization", "Bearer test-client-key")
			r.Header.Set("X-Request-ID", "diagnostic-request")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 503 || called || !strings.Contains(w.Body.String(), ErrorCodeUsageStoreUnavailable) {
				t.Fatalf("status=%d called=%v body=%s", w.Code, called, w.Body.String())
			}
			var record map[string]any
			decoder := json.NewDecoder(&logs)
			found := false
			for decoder.Decode(&record) == nil {
				if record["msg"] == "usage start failed" {
					found = true
					if record["request_id"] != "diagnostic-request" || record["event_id"] == "" || record["event_id"] == nil || record["api_key_id"] == "" || record["api_key_id"] == nil || record["endpoint"] != "/v1/responses" || record["reason"] != wantReason {
						t.Fatalf("missing correlation: %+v", record)
					}
				}
				encoded, _ := json.Marshal(record)
				for _, secret := range []string{"secret-driver-value", "secret-query", "secret-body", "test-client-key"} {
					if bytes.Contains(encoded, []byte(secret)) {
						t.Fatalf("log contains %s", secret)
					}
				}
			}
			if !found {
				t.Fatal("usage start failure was not logged")
			}
		})
	}
}
