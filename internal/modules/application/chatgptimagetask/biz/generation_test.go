package biz

import (
	"context"
	"path/filepath"
	"testing"

	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	"ai-proxy/internal/modules/application/chatgptimagetask/internal/store"
	imgcommon "ai-proxy/internal/modules/application/chatgptimagetask/pkg/common"
	imgevents "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
	basebiz "ai-proxy/internal/modules/base/biz"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestRunGenerationRecordsSuccessfulUpstreamResult(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(context.Background())

	tasks := store.New(filepath.Join(t.TempDir(), "state.duckdb"))
	defer tasks.Close()
	if _, created, err := tasks.GetOrCreateGeneration("owner", "task", "prompt", "gpt-image-2", "", ""); err != nil || !created {
		t.Fatalf("create task: created=%v err=%v", created, err)
	}

	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquireImageToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireImageTokenResult{AccessToken: "token", Account: accevents.AccountView{ID: "account-1", Proxy: "http://task-proxy.invalid:8080"}}, nil)
	})
	accounts.Subscribe(accevents.TopicReleaseImageSlot, func(_ event.Event, result event.Result) {
		result.Set(accevents.ReleaseImageSlotResult{OK: true}, nil)
	})
	accounts.Subscribe(accevents.TopicMarkImageResult, func(_ event.Event, result event.Result) {
		result.Set(accevents.MarkImageResultResult{}, nil)
	})

	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicGenerateImage, func(ev event.Event, result event.Result) {
		if command := ev.Data().(upevents.GenerateImageCommand); command.Proxy != "http://task-proxy.invalid:8080" {
			t.Fatalf("task image proxy=%q", command.Proxy)
		}
		// This successful EventHub result has a nil *cd.Error. The retry helper
		// must preserve that typed nil rather than converting it to a non-nil
		// error interface and recording "<nil>" as a task failure.
		result.Set(upevents.GenerateImageResult{
			ConversationID: "conversation-1",
			Images:         []upevents.ImageOutput{{URL: "https://example.invalid/image.png"}},
		}, nil)
	})

	imageTask := &ImageTask{Base: basebiz.New(imgcommon.UnitID, hub, background), store: tasks}
	imageTask.runGeneration("owner", "task", "prompt", "gpt-image-2", "", "", "")

	view, found := tasks.Get("owner", "task")
	if !found || view.Status != imgevents.StatusSuccess || view.Error != "" || view.ConversationID != "conversation-1" {
		t.Fatalf("task=%+v found=%v", view, found)
	}
}

func TestResumableConversationFailure(t *testing.T) {
	if !isResumableConversationFailure(imgevents.TaskView{Status: imgevents.StatusError, ConversationID: "conversation-1", Error: "<nil>"}) {
		t.Fatal("legacy typed-nil task should be recoverable by polling its existing conversation")
	}
	if isResumableConversationFailure(imgevents.TaskView{Status: imgevents.StatusError}) {
		t.Fatal("task without an upstream conversation must not be resumed")
	}
}
