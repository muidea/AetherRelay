package store

import (
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

// CP-SCHED-012: account-specific limits normalize load before session counts.
func TestSchedulingUsesAccountSlotUtilization(t *testing.T) {
	s, ids, _ := quotaEvidenceStore(t, func([]events.AccountView, time.Time) map[string]events.AccountUsageSnapshot { return nil })
	limit := 4
	if _, err := s.Update(ids[1], nil, nil, nil, &limit); err != nil {
		t.Fatal(err)
	}
	loads := map[string]SchedulingLoad{ids[0]: {Inflight: 1}, ids[1]: {Inflight: 1, Sessions: 3}}
	lease, err := s.AcquireScheduledTransport("gpt-5.5", nil, nil, "", events.TransportResponses, events.ClientIdentityCandidate{}, loads)
	if err != nil {
		t.Fatal(err)
	}
	if lease.AccountID != ids[1] {
		t.Fatal("1/4 load must outrank 1/2 load")
	}
	loads[ids[1]] = SchedulingLoad{Inflight: 2, Sessions: 3}
	lease, err = s.AcquireScheduledTransport("gpt-5.5", nil, nil, "", events.TransportResponses, events.ClientIdentityCandidate{}, loads)
	if err != nil {
		t.Fatal(err)
	}
	if lease.AccountID != ids[0] {
		t.Fatal("equal utilization must prefer fewer bound sessions")
	}
}
