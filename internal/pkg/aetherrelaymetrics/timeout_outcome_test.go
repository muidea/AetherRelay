package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestTimeoutOutcomeKeepsHealthSemantics(t *testing.T) {
	for _, status := range []int{200, 504} {
		if shouldTrackProviderHealth(status, "first_event_timeout") || retryableHealthFailure(status, "first_event_timeout") {
			t.Fatalf("first-event budget poisoned health: %d", status)
		}
		if !shouldTrackProviderHealth(status, "idle_timeout") || !retryableHealthFailure(status, "idle_timeout") {
			t.Fatalf("idle timeout lost health semantics: %d", status)
		}
	}
}

func TestFirstEventTimeoutRetainsMetricsWithoutOpeningCircuit(t *testing.T) {
	r := NewRegistry()
	r.RecordRequest("codexoauth", "gpt-6-astra", "messages", 200, time.Second, "success")
	for range 3 {
		r.RecordRequest("codexoauth", "gpt-6-astra", "messages", 504, 300*time.Second, "first_event_timeout")
	}
	modelHealth, _ := r.ProviderModelHealth("codexoauth", "gpt-6-astra")
	for _, health := range []StatsProviderHealth{r.ProviderHealthSnapshot()["codexoauth"], modelHealth} {
		if health.Failures != 0 || health.ConsecutiveFailures != 0 || health.CircuitState != "closed" {
			t.Fatalf("first-event timeouts opened circuit: %+v", health)
		}
	}
	var exposition strings.Builder
	r.WritePrometheus(&exposition)
	found := false
	for _, line := range strings.Split(exposition.String(), "\n") {
		if strings.HasPrefix(line, "aetherrelay_requests_total{") && strings.Contains(line, `outcome="first_event_timeout"`) && strings.HasSuffix(line, "} 3") {
			found = true
		}
	}
	if !found {
		t.Fatalf("timeout request metrics missing: %s", exposition.String())
	}
	for range 3 {
		r.RecordRequest("codexoauth", "gpt-6-astra", "messages", 502, time.Second, "upstream_failed")
	}
	if health := r.ProviderHealthSnapshot()["codexoauth"]; health.CircuitState != "open" || health.ConsecutiveFailures != 3 {
		t.Fatalf("upstream failures must still open circuit: %+v", health)
	}
}
