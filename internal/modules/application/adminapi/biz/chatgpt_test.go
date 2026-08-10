package biz

import (
	"context"
	"testing"
	"time"

	admincommon "aetherrelay/internal/modules/application/adminapi/pkg/common"
	accountcommon "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/common"
	accountevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	basebiz "aetherrelay/internal/modules/base/biz"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestChatGPTRefreshProgressBypassesBlockedAccountLane(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() {
		background.Shutdown(nil)
		hub.Terminate(context.Background())
	})
	started := make(chan struct{})
	release := make(chan struct{})
	accounts := event.NewSimpleObserver(accountcommon.UnitID, hub)
	accounts.Subscribe("test.block-account-lane", func(_ event.Event, result event.Result) {
		close(started)
		<-release
		result.Set(struct{}{}, nil)
	})
	accounts.Subscribe(accountevents.TopicRefreshProgress, func(_ event.Event, result event.Result) {
		result.Set(accountevents.RefreshProgressResult{Progress: accountevents.RefreshProgress{ProgressID: "progress", Total: 1}}, nil)
	})
	go func() {
		_, _ = hub.Send(event.NewEvent("test.block-account-lane", "test", accountcommon.UnitID, nil, nil)).Get()
	}()
	<-started
	defer close(release)

	admin := &Admin{Base: basebiz.New(admincommon.UnitID, hub, background)}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	progress, err := admin.ChatGPTAccountRefreshProgress(ctx, "progress")
	if err != nil || progress.ProgressID != "progress" {
		t.Fatalf("progress=%+v err=%v", progress, err)
	}
}
