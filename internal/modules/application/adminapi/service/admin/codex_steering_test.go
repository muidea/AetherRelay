package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
)

type steeringRuntimeStub struct {
	codexAccountRuntimeStub
	calls                int
	slot, model, key, id string
	err                  error
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
	return proxyevents.SteeringReport{ProbeID: id, TargetSlotID: slot}, s.err
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
