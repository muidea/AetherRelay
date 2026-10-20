package events

import (
	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	"github.com/muidea/magicCommon/event"
)

const TopicProbeAdmission = "aetherrelay.codex.accountpool.command.probe_admission"

// The bounded network refresh must not block the owner's ordinary management
// lane, including a reauthorization that invalidates this probe's epoch.
func BindProbeRefreshLane(ev event.Event) event.Event {
	if cmd, ok := ev.Data().(RefreshTokenCommand); ok && cmd.ProbeLeaseID != "" {
		ev.BindLaneKey(common.UnitID + "/probe-refresh/" + cmd.AccountID)
	}
	return ev
}

// ProbeAdmission is strict and never selects a replacement account. Preflight
// returns no credentials or lease. Acquire atomically rechecks all admission
// facts and reserves concurrency, without production affinity/statistics.
// LeaseID rechecks an existing reservation without acquiring another lease or
// returning credentials. The owner must verify it belongs to AccountID before
// excluding that one reservation from the concurrency check.
type ProbeAdmissionCommand struct {
	Request   LeaseRequest
	AccountID string
	Model     string
	Acquire   bool
	LeaseID   string
}

type ProbeAdmissionResult struct {
	Allowed       bool
	Reason        string
	AccountLabel  string
	ConfigVersion string
	RefreshNeeded bool
	Credential    AcquireResult
}
