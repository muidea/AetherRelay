package biz

import (
	"context"
	"errors"
	"testing"
	"time"

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
