package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	proxybiz "aetherrelay/internal/modules/application/proxyapi/biz"
	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	basebiz "aetherrelay/internal/modules/base/biz"
	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func slotsForTest(t *testing.T, s *Account, id string) events.AccountSlots {
	t.Helper()
	r := s.SendEvent(event.NewEventWithContext(events.TopicLeaseSnapshot, "test", s.ID(), nil, context.Background(), events.LeaseSnapshotCommand{}))
	v, err := r.Get()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range v.(events.LeaseSnapshotResult).Accounts {
		if a.AccountID == id {
			return a
		}
	}
	t.Fatalf("account %s missing", id)
	return events.AccountSlots{}
}

func TestSixSlotsBusySnapshotAndIdempotentRelease(t *testing.T) {
	s, _, ids := newSchedulingAccount(t)
	limit, disabled := 6, events.StatusDisabled
	if _, err := s.store.Update(ids[0], nil, nil, nil, &limit); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Update(ids[1], &disabled, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	for i := 0; i < 6; i++ {
		acquireForTest(t, s, events.AcquireCommand{Model: "gpt-test", Request: events.LeaseRequest{RequestID: "request-original", EventID: "event-original", APIKeyID: "claude-office", Protocol: "anthropic", Endpoint: "/v1/messages"}})
	}
	now = now.Add(90 * time.Second)
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, nil)))
	defer slog.SetDefault(previous)
	r := event.NewResult(events.TopicAcquire, "test", s.ID())
	s.handleAcquire(event.NewEvent(events.TopicAcquire, "test", s.ID(), nil, events.AcquireCommand{Model: "gpt-test", Request: events.LeaseRequest{RequestID: "request-denied"}}), r)
	v, err := r.Get()
	if err == nil || v.(events.AcquireResult).UnavailableReason != "accounts_busy" {
		t.Fatal(v, err)
	}
	var record struct {
		Message   string `json:"msg"`
		RequestID string `json:"request_id"`
		Snapshot  string `json:"slot_snapshot"`
	}
	if err := json.Unmarshal(log.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	var snapshot events.LeaseSnapshotResult
	if err := json.Unmarshal([]byte(record.Snapshot), &snapshot); err != nil {
		t.Fatal(err)
	}
	if record.RequestID != "request-denied" || record.Message != "Codex account slots exhausted" {
		t.Fatal(record)
	}
	a := slotsForTest(t, s, ids[0])
	if a.Inflight != 6 || a.MaxConcurrency != 6 || len(a.Leases) != 6 || a.OldestLeaseMS != 90000 {
		t.Fatal(a)
	}
	if a.Leases[0].RequestID != "request-original" || a.Leases[0].APIKeyID != "claude-office" || a.Leases[0].EventID != "event-original" {
		t.Fatal(a.Leases)
	}
	logged := false
	for _, a := range snapshot.Accounts {
		if a.AccountID == ids[0] && len(a.Leases) == 6 && a.Inflight == 6 {
			logged = true
		}
	}
	if !logged {
		t.Fatal("busy log lost occupying leases", snapshot)
	}
	lease := a.Leases[0].LeaseID
	if !releaseForTest(t, s, lease).Released || releaseForTest(t, s, lease).Released {
		t.Fatal("release not idempotent")
	}
	if slotsForTest(t, s, ids[0]).Inflight != 5 {
		t.Fatal("duplicate release changed occupancy")
	}
	acquireForTest(t, s, events.AcquireCommand{Model: "gpt-test"})
	if slotsForTest(t, s, ids[0]).Inflight != 6 {
		t.Fatal("slot did not recover")
	}
}

