package biz

import (
	"context"
	"path/filepath"
	"testing"

	"ai-proxy/internal/modules/application/chatgptimagetask/internal/store"
	imgcommon "ai-proxy/internal/modules/application/chatgptimagetask/pkg/common"
	imgevents "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	basebiz "ai-proxy/internal/modules/base/biz"
	"ai-proxy/internal/pkg/chatgpttokenusage"
	cd "github.com/muidea/magicCommon/def"
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

	proxy := event.NewSimpleObserver(proxycommon.UnitID, hub)
	proxy.Subscribe(proxyevents.TopicExecuteFeatureImage, func(ev event.Event, result event.Result) {
		if command := ev.Data().(proxyevents.ExecuteFeatureImageCommand); command.Model != "gpt-image-2" {
			t.Fatalf("task image model=%q", command.Model)
		}
		result.Set(proxyevents.ExecuteFeatureImageResult{
			Provider:       "chatgptweb",
			ConversationID: "conversation-1",
			AccountID:      "account-1",
			Usage:          &tokenusage.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8},
			Data:           []proxyevents.FeatureImageData{{URL: "https://example.invalid/image.png"}},
		}, nil)
	})

	imageTask := &ImageTask{Base: basebiz.New(imgcommon.UnitID, hub, background), store: tasks}
	imageTask.runGeneration("owner", "task", "prompt", "gpt-image-2", "", "", "")

	view, found := tasks.Get("owner", "task")
	resume, resumable := tasks.GetResumeInfo("owner", "task")
	if !found || view.Status != imgevents.StatusSuccess || view.Error != "" || view.Provider != "chatgptweb" || view.ConversationID != "conversation-1" || view.Usage == nil || view.Usage.TotalTokens != 8 || !resumable || resume.AccountID != "account-1" {
		t.Fatalf("task=%+v found=%v", view, found)
	}
}

func TestRunGenerationRecordsRecoveryMetadataOnFailure(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(context.Background())

	tasks := store.New(filepath.Join(t.TempDir(), "state.duckdb"))
	defer tasks.Close()
	if _, created, err := tasks.GetOrCreateGeneration("owner", "task", "prompt", "gpt-image-2", "", ""); err != nil || !created {
		t.Fatalf("create task: created=%v err=%v", created, err)
	}

	proxy := event.NewSimpleObserver(proxycommon.UnitID, hub)
	proxy.Subscribe(proxyevents.TopicExecuteFeatureImage, func(_ event.Event, result event.Result) {
		result.Set(proxyevents.ExecuteFeatureImageResult{Provider: "chatgptweb", ConversationID: "conversation-1", AccountID: "account-1"}, cd.NewError(cd.Unexpected, "upstream timeout"))
	})

	imageTask := &ImageTask{Base: basebiz.New(imgcommon.UnitID, hub, background), store: tasks}
	imageTask.runGeneration("owner", "task", "prompt", "gpt-image-2", "", "", "")

	resume, found := tasks.GetResumeInfo("owner", "task")
	if !found || resume.Task.Status != imgevents.StatusError || resume.Task.Provider != "chatgptweb" || resume.Task.ConversationID != "conversation-1" || resume.AccountID != "account-1" {
		t.Fatalf("resume=%+v found=%v", resume, found)
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
