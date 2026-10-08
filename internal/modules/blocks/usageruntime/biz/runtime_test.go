package biz

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

func TestNewRuntimeEnsuresBuiltinLocalScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.duckdb")
	bootstrap := configevents.Bootstrap{Config: config.Config{UsageStore: config.UsageStoreConfig{
		Path: path, MemoryLimit: "256MB", Threads: 1, QueryCacheSeconds: 0,
	}}}

	runtime, err := NewRuntime(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	records, err := runtime.Store().ListClientAPIKeys(context.Background())
	if err != nil {
		runtime.Close(context.Background())
		t.Fatal(err)
	}
	record, ok := records[config.BuiltinClientAPIKeyID]
	if !ok {
		runtime.Close(context.Background())
		t.Fatalf("built-in key %q was not materialized: %#v", config.BuiltinClientAPIKeyID, records)
	}
	if record.ID != config.BuiltinClientAPIKeyID || !record.Enabled || record.Hash != "" || record.ProviderAccess.Mode != clientaccess.ModeAll || record.CreatedAt.IsZero() {
		runtime.Close(context.Background())
		t.Fatalf("built-in record contains unexpected credential/access data: %#v", record)
	}
	createdAt := record.CreatedAt
	runtime.Close(context.Background())

	// A second startup must keep the same metadata row rather than creating a
	// second scope or replacing its lifecycle timestamp.
	second, err := NewRuntime(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(context.Background())
	secondRecords, err := second.Store().ListClientAPIKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(secondRecords) != 1 || secondRecords[config.BuiltinClientAPIKeyID].CreatedAt != createdAt {
		t.Fatalf("startup ensure was not idempotent: %#v", secondRecords)
	}
}

func TestNewRuntimeCanonicalizesBuiltinLocalScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.duckdb")
	bootstrap := configevents.Bootstrap{Config: config.Config{UsageStore: config.UsageStoreConfig{
		Path: path, MemoryLimit: "256MB", Threads: 1, QueryCacheSeconds: 0,
	}}}
	store, err := usage.OpenDuckDB(bootstrap.Config.UsageStore)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := clientaccess.Selected([]string{"external"})
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.CreateClientAPIKey(context.Background(), usage.ClientAPIKeyRecord{
		ID: config.BuiltinClientAPIKeyID, Hash: "sha256:" + strings.Repeat("a", 64), Enabled: false, CreatedAt: time.Now().UTC(), ProviderAccess: policy,
	}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	runtime, err := NewRuntime(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	records, err := runtime.Store().ListClientAPIKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	record := records[config.BuiltinClientAPIKeyID]
	if record.Hash != "" || !record.Enabled || record.RevokedAt != nil || record.LastRotatedAt != nil || record.ProviderAccess.Mode != clientaccess.ModeAll || len(record.ProviderAccess.ProviderIDs) != 0 {
		t.Fatalf("built-in scope was not canonicalized: %#v", record)
	}
}

// A failure receipt must preserve ownership until a later successful attempt.
type failingCloseStore struct {
	usage.Store
	checkpointErr error
	closeErr      error
	closes        int
}

func (s *failingCloseStore) Checkpoint(context.Context) error { return s.checkpointErr }
func (s *failingCloseStore) Close() error                     { s.closes++; return s.closeErr }

func TestRuntimeCloseRetainsStoreOnFailure(t *testing.T) {
	store := &failingCloseStore{checkpointErr: fmt.Errorf("checkpoint failed")}
	runtime := &Runtime{store: store}
	if err := runtime.Close(context.Background()); err == nil {
		t.Fatal("checkpoint failure swallowed")
	}
	if runtime.Store() != store || store.closes != 0 {
		t.Fatal("store released after checkpoint failure")
	}
	store.checkpointErr = nil
	store.closeErr = fmt.Errorf("close failed")
	if err := runtime.Close(context.Background()); err == nil {
		t.Fatal("close failure swallowed")
	}
	if runtime.Store() != store {
		t.Fatal("store released after close failure")
	}
	store.closeErr = nil
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.Store() != nil {
		t.Fatal("successful close retained store")
	}
}
