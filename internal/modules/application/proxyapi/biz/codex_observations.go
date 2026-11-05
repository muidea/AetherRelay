package biz

import (
	"context"
	"log/slog"
	"time"

	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	"github.com/muidea/magicCommon/event"
)

// Feedback survives a disconnected client, but never inherits an unlimited
// lifetime. The account owner propagates this deadline through lock and SQL waits.
func (s *Proxy) recordCodexAccountObservation(ctx context.Context, topic string, command any) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	result := s.SendEvent(event.NewEventWithContext(topic, s.ID(), acccommon.UnitID, event.NewHeader(), cleanup, command))
	if result == nil {
		slog.Warn("Codex account feedback unavailable", "request_id", codexLeaseRequest(ctx).RequestID, "command", topic)
		return
	}
	if _, err := result.Get(); err != nil {
		slog.Warn("Codex account feedback failed", "request_id", codexLeaseRequest(ctx).RequestID, "command", topic, "error", err)
	}
}
