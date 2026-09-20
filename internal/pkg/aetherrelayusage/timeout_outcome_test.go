package usage

import "testing"

func TestTimeoutOutcomesAvailableToAdminFilters(t *testing.T) {
	for _, want := range []string{"first_event_timeout", "idle_timeout"} {
		found := false
		for _, got := range KnownOutcomes() {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing outcome filter: %s", want)
		}
	}
}
