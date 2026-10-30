// Package retention defines the shared retention window for call evidence.
package retention

import "time"

const Months = 3
const Interval = time.Hour

var timezone = time.FixedZone("Asia/Singapore", 8*60*60)

// Cutoff subtracts calendar months in the management timezone, clamping the
// day to the target month's last day (May 31 -> February 28, for example).
func Cutoff(now time.Time) time.Time {
	now = now.In(timezone)
	first := time.Date(now.Year(), now.Month()-Months, 1, now.Hour(), now.Minute(), now.Second(), now.Nanosecond(), timezone)
	lastDay := first.AddDate(0, 1, -1).Day()
	return first.AddDate(0, 0, min(now.Day(), lastDay)-1).UTC()
}
