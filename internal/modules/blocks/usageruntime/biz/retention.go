package biz

import (
	"context"
	"log/slog"
	"time"

	retention "aetherrelay/internal/pkg/aetherrelayretention"
)

func (r *Runtime) pruneDetails(ctx context.Context, now time.Time) {
	pruner, ok := r.store.(interface {
		PruneBefore(context.Context, time.Time) (int64, error)
	})
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cutoff := retention.Cutoff(now)
	n, err := pruner.PruneBefore(ctx, cutoff)
	if err != nil {
		slog.Warn("usage retention incomplete; retrying next sweep", "cutoff", cutoff, "deleted", n, "error", err)
	} else if n > 0 {
		slog.Info("usage retention completed", "cutoff", cutoff, "deleted", n, "retention_months", retention.Months)
	}
}
