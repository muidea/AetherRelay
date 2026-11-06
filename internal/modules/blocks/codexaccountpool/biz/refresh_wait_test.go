package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/internal/oauth"
	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestRefreshWaiterDeadlineDoesNotDiscardSharedRefresh(t *testing.T) {
	flight := &refreshFlight{done: make(chan struct{})}
	account := &Account{refreshes: map[string]*refreshFlight{"account": flight}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := account.refreshToken(ctx, "account")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter err=%v", err)
	}
	if account.refreshes["account"] != flight {
		t.Fatal("waiter removed shared refresh")
	}
	done := make(chan events.RefreshTokenResult, 1)
	joined := make(chan struct{})
	go func() {
		shared, created := account.beginRefresh("account")
		if created {
			t.Error("created duplicate refresh")
		}
		close(joined)
		<-shared.done
		done <- shared.result
	}()
	<-joined
	account.finishRefresh("account", flight, events.RefreshTokenResult{Refreshed: true, AccountID: "account"}, nil)
	select {
	case result := <-done:
		if !result.Refreshed {
			t.Fatal("other waiter lost refresh result")
		}
	case <-time.After(time.Second):
		t.Fatal("other waiter did not receive refresh result")
	}
}

func TestRefreshOwnerBudgetIncludesBackgroundWorkerWait(t *testing.T) {
	s, hub, ids := newSchedulingAccount(t)
	background := task.NewBackgroundRoutine(1)
	owner := newAccount(hub, background, nil, 0, 20*time.Millisecond)
	owner.store = s.store
	defer background.Shutdown(context.Background())
	defer owner.shutdown()
	refreshed := make(chan struct{})
	owner.oauthRefresh = func(context.Context, oauth.Request) (oauth.Result, error) {
		defer close(refreshed)
		return oauth.Result{AccessToken: "rotated-access", RefreshToken: "rotated-refresh"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	// A capacity/discovery job can already occupy the only managed worker when
	// it asks the account owner for refresh on that same background pool.
	if err := background.AsyncFunction(func() {
		_, err := owner.refreshToken(ctx, ids[0])
		done <- err
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiter did not honor owner budget: %v", err)
		}
	case <-time.After(time.Second):
		cancel() // Break the cycle before reporting a regression.
		<-done
		t.Fatal("background job and nested refresh waited on each other")
	}
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("accepted credential rotation was discarded after waiter timeout")
	}
	background.Shutdown(context.Background())
	credential, ok := owner.store.RefreshCredential(ids[0])
	if !ok || credential.AccessToken != "rotated-access" || credential.RefreshToken != "rotated-refresh" {
		t.Fatal("rotated credential was not persisted")
	}
}

func TestRefreshSubmissionFailureReleasesSharedFlight(t *testing.T) {
	hub := event.NewHub(4)
	defer hub.Terminate(context.Background())
	background := task.NewBackgroundRoutine(1)
	background.Shutdown(context.Background())
	account := newAccount(hub, background, nil, 0, time.Second)
	defer account.shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := account.refreshToken(ctx, "account")
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("submission failure not returned: %v", err)
	}
	if len(account.refreshes) != 0 {
		t.Fatal("failed submission left a stuck shared refresh")
	}
}
