package biz

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-proxy/internal/modules/application/chatgptaccountpool/internal/oauth"
	"ai-proxy/internal/modules/application/chatgptaccountpool/internal/store"
	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type refreshTestOAuthClient struct {
	calls atomic.Int32
	proxy string
}

func (c *refreshTestOAuthClient) Refresh(_ context.Context, request oauth.Request) (oauth.Result, error) {
	c.calls.Add(1)
	if request.RefreshToken != "refresh-old" {
		return oauth.Result{}, errors.New("unexpected refresh token")
	}
	if request.Proxy != c.proxy {
		return oauth.Result{}, errors.New("unexpected account proxy")
	}
	return oauth.Result{AccessToken: "new-token", RefreshToken: "refresh-new", IDToken: "id-new"}, nil
}

type failedRefreshOAuthClient struct{ err error }

func (c failedRefreshOAuthClient) Refresh(context.Context, oauth.Request) (oauth.Result, error) {
	return oauth.Result{}, c.err
}

func (failedRefreshOAuthClient) ExchangeAuthorizationCode(context.Context, oauth.AuthorizationCodeRequest) (oauth.Result, error) {
	return oauth.Result{}, errors.New("not implemented")
}

func (*refreshTestOAuthClient) ExchangeAuthorizationCode(context.Context, oauth.AuthorizationCodeRequest) (oauth.Result, error) {
	return oauth.Result{}, errors.New("not implemented")
}

func TestManualRefreshUsesChatGPTWebUpstreamOwner(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := store.New(filepath.Join(t.TempDir(), "accounts.json"), 1)
	if _, _, err := accounts.Add([]string{"account-token"}, "web"); err != nil {
		t.Fatal(err)
	}
	proxyURL := "http://account-proxy.invalid:8080"
	if _, _, err := accounts.UpdateByID(accounts.List()[0].ID, nil, nil, nil, &proxyURL); err != nil {
		t.Fatal(err)
	}
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicGetUserInfo, func(ev event.Event, result event.Result) {
		command, ok := ev.Data().(upevents.GetUserInfoCommand)
		if !ok || command.AccessToken != "account-token" || command.Proxy != proxyURL {
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

func TestRequestTextTokenRefreshCoalescesAndRotates(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := store.New(filepath.Join(t.TempDir(), "accounts.json"), 1)
	if _, _, err := accounts.AddOAuth("old-token", "refresh-old", "id-old"); err != nil {
		t.Fatal(err)
	}
	account := newAccount(hub, background, accounts, 0)
	defer account.Teardown(context.Background())
	fakeOAuth := &refreshTestOAuthClient{}
	account.oauth = fakeOAuth

	var wg sync.WaitGroup
	results := make(chan accevents.RefreshTextTokenResult, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := hub.Send(event.NewEvent(accevents.TopicRefreshTextToken, "test", acccommon.UnitID, nil, accevents.RefreshTextTokenCommand{AccessToken: "old-token"})).Get()
			if err != nil {
				errs <- err
				return
			}
			refreshed, ok := value.(accevents.RefreshTextTokenResult)
			if !ok {
				errs <- errors.New("invalid refresh text token result")
				return
			}
			results <- refreshed
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if fakeOAuth.calls.Load() != 1 {
		t.Fatalf("oauth refresh calls=%d, want 1", fakeOAuth.calls.Load())
	}
	for refreshed := range results {
		if refreshed.AccessToken != "new-token" || !refreshed.Refreshed || refreshed.Account.AccessToken != "new-token" {
			t.Fatalf("refresh result=%+v", refreshed)
		}
	}
	if view, ok := accounts.ViewForAccessToken("old-token"); !ok || view.AccessToken != "new-token" {
		t.Fatalf("rotated account=%+v ok=%v", view, ok)
	}
}

func TestRequestTextTokenRefreshUsesAccountProxy(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := store.New(filepath.Join(t.TempDir(), "accounts.json"), 1)
	if _, _, err := accounts.AddOAuth("old-token", "refresh-old", "id-old"); err != nil {
		t.Fatal(err)
	}
	proxyURL := "http://account-proxy.invalid:8080"
	if _, _, err := accounts.UpdateByID(accounts.List()[0].ID, nil, nil, nil, &proxyURL); err != nil {
		t.Fatal(err)
	}
	account := newAccount(hub, background, accounts, 0)
	defer account.Teardown(context.Background())
	account.oauth = &refreshTestOAuthClient{proxy: proxyURL}

	value, err := hub.Send(event.NewEvent(accevents.TopicRefreshTextToken, "test", acccommon.UnitID, nil, accevents.RefreshTextTokenCommand{AccessToken: "old-token"})).Get()
	if err != nil {
		t.Fatal(err)
	}
	refreshed, ok := value.(accevents.RefreshTextTokenResult)
	if !ok || !refreshed.Refreshed || refreshed.AccessToken != "new-token" {
		t.Fatalf("refresh result=%+v", value)
	}
}

func TestTransientOAuthRefreshFailureRetainsAccount(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := store.New(filepath.Join(t.TempDir(), "accounts.json"), 1)
	if _, _, err := accounts.AddOAuth("old-token", "refresh-old", "id-old"); err != nil {
		t.Fatal(err)
	}
	account := newAccount(hub, background, accounts, 0)
	defer account.Teardown(context.Background())
	account.oauth = failedRefreshOAuthClient{err: &oauth.Error{Class: "transport", Retryable: true}}

	value, err := hub.Send(event.NewEvent(accevents.TopicRefreshTextToken, "test", acccommon.UnitID, nil, accevents.RefreshTextTokenCommand{AccessToken: "old-token"})).Get()
	if err != nil {
		t.Fatal(err)
	}
	refreshed, ok := value.(accevents.RefreshTextTokenResult)
	if !ok || refreshed.Refreshed || refreshed.PermanentFailure || refreshed.ErrorClass != "transport" {
		t.Fatalf("refresh result=%+v", value)
	}
	if view, ok := accounts.ViewForAccessToken("old-token"); !ok || view.Status != store.StatusNormal {
		t.Fatalf("account should remain usable: view=%+v found=%v", view, ok)
	}
}
