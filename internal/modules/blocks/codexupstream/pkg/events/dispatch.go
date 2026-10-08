package events

import (
	"context"
	"time"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	"github.com/google/uuid"
	"github.com/muidea/magicCommon/event"
)

type submittedAtKey struct{}

// BindCommandLane is the upstream owner's scheduling contract. Independent
// network operations have independent lanes. Reads remain ordered per stream;
// cancellation never waits behind a read. Websocket writes remain ordered.
func BindCommandLane(ev event.Event) event.Event {
	lane := common.UnitID + "/request/" + uuid.NewString()
	switch cmd := ev.Data().(type) {
	case PullCommand:
		lane = common.UnitID + "/stream/read/" + cmd.StreamID
	case CancelCommand:
		lane = common.UnitID + "/stream/cancel/" + cmd.StreamID
	case WSPullCommand:
		lane = common.UnitID + "/websocket/read/" + cmd.SessionID
	case WSSendCommand:
		lane = common.UnitID + "/websocket/write/" + cmd.SessionID
	case WSCloseCommand:
		lane = common.UnitID + "/websocket/close/" + cmd.SessionID
	}
	ev.BindLaneKey(lane)
	ev.BindContext(context.WithValue(ev.Context(), submittedAtKey{}, time.Now()))
	return ev
}

// QueueWait is measured at handler entry, separately from network latency.
func QueueWait(ctx context.Context) time.Duration {
	at, ok := ctx.Value(submittedAtKey{}).(time.Time)
	if !ok {
		return 0
	}
	return time.Since(at)
}
