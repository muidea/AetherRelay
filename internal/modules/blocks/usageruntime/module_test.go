package usageruntime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	configruntime "aetherrelay/internal/modules/blocks/configruntime"
	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	usageevents "aetherrelay/internal/modules/blocks/usageruntime/pkg/events"
	"aetherrelay/internal/pkg/aetherrelaybootstrap"
	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelayusage"
	"github.com/muidea/magicCommon/event"
	plugincommon "github.com/muidea/magicCommon/framework/plugin/common"
	"github.com/muidea/magicCommon/task"
)

func TestClientAPIKeyHintThroughEventHub(t *testing.T) {
	t.Setenv("AETHERRELAY_CREDENTIAL_KEY", "")
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hint.duckdb")
	aetherrelaybootstrap.Configure(configevents.Bootstrap{Config: config.Config{
		State:      config.StateConfig{Database: path, MemoryLimit: "128MB", Threads: 1},
		UsageStore: config.UsageStoreConfig{Path: path, MemoryLimit: "128MB", Threads: 1},
	}})
	hub := event.NewHub(32)
	background := task.NewBackgroundRoutine(32)
	defer hub.Terminate(ctx)
	defer background.Shutdown(ctx)
	manager := plugincommon.NewPluginMgr("usage-hint-test")
	if err := manager.Register(configruntime.New()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(New()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Setup(ctx, hub, background); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := manager.TeardownChecked(ctx); err != nil {
			t.Error(err)
		}
	}()
	store, err := usageevents.RequestStore(ctx, hub, "usage-hint-test")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	record := usage.ClientAPIKeyRecord{ID: "key", Hash: "sha256:old", Hint: "sk_sx****ab", Enabled: true, CreatedAt: at, ProviderAccess: clientaccess.All()}
	if err := store.CreateClientAPIKey(ctx, record); err != nil {
		t.Fatal(err)
	}
	keys, err := store.ListClientAPIKeys(ctx)
	if err != nil || keys[record.ID].Hint != record.Hint || keys[record.ID].Hash != record.Hash {
		t.Fatalf("create lost hint: %+v err=%v", keys[record.ID], err)
	}
	if err := store.RotateClientAPIKey(ctx, record.ID, "sha256:new", "sk_K9****X2", at); err != nil {
		t.Fatal(err)
	}
	keys, err = store.ListClientAPIKeys(ctx)
	if err != nil || keys[record.ID].Hint != "sk_K9****X2" || keys[record.ID].Hash != "sha256:new" {
		t.Fatalf("rotation lost credential pair: %+v err=%v", keys[record.ID], err)
	}
}

func TestFrameworkRetainsUsageOwnerAfterCheckpointFailure(t *testing.T) {
	t.Setenv("AETHERRELAY_CREDENTIAL_KEY", "")
	path := filepath.Join(t.TempDir(), "state.duckdb")
	aetherrelaybootstrap.Configure(configevents.Bootstrap{Config: config.Config{
		State:      config.StateConfig{Database: path, MemoryLimit: "128MB", Threads: 1},
		UsageStore: config.UsageStoreConfig{Path: path, MemoryLimit: "128MB", Threads: 1},
	}})
	hub := event.NewHub(32)
	background := task.NewBackgroundRoutine(32)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(context.Background())
	manager := plugincommon.NewPluginMgr("usage-shutdown-test")
	owner := New()
	if err := manager.Register(configruntime.New()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(owner); err != nil {
		t.Fatal(err)
	}
	if err := manager.Setup(context.Background(), hub, background); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.TeardownChecked(context.Background()); err != nil {
			t.Error(err)
		}
	})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.TeardownChecked(canceled); err == nil {
		t.Fatal("framework swallowed checkpoint failure")
	}
	if owner.bizPtr == nil {
		t.Fatal("framework released the failed owner")
	}
	if err := manager.TeardownChecked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if owner.bizPtr != nil {
		t.Fatal("successful retry did not release owner")
	}
}
