package biz

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"ai-proxy/internal/modules/application/chatgptaccountpool/internal/store"
	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestManualRefreshUsesChatGPTWebUpstreamOwner(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := store.New(filepath.Join(t.TempDir(), "accounts.json"), 1)
	if _, _, err := accounts.Add([]string{"account-token"}, "web"); err != nil {
		t.Fatal(err)
	}
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicGetUserInfo, func(ev event.Event, result event.Result) {
		command, ok := ev.Data().(upevents.GetUserInfoCommand)
		if !ok || command.AccessToken != "account-token" {
			t.Fatalf("unexpected upstream command: %#v", ev.Data())
		}
		result.Set(upevents.GetUserInfoResult{PlanType: "plus", Quota: 3}, nil)
	})
	account := newAccount(hub, background, accounts, 0)
	defer account.Teardown(context.Background())

	value, err := hub.Send(event.NewEvent(accevents.TopicRefresh, "test", acccommon.UnitID, nil, accevents.RefreshCommand{AccessTokens: []string{"account-token"}})).Get()
	if err != nil {
		t.Fatal(err)
	}
	started, ok := value.(accevents.RefreshResult)
	if !ok || started.ProgressID == "" {
		t.Fatalf("start result=%#v", value)
	}

	var progress accevents.RefreshProgress
	deadline := time.Now().Add(time.Second)
	for !progress.Done && time.Now().Before(deadline) {
		value, err = hub.Send(event.NewEvent(accevents.TopicRefreshProgress, "test", acccommon.UnitID, nil, accevents.RefreshProgressCommand{ProgressID: started.ProgressID})).Get()
		if err != nil {
			t.Fatal(err)
		}
		progress = value.(accevents.RefreshProgressResult).Progress
		if !progress.Done {
			time.Sleep(time.Millisecond)
		}
	}
	if !progress.Done || progress.Total != 1 || progress.Processed != 1 || progress.Refreshed != 1 || progress.TotalQuota != 3 || progress.StatusCounts.Normal != 1 {
		t.Fatalf("progress=%#v", progress)
	}
	for _, refreshErr := range progress.Errors {
		if refreshErr.AccountID == "account-token" {
			t.Fatalf("progress leaked access token: %#v", progress)
		}
	}
}

func TestManualRefreshInFlightDuringTeardownExitsSafely(t *testing.T) {
	hub := event.NewHub(8)
	defer hub.Terminate(context.Background())
	background := task.NewBackgroundRoutine(8)

	accounts := store.New(filepath.Join(t.TempDir(), "accounts.json"), 1)
	if _, _, err := accounts.Add([]string{"account-token"}, "web"); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicGetUserInfo, func(event.Event, event.Result) {
		close(started)
		<-release
	})
	account := newAccount(hub, background, accounts, 0)
	account.putProgress(accevents.RefreshProgress{ProgressID: "refresh", Errors: []accevents.RefreshError{}})
	if err := background.AsyncFunction(func() {
		account.runManualRefresh("refresh", []string{"account-token"})
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not reach upstream request")
	}
	account.Teardown(context.Background())
	close(release)
	if !background.Shutdown(context.Background()) {
		t.Fatal("background routine did not stop")
	}

	if account.store == nil {
		t.Fatal("teardown released store while background task can still reference it")
	}
	progress, found := account.getProgress("refresh")
	if !found || !progress.Done || progress.Error != "account pool is shutting down" {
		t.Fatalf("progress=%#v, found=%v", progress, found)
	}
}
