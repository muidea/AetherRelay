package biz

import (
	"context"
	"log/slog"
	"time"

	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

// Runtime is the Usage Block's private DuckDB lifecycle owner.
type Runtime struct{ store usage.Store }

func NewRuntime(bootstrap configevents.Bootstrap) (*Runtime, error) {
	store, err := usage.OpenDuckDB(bootstrap.Config.UsageStore)
	if err != nil {
		return nil, err
	}
	// Admin feature/tool calls use a server-owned, stable scope.  Materialize
	// its metadata before any Application module reads the client-key catalog;
	// EnsureClientAPIKey is idempotent and intentionally stores no raw secret.
	if err := store.EnsureClientAPIKey(context.Background(), config.BuiltinClientAPIKeyID, time.Now().UTC()); err != nil {
		_ = store.Close()
		return nil, err
	}
	return &Runtime{store: store}, nil
}

func (r *Runtime) Store() usage.Store {
	if r == nil {
		return nil
	}
	return r.store
}

func (r *Runtime) Close(ctx context.Context) {
	if r == nil || r.store == nil {
		return
	}
	checkpointCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if err := r.store.Checkpoint(checkpointCtx); err != nil {
		slog.Error("usage store checkpoint failed", slog.Any("error", err))
	}
	cancel()
	_ = r.store.Close()
	r.store = nil
}
