package biz

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

func TestStartupAndMaintenanceApplyRetention(t *testing.T) {
	ctx := context.Background()
	bootstrap := configevents.Bootstrap{Config: config.Config{UsageStore: config.UsageStoreConfig{Path: filepath.Join(t.TempDir(), "retention.duckdb"), MemoryLimit: "128MB", Threads: 1}}}
	s, err := usage.OpenDuckDB(bootstrap.Config.UsageStore)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(store usage.Store, id string, at time.Time) {
		t.Helper()
		if err := store.Start(ctx, usage.StartRecord{EventID: id, APIKeyID: "key", StartedAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := store.Complete(ctx, usage.CompleteRecord{EventID: id, HTTPStatus: 200, Outcome: "success", CompletedAt: at.Add(time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	insert(s, "expired", now.AddDate(0, -4, 0))
	insert(s, "recent", now)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(ctx)
	if n, err := runtime.Store().Count(ctx, usage.UsageFilter{AllTime: true}); err != nil || n != 1 {
		t.Fatalf("startup retention=%d %v", n, err)
	}
	insert(runtime.Store(), "late-expired", now.AddDate(0, -4, 0))
	runtime.pruneDetails(ctx, now)
	if n, err := runtime.Store().Count(ctx, usage.UsageFilter{AllTime: true}); err != nil || n != 1 {
		t.Fatalf("maintenance retention=%d %v", n, err)
	}
}
