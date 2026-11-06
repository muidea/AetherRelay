package events

import (
	"aetherrelay/internal/modules/blocks/usageruntime/pkg/common"

	"github.com/muidea/magicCommon/event"
)

// bindCommandLane bounds analytical reads to one lane without making durable
// request accounting wait for them. Key lifecycle commands, recovery and
// checkpoint retain the owner's original ordering with Start/Complete/WriteTrace.
func bindCommandLane(ev event.Event) {
	switch ev.ID() {
	case TopicDashboard, TopicCount, TopicEvents, TopicExport, TopicFilterOptions,
		TopicAllTime, TopicExecutionTrace, TopicAnomalies:
		ev.BindLaneKey(common.UnitID + "/query")
	default:
		ev.BindLaneKey(common.UnitID)
	}
}