func TestNativeWebsocketTracksIdleWithoutReleasingSlot(t *testing.T) {
	s, hub, ids := newSchedulingAccount(t)
	disabled := events.StatusDisabled
	if _, err := s.store.Update(ids[1], &disabled, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	up := event.NewSimpleObserver(upcommon.UnitID, hub)
	up.Subscribe(upevents.TopicWSOpen, func(_ event.Event, r event.Result) {
		r.Set(upevents.WSOpenResult{SessionID: "session", HTTPStatus: 101}, nil)
	})
	up.Subscribe(upevents.TopicWSSend, func(_ event.Event, r event.Result) { r.Set(upevents.WSSendResult{Sent: true}, nil) })
	up.Subscribe(upevents.TopicWSPull, func(_ event.Event, r event.Result) {
		r.Set(upevents.WSPullResult{Payload: []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`)}, nil)
	})
	up.Subscribe(upevents.TopicWSClose, func(_ event.Event, r event.Result) { r.Set(upevents.WSCloseResult{Closed: true}, nil) })
	p := &proxybiz.Proxy{Base: basebiz.New("test-proxy", hub, s.BackgroundRoutine())}
	ctx := codexresponses.WithExecutionIdentity(context.Background(), codexresponses.ExecutionIdentity{RequestID: "ws-request", EventID: "turn-1", APIKeyID: "codex-owner", Protocol: "openai", Endpoint: "/v1/responses"})
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	opened, err := p.OpenCodexWebsocket(ctx, codexresponses.WebsocketOpenRequest{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.CloseCodexWebsocket(ctx, opened.SessionID)
	for _, eventID := range []string{"turn-1", "turn-2"} {
		i := codexresponses.ExecutionIdentityFromContext(ctx)
		i.EventID = eventID
		ctx = codexresponses.WithExecutionIdentity(ctx, i)
		if err := p.SendCodexWebsocket(ctx, opened.SessionID, []byte(`{"model":"gpt-test"}`)); err != nil {
			t.Fatal(err)
		}
		a := slotsForTest(t, s, ids[0])
		if a.Inflight != 1 || a.WebsocketActive != 1 || a.Leases[0].EventID != eventID {
			t.Fatal(a)
		}
		if _, err := p.PullCodexWebsocket(ctx, opened.SessionID); err != nil {
			t.Fatal(err)
		}
		a = slotsForTest(t, s, ids[0])
		if a.Inflight != 1 || a.WebsocketIdle != 1 || a.WebsocketActive != 0 {
			t.Fatal(a)
		}
		now = now.Add(2 * time.Second)
		a = slotsForTest(t, s, ids[0])
		if a.Leases[0].IdleMS != 2000 {
			t.Fatal("idle duration was not observed", a)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	p.CloseCodexWebsocket(canceled, opened.SessionID)
	p.CloseCodexWebsocket(canceled, opened.SessionID)
	if a := slotsForTest(t, s, ids[0]); a.Inflight != 0 || len(a.Leases) != 0 || a.WebsocketIdle != 0 {
		t.Fatal(a)
	}
}

func TestReleaseFailureCounterAndDeletedAccountDrain(t *testing.T) {
	s, _, ids := newSchedulingAccount(t)
	lease := acquireForTest(t, s, events.AcquireCommand{Model: "gpt-test", PreferredID: ids[0]})
	report := func() {
		t.Helper()
		r := s.SendEvent(event.NewEventWithContext(events.TopicReleaseFailure, "test", s.ID(), nil, context.Background(), events.ReleaseFailureCommand{LeaseID: lease.LeaseID, AccountID: ids[0]}))
		if _, err := r.Get(); err != nil {
			t.Fatal(err)
		}
	}
	report()
	if a := slotsForTest(t, s, ids[0]); a.ReleaseFailures != 1 || a.Inflight != 1 {
		t.Fatal(a)
	}
	releaseForTest(t, s, lease.LeaseID)
	report() // A failed/malformed acknowledgment may follow an already applied release.
	if a := slotsForTest(t, s, ids[0]); a.ReleaseFailures != 2 || a.Inflight != 0 {
		t.Fatal(a)
	}
	lease = acquireForTest(t, s, events.AcquireCommand{Model: "gpt-test", PreferredID: ids[0]})
	if _, err := s.store.Delete([]string{ids[0]}); err != nil {
		t.Fatal(err)
	}
	report()
	if a := slotsForTest(t, s, ids[0]); a.Inflight != 1 || a.ReleaseFailures != 3 {
		t.Fatal("deleted account hid draining slot", a)
	}
	releaseForTest(t, s, lease.LeaseID)
	s.scheduleMu.Lock()
	snapshot := s.leaseSnapshotLocked(s.now().UTC())
	s.scheduleMu.Unlock()
	for _, a := range snapshot.Accounts {
		if a.AccountID == ids[0] {
			t.Fatal("drained account retained", a)
		}
	}
}
