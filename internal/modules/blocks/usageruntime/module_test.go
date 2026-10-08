package usageruntime

import (
	"context"
	"path/filepath"
	"testing"

	configruntime "aetherrelay/internal/modules/blocks/configruntime"
	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	"aetherrelay/internal/pkg/aetherrelaybootstrap"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	"github.com/muidea/magicCommon/event"
	plugincommon "github.com/muidea/magicCommon/framework/plugin/common"
	"github.com/muidea/magicCommon/task"
)

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
