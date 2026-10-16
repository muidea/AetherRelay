package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	"aetherrelay/internal/modules/blocks/codexupstream/pkg/steering"
)

type steeringRuntimeStub struct {
	codexAccountRuntimeStub
	calls                int
	slot, model, key, id string
	err                  error
	evidence             steering.Evidence
}

func (s *steeringRuntimeStub) StartCodexSteeringProbe(_ context.Context, slot, model, key string) (proxyevents.SteeringReport, error) {
	s.calls++
	s.slot = slot
	s.model = model
	s.key = key
	return proxyevents.SteeringReport{ProbeID: "probe", TargetSlotID: slot, Model: model}, s.err
}
func (s *steeringRuntimeStub) GetCodexSteeringProbe(_ context.Context, slot, id string) (proxyevents.SteeringReport, error) {
	s.calls++
	s.slot = slot
	s.id = id
	return proxyevents.SteeringReport{Evidence: s.evidence, ProbeID: id, TargetSlotID: slot}, s.err
}

func TestSteeringManagementDiagnosticJSON(t *testing.T) {
	m := steering.NewMachine()
	m.HTTPStatus = 101
	m.Observe([]byte(`{"type":"response.created","response":{"id":"resp_parent"}}`))
	m.WriteAttempt("response.steer")
	m.WriteResult("response.steer", nil)
	m.Observe([]byte(`{"type":"error","status":400,"error":{"code":"future_error","message":"input rejected","param":"input","type":"invalid_request_error"}}`))
	runtime := &steeringRuntimeStub{evidence: m.Evidence}
	h := NewHandler("", &testRuntime{}).WithCodexRuntime(runtime)
	r := httptest.NewRequest(http.MethodGet, "/admin/api/codex/accounts/slot/steering-probes/probe", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var report proxyevents.SteeringReport
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || report.HTTPStatus != 101 || report.UpstreamError == nil || report.UpstreamError.Code != "future_error" || report.UpstreamError.Status != 400 || report.UpstreamError.Message != "input rejected" || !report.SteerWriteCompleted || len(report.Timeline) == 0 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestSteeringManagementHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body, key string
		mutation                      bool
		want                          int
	}{
		{"start", "POST", "slot/steering-probes", `{"model":"gpt-test"}`, "one", true, 202},
		{"get", "GET", "slot/steering-probes/probe", "", "", false, 200},
		{"latest", "GET", "slot/steering-probes/latest", "", "", false, 200},
		{"missing_key", "POST", "slot/steering-probes", `{"model":"gpt-test"}`, "", true, 400},
		{"missing_model", "POST", "slot/steering-probes", `{}`, "one", true, 400},
		{"override", "POST", "slot/steering-probes", `{"model":"gpt-test","proxy":"http://untrusted"}`, "one", true, 400},
		{"extra_json", "POST", "slot/steering-probes", `{"model":"gpt-test"}{}`, "one", true, 400},
		{"mutation_guard", "POST", "slot/steering-probes", `{"model":"gpt-test"}`, "one", false, 403},
		{"invalid_path", "POST", "slot/steering-probes/extra", `{"model":"gpt-test"}`, "one", true, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &steeringRuntimeStub{}
			h := NewHandler("", &testRuntime{}).WithCodexRuntime(runtime)
			req := httptest.NewRequest(tc.method, "/admin/api/codex/accounts/"+tc.path, strings.NewReader(tc.body))
			req.RemoteAddr = "127.0.0.1:1234"
			if tc.mutation {
				req.Header.Set("X-AetherRelay-Admin", "1")
			}
			req.Header.Set("Idempotency-Key", tc.key)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
			if tc.want >= 400 && runtime.calls != 0 {
				t.Fatal("invalid request executed probe")
			}
			if tc.want < 300 && (runtime.calls != 1 || runtime.slot != "slot" || w.Header().Get("Cache-Control") != "no-store") {
				t.Fatal(runtime, w.Header())
			}
		})
	}
}
func TestSteeringManagementErrorStatusAndAuthentication(t *testing.T) {
	for _, status := range []int{409, 429, 404, 503} {
		runtime := &steeringRuntimeStub{err: &proxyevents.SteeringAPIError{Status: status, Code: "bounded_error"}}
		h := NewHandler("", &testRuntime{}).WithCodexRuntime(runtime)
		req := httptest.NewRequest(http.MethodGet, "/admin/api/codex/accounts/slot/steering-probes/id", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	runtime := &steeringRuntimeStub{}
	h := NewHandler("", &testRuntime{}).WithCodexRuntime(runtime)
	req := httptest.NewRequest(http.MethodGet, "/admin/api/codex/accounts/slot/steering-probes/id", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == 200 || runtime.calls != 0 {
		t.Fatal("remote request bypassed admin authentication")
	}
}
