package biz

import (
	"context"
	"fmt"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/internal/oauth"
	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func TestRefreshDoesNotOverwriteReauthenticatedCredentials(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "rotated_tokens", true: "invalid_grant"}[failure], func(t *testing.T) {
			s, hub, ids := newSchedulingAccount(t)
			entered, proceed := make(chan struct{}), make(chan struct{})
			s.oauthRefresh = func(ctx context.Context, request oauth.Request) (oauth.Result, error) {
				close(entered)
				select {
				case <-proceed:
				case <-ctx.Done():
					return oauth.Result{}, ctx.Err()
				}
				if failure {
					return oauth.Result{}, &oauth.Error{Permanent: true, Class: "invalid_grant", Cause: fmt.Errorf("retired credential")}
				}
				return oauth.Result{AccessToken: "stale-access", RefreshToken: "stale-refresh"}, nil
			}
			done := make(chan error, 1)
			go func() {
				_, err := hub.Send(events.BindCommandLane(event.NewEventWithContext(events.TopicRefreshToken, "test", s.ID(), nil, context.Background(), events.RefreshTokenCommand{AccountID: ids[0]}))).Get()
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("refresh did not start")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, importErr := hub.Send(event.NewEventWithContext(events.TopicImport, "test", s.ID(), nil, ctx, events.ImportCommand{Accounts: []events.CredentialInput{{TargetID: ids[0], AccessToken: "new-access", RefreshToken: "new-refresh", Reauthenticate: true}}})).Get()
			close(proceed)
			if importErr != nil {
				t.Fatal("reauthentication blocked by refresh", importErr)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("stale refresh result accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("refresh did not finish")
			}
			credential, ok := s.store.RefreshCredential(ids[0])
			if !ok || credential.AccessToken != "new-access" || credential.RefreshToken != "new-refresh" {
				t.Fatal("stale refresh replaced new credentials")
			}
			view, _ := s.store.View(ids[0])
			if view.PermanentAuthFailure || view.Status != events.StatusNormal || view.LastTokenRefreshErrorClass != "" {
				t.Fatal("stale refresh changed reauthenticated account health", view)
			}
		})
	}
}
