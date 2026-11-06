package biz

import (
	"context"
	"errors"
	"log/slog"
	"time"

	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

const codexAccountCommandSlow = 3 * time.Second

// The timer reports an outstanding command even if its handler never returns.
// Duration includes admission, queueing and execution; it is not a lock metric.
func (s *Proxy) sendCodexAccountCommand(ev event.Event, stage, accountID string) (any, *cd.Error) {
	started := time.Now()
	identity := codexLeaseRequest(ev.Context())
	logger := slog.Default().With("stage", stage, "account_id", accountID,
		"request_id", identity.RequestID, "event_id", identity.EventID, "api_key_id", identity.APIKeyID,
		"endpoint", identity.Endpoint)
	if progressID, _ := ev.Context().Value(codexUsageProgressKey{}).(string); progressID != "" {
		logger = logger.With("progress_id", progressID)
	}
	reported := make(chan struct{})
	timer := time.AfterFunc(codexAccountCommandSlow, func() {
		defer close(reported)
		logger.Warn("Codex account command still waiting", "duration_ms", time.Since(started).Milliseconds())
	})
	value, err := s.SendEvent(ev).Get()
	if !timer.Stop() {
		<-reported
	}
	if err != nil || time.Since(started) >= codexAccountCommandSlow {
		reason := codexAccountCommandFailure(ev.Context(), err)
		unavailable := ""
		if out, ok := value.(accevents.AcquireResult); ok {
			unavailable = out.UnavailableReason
		}
		code := 0
		if err != nil {
			code = int(err.Code)
		}
		logger.Warn("Codex account command completed", "duration_ms", time.Since(started).Milliseconds(),
			"reason", reason, "command_error_code", code, "unavailable_reason", unavailable)
	}
	return value, err
}

func codexAccountCommandFailure(ctx context.Context, err *cd.Error) string {
	if err == nil {
		return "completed"
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if ctx.Err() != nil {
		return "canceled"
	}
	// Only framework-owned, fixed messages are mapped. Never log raw command
	// errors: storage and OAuth errors may contain credential or SQL values.
	switch err.Message {
	case "event admission timed out or was canceled":
		return "command_admission_timeout"
	case "event canceled before dispatch", "event context expired before dispatch", "event context is canceled":
		return "command_canceled_before_dispatch"
	default:
		return "command_failed"
	}
}
