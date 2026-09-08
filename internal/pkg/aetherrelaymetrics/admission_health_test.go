package metrics

import (
	"testing"
	"time"
)

func TestLocalAdmissionDoesNotExtendOpenCircuit(t *testing.T) {
	// CP-FAIL-019: preserve the real failure interval and allow half-open recovery.
	r := NewRegistry()
	for range 3 {
		r.RecordRequestPlan("codexoauth", "m", "responses", 502, time.Second, "upstream_failed", "", "", "", "")
	}
	before := r.ProviderHealthSnapshot()["codexoauth"]
	for range 10 {
		r.RecordRequestPlan("codexoauth", "m", "responses", 503, time.Millisecond, "provider_unavailable", "", "", "", "")
	}
	after := r.ProviderHealthSnapshot()["codexoauth"]
	if after.Failures != before.Failures || !after.CircuitRetryAt.Equal(before.CircuitRetryAt) {
		t.Fatalf("denial extended circuit: before=%+v after=%+v", before, after)
	}
	r.mu.Lock()
	v := r.providerHealth["codexoauth"]
	v.CircuitOpenUntil = time.Now().Add(-time.Second)
	r.providerHealth["codexoauth"] = v
	r.mu.Unlock()
	if r.ProviderHealthSnapshot()["codexoauth"].CircuitState != "half_open" {
		t.Fatal("no recovery probe")
	}
	r.RecordRequestPlan("codexoauth", "m", "responses", 200, time.Second, "success", "", "", "", "")
	if got := r.ProviderHealthSnapshot()["codexoauth"]; got.CircuitState != "closed" || got.ConsecutiveFailures != 0 {
		t.Fatalf("recovery failed: %+v", got)
	}
}
