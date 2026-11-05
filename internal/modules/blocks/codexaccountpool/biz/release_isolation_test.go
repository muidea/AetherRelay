package biz

import (
	"context"
	"sync"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func TestLeaseReleaseDoesNotWaitForRefreshOrAdmission(t *testing.T) {
	for _, isolatedRefresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "busy_management_lane", true: "refresh_lane"}[isolatedRefresh], func(t *testing.T) {
			s, hub, ids := newSchedulingAccount(t)
			lease := acquireForTest(t, s, events.AcquireCommand{Model: "gpt-test", PreferredID: ids[0]})
			flight, _ := s.beginRefresh(ids[0])
			var once sync.Once
			unblock := func() {
				once.Do(func() { s.finishRefresh(ids[0], flight, events.RefreshTokenResult{Refreshed: true}, nil) })
			}
			defer unblock()
			entered, refreshed := make(chan struct{}), make(chan struct{})
			s.UnsubscribeFunc(events.TopicRefreshToken)
			s.SubscribeFunc(events.TopicRefreshToken, func(ev event.Event, result event.Result) {
				close(entered)
				s.handleRefreshToken(ev, result)
			})
			go func() {
				defer close(refreshed)
				ev := event.NewEventWithContext(events.TopicRefreshToken, "test", s.ID(), nil, context.Background(), events.RefreshTokenCommand{AccountID: ids[0]})
				if isolatedRefresh {
					ev = events.BindCommandLane(ev)
				}
				hub.Send(ev)
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("refresh did not start")
			}
			// Admission may be waiting on persistence while holding scheduleMu.
			s.scheduleMu.Lock()
			released := make(chan event.Result, 1)
			go func() {
				released <- hub.Send(events.BindCommandLane(event.NewEvent(events.TopicRelease, "test", s.ID(), nil, events.ReleaseCommand{LeaseID: lease.LeaseID})))
			}()
			select {
			case result := <-released:
				s.scheduleMu.Unlock()
				value, err := result.Get()
				if err != nil || !value.(events.ReleaseResult).Released {
					t.Fatal(value, err)
				}
			case <-time.After(time.Second):
				s.scheduleMu.Unlock()
				t.Fatal("slow refresh/admission blocked lease release")
			}
			if isolatedRefresh {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if _, err := hub.Send(event.NewEventWithContext(events.TopicList, "test", s.ID(), nil, ctx, events.ListCommand{})).Get(); err != nil {
					t.Fatal("refresh blocked account management", err)
				}
			}
			unblock()
			<-refreshed
			if a := slotsForTest(t, s, ids[0]); a.Inflight != 0 || len(a.Leases) != 0 {
				t.Fatal("released lease remained occupied", a)
			}
		})
	}
}
