package biz

import (
	"bytes"
	"context"
	"fmt"
	"time"

	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
)

type accountSlotsSnapshot struct {
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
	accevents.LeaseSnapshotResult
}

// Pull the authoritative owner on every scrape. No asynchronous mirror can
// silently lose a release or retain a stale occupancy after account removal.
func (s *MetricsRuntime) accountSlots(parent context.Context) accountSlotsSnapshot {
	out := accountSlotsSnapshot{LeaseSnapshotResult: accevents.LeaseSnapshotResult{Accounts: []accevents.AccountSlots{}}}
	if s.EventHub() == nil {
		out.Error = "Codex account pool EventHub unavailable"
		return out
	}
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	result := s.SendEvent(event.NewEventWithContext(accevents.TopicLeaseSnapshot, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.LeaseSnapshotCommand{}))
	if result == nil {
		out.Error = "missing Codex lease snapshot result"
		return out
	}
	value, err := result.Get()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	snapshot, ok := value.(accevents.LeaseSnapshotResult)
	if !ok {
		out.Error = "invalid Codex lease snapshot result"
		return out
	}
	out.Available, out.LeaseSnapshotResult = true, snapshot
	return out
}

func writeAccountSlots(b *bytes.Buffer, s accountSlotsSnapshot) {
	fmt.Fprintln(b, "# HELP aetherrelay_codex_account_slots_available Whether the authoritative slot snapshot is available.")
	fmt.Fprintln(b, "# TYPE aetherrelay_codex_account_slots_available gauge")
	if !s.Available {
		fmt.Fprintln(b, "aetherrelay_codex_account_slots_available 0")
		return
	}
	fmt.Fprintln(b, "aetherrelay_codex_account_slots_available 1")
	for _, metric := range []struct{ name, help, kind string }{
		{"limit", "Configured account concurrency limit.", "gauge"},
		{"inflight", "Account slots currently held by all protocols.", "gauge"},
		{"websocket_active", "Slots held by active native WebSocket turns.", "gauge"},
		{"websocket_idle", "Slots held by native WebSockets between turns.", "gauge"},
		{"oldest_lease_seconds", "Age of the oldest held account lease.", "gauge"},
		{"release_failures_total", "Release attempt failures reported to the account owner in this process.", "counter"},
	} {
		name := "aetherrelay_codex_account_" + metric.name
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, metric.help, name, metric.kind)
		for _, a := range s.Accounts {
			var value string
			switch metric.name {
			case "limit":
				value = fmt.Sprint(a.MaxConcurrency)
			case "inflight":
				value = fmt.Sprint(a.Inflight)
			case "websocket_active":
				value = fmt.Sprint(a.WebsocketActive)
			case "websocket_idle":
				value = fmt.Sprint(a.WebsocketIdle)
			case "oldest_lease_seconds":
				value = fmt.Sprintf("%.3f", float64(a.OldestLeaseMS)/1000)
			case "release_failures_total":
				value = fmt.Sprint(a.ReleaseFailures)
			}
			// Account IDs are owner-generated. Request/lease IDs are deliberately
			// absent from labels; their detail belongs in /stats and busy logs.
			fmt.Fprintf(b, "%s{account_id=%q} %s\n", name, a.AccountID, value)
		}
	}
}
