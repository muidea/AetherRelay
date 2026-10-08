package codexupstream

import (
	"context"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	plugincommon "github.com/muidea/magicCommon/framework/plugin/common"
	"github.com/muidea/magicCommon/task"
)

// Exercise the framework registration and lifecycle boundary, rather than
// constructing the biz directly, so rejected plugins cannot pass unnoticed.
func TestFrameworkRegistersAndRunsCodexUpstream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(2)
	mgr := plugincommon.NewPluginMgr("codex-upstream-test")
	block := New()
	if err := mgr.Register(block); err != nil {
		t.Fatalf("register Codex upstream: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if err := mgr.TeardownChecked(cleanupCtx); err != nil {
			t.Errorf("teardown: %v", err)
		}
		if !background.Shutdown(cleanupCtx) {
			t.Error("background tasks did not drain")
		}
		hub.Terminate(cleanupCtx)
	})
	if err := mgr.Setup(ctx, hub, background); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := mgr.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	sendCancel := func() (any, *cd.Error) {
		return hub.Send(events.BindCommandLane(event.NewEventWithContext(
			events.TopicCancel, "test", common.UnitID, nil, ctx,
			events.CancelCommand{StreamID: "nonexistent"},
		))).Get()
	}
	value, err := sendCancel()
	if err != nil {
		t.Fatalf("registered upstream must handle commands: %v", err)
	}
	result, ok := value.(events.CancelResult)
	if !ok || result.Cancelled {
		t.Fatalf("unexpected cancel result: %#v", value)
	}
	if err := mgr.BeginShutdown(ctx); err != nil {
		t.Fatalf("begin shutdown: %v", err)
	}
	if _, err := sendCancel(); err == nil || err.Code != cd.ResourceExhausted {
		t.Fatalf("shutdown must close admission: %v", err)
	}
	if err := mgr.Quiesce(ctx); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	if err := mgr.TeardownChecked(ctx); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if block.bizPtr != nil {
		t.Fatal("teardown retained the drained biz")
	}
}
