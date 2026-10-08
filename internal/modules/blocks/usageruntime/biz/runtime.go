package biz

import (
	"context"
	"fmt"
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

func (r *Runtime) Close(ctx context.Context) error {
	if r == nil || r.store == nil {
		return nil
	}
	if closer, ok := r.store.(interface{ CloseContext(context.Context) error }); ok {
		if err := closer.CloseContext(ctx); err != nil {
			return fmt.Errorf("close usage store: %w", err)
		}
	} else {
		if err := r.store.Checkpoint(ctx); err != nil {
			return fmt.Errorf("checkpoint usage store: %w", err)
		}
		if err := r.store.Close(); err != nil {
			return fmt.Errorf("close usage store: %w", err)
		}
	}
	r.store = nil
	return nil
}
