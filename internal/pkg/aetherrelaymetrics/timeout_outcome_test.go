package metrics

import "testing"

func TestTimeoutOutcomeKeepsHealthSemantics(t *testing.T) {
	for _, outcome := range []string{"first_event_timeout", "idle_timeout"} {
		for _, status := range []int{200, 504} {
			if !shouldTrackProviderHealth(status, outcome) || !retryableHealthFailure(status, outcome) {
				t.Fatalf("timeout lost health semantics: %d %s", status, outcome)
			}
		}
	}
}
