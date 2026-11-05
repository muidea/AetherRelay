package events

import (
	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	"github.com/muidea/magicCommon/event"
)

// BindCommandLane isolates in-memory lease release from network refresh and
// persistent account mutations. Refresh singleflight remains owned by the pool.
func BindCommandLane(ev event.Event) event.Event {
	switch ev.ID() {
	case TopicRelease:
		ev.BindLaneKey(common.UnitID + "/release")
	case TopicLeaseState, TopicReleaseFailure:
		ev.BindLaneKey(common.UnitID + "/" + ev.ID())
	case TopicRefreshToken:
		if cmd, ok := ev.Data().(RefreshTokenCommand); ok {
			if cmd.ProbeLeaseID != "" {
				return BindProbeRefreshLane(ev)
			}
			ev.BindLaneKey(common.UnitID + "/refresh/" + cmd.AccountID)
		}
	case TopicRefreshByID, TopicOAuthFinish:
		ev.BindLaneKey(common.UnitID + "/network/" + ev.ID())
	}
	return ev
}
