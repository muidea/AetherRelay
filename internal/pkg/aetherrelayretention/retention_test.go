package retention

import (
	"testing"
	"time"
)

func TestCalendarCutoff(t *testing.T) {
	for _, tc := range []struct{ now, want string }{
		{"2026-10-30T05:30:01Z", "2026-07-30T05:30:01Z"},
		{"2026-05-31T05:30:01Z", "2026-02-28T05:30:01Z"},
		{"2024-05-31T05:30:01Z", "2024-02-29T05:30:01Z"},
		// The calendar day has already changed in Singapore.
		{"2026-05-30T18:00:00Z", "2026-02-27T18:00:00Z"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		if got := Cutoff(now).Format(time.RFC3339); got != tc.want {
			t.Fatalf("%s -> %s, want %s", tc.now, got, tc.want)
		}
	}
}
