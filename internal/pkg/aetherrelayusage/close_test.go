package usage

import (
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestCanceledCloseRetainsDatabaseForRetry(t *testing.T) {
	cfg := config.UsageStoreConfig{Path: filepath.Join(t.TempDir(), "usage.duckdb"), MemoryLimit: "128MB", Threads: 1}
	store, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureClientAPIKey(context.Background(), "retained", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.CloseContext(ctx); err == nil {
		t.Fatal("canceled checkpoint reported success")
	}
	if store.closed.Load() {
		t.Fatal("failed checkpoint closed store")
	}
	if err := store.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	keys, err := reopened.ListClientAPIKeys(context.Background())
	if err != nil || keys["retained"].ID != "retained" {
		t.Fatalf("keys lost: %v", err)
	}
}
