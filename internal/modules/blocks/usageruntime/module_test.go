package usageruntime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	configruntime "aetherrelay/internal/modules/blocks/configruntime"
	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	usageevents "aetherrelay/internal/modules/blocks/usageruntime/pkg/events"
	"aetherrelay/internal/pkg/aetherrelaybootstrap"
	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	trace "aetherrelay/internal/pkg/aetherrelaytrace"
	"aetherrelay/internal/pkg/aetherrelayusage"
	"github.com/muidea/magicCommon/event"
	plugincommon "github.com/muidea/magicCommon/framework/plugin/common"
	"github.com/muidea/magicCommon/task"
)

func TestClientAPIKeyHintAndSettlementThroughEventHub(t *testing.T) {
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
	if err := store.Start(ctx, usage.StartRecord{EventID: "request", APIKeyID: "key", StartedAt: at}); err != nil {
		t.Fatal(err)
	}

	tr := trace.Trace{Context: trace.Context{RequestID: "request-id", Revision: "revision"}, Attempts: []trace.Attempt{{ID: 1, Finished: true, Failed: true, FailureClass: "network", Transport: "websocket"}, {ID: 2, Finished: true, Transport: "http"}}}
	if err := store.WriteTrace(ctx, "request", tr); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := store.ExecutionTrace(ctx, "request")
	if err != nil || checkpoint == nil || len(checkpoint.Attempts) != 2 {
		t.Fatalf("typed checkpoint %+v %v", checkpoint, err)
	}
	checkpoint.Attempts[0].Failed = false
	independent, _ := store.ExecutionTrace(ctx, "request")
	if !independent.Attempts[0].Failed {
		t.Fatal("EventHub shared trace owner memory")
	}
	complete := usage.CompleteRecord{EventID: "request", CompletedAt: at.Add(time.Second), HTTPStatus: 200, Outcome: "success", OutputTokens: 7, Trace: &tr}
	if err := store.Complete(ctx, complete); err != nil {
		t.Fatal(err)
	}

	if err := store.WriteTrace(ctx, "request", trace.Trace{}); !errors.Is(err, usage.ErrEventNotStarted) {
		t.Fatalf("lost trace write sentinel %v", err)
	}
	dashboard, err := store.Anomalies(ctx, usage.UsageFilter{AllTime: true}, "cause")
	if err != nil || dashboard.Summary.Recovered != 1 || dashboard.Summary.Completed != 1 || dashboard.Summary.FailedAttempts != 1 {
		t.Fatalf("typed aggregation %+v %v", dashboard, err)
	}
	complete.OutputTokens = 99
	if err := store.Complete(ctx, complete); !errors.Is(err, usage.ErrEventNotStarted) {
		t.Fatalf("EventHub lost settlement sentinel: %v", err)
	}
	page, err := store.Events(ctx, usage.EventFilter{UsageFilter: usage.UsageFilter{AllTime: true}})
	if err != nil || len(page.Events) != 1 || page.Events[0].OutputTokens != 7 {
		t.Fatalf("duplicate changed usage: %+v err=%v", page, err)
	}
	if err := store.Start(ctx, usage.StartRecord{EventID: "invalid", APIKeyID: "key", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, usage.CompleteRecord{EventID: "invalid"}); err == nil || errors.Is(err, usage.ErrEventNotStarted) {
		t.Fatalf("invalid settlement was hidden: %v", err)
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
