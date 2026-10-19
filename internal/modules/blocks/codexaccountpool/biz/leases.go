package biz

import (
	"encoding/json"
	"log/slog"
	"sort"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

type accountLease struct {
	AccountID string
	events.LeaseView
}

func (s *Account) logSlotsExhaustedLocked(request events.LeaseRequest, model string, now time.Time) {
	data, err := json.Marshal(s.leaseSnapshotLocked(now))
	if err != nil {
		slog.Error("Codex account slot snapshot encode failed", "request_id", request.RequestID, "error", err)
		return
	}
	slog.Warn("Codex account slots exhausted", "request_id", request.RequestID, "event_id", request.EventID, "api_key_id", request.APIKeyID, "model", model, "slot_snapshot", string(data))
}

func (s *Account) addLeaseLocked(id, accountID, model, transport, kind string, request events.LeaseRequest, now time.Time) {
	if kind != events.LeaseWebsocket && kind != events.LeaseProbe {
		kind = events.LeaseHTTP
	}
	s.leases[id] = accountLease{AccountID: accountID, LeaseView: events.LeaseView{
		LeaseRequest: request, LeaseID: id, Model: model, Transport: transport,
		Kind: kind, State: events.LeaseActive, AcquiredAt: now, StateChangedAt: now,
	}}
}

// All scheduler facts are copied under the same lock used by admission/release.
func (s *Account) leaseSnapshotLocked(now time.Time) events.LeaseSnapshotResult {
	out := events.LeaseSnapshotResult{At: now, Accounts: []events.AccountSlots{}}
	accounts := make(map[string]events.AccountSlots)
	for _, a := range s.store.List() {
		accounts[a.ID] = events.AccountSlots{AccountID: a.ID, AccountLabel: a.Email, MaxConcurrency: a.MaxConcurrency, Leases: []events.LeaseView{}, ReleaseFailures: s.releaseFailures[a.ID]}
	}
	for id := range s.releaseFailures {
		if _, configured := accounts[id]; !configured && s.inflight[id] == 0 {
			delete(s.releaseFailures, id)
		}
	}
	for _, l := range s.leases {
		a := accounts[l.AccountID]
		a.AccountID = l.AccountID // A deleted account may still have draining leases.
		a.ReleaseFailures = s.releaseFailures[l.AccountID]
		v := l.LeaseView
		v.HeldMS = max(int64(0), now.Sub(v.AcquiredAt).Milliseconds())
		if v.Kind == events.LeaseWebsocket {
			if v.State == events.LeaseIdle {
				a.WebsocketIdle++
				v.IdleMS = max(int64(0), now.Sub(v.StateChangedAt).Milliseconds())
			} else {
				a.WebsocketActive++
			}
		}
		a.OldestLeaseMS = max(a.OldestLeaseMS, v.HeldMS)
		a.Leases = append(a.Leases, v)
		accounts[l.AccountID] = a
	}
	for id, a := range accounts {
		a.Inflight = s.inflight[id]
		sort.Slice(a.Leases, func(i, j int) bool { return a.Leases[i].LeaseID < a.Leases[j].LeaseID })
		out.Accounts = append(out.Accounts, a)
	}
	sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i].AccountID < out.Accounts[j].AccountID })
	return out
}

func (s *Account) handleLeaseSnapshot(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if _, ok := ev.Data().(events.LeaseSnapshotCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex lease snapshot command"))
		return
	}
	s.scheduleMu.Lock()
	out := s.leaseSnapshotLocked(s.now().UTC())
	s.scheduleMu.Unlock()
	result.Set(out, nil)
}

func (s *Account) handleLeaseState(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.LeaseStateCommand)
	if !ok || cmd.LeaseID == "" || (cmd.State != events.LeaseIdle && cmd.State != events.LeaseActive) {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex lease state command"))
		return
	}
	s.scheduleMu.Lock()
	l, found := s.leases[cmd.LeaseID]
	if found && l.Kind == events.LeaseWebsocket {
		if l.State != cmd.State {
			l.StateChangedAt = s.now().UTC()
		}
		l.State = cmd.State
		if cmd.State == events.LeaseActive {
			l.LeaseRequest = cmd.Request
		}
		s.leases[cmd.LeaseID] = l
	} else {
		found = false
	}
	s.scheduleMu.Unlock()
	result.Set(events.LeaseStateResult{Updated: found}, nil)
}

func (s *Account) handleReleaseFailure(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.ReleaseFailureCommand)
	if !ok || cmd.LeaseID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex release failure command"))
		return
	}
	s.scheduleMu.Lock()
	accountID := cmd.AccountID
	known := false
	if l, found := s.leases[cmd.LeaseID]; found {
		accountID = l.AccountID
		known = true
	}
	// Only configured accounts or existing leases can create counter labels.
	for _, a := range s.store.List() {
		if a.ID == accountID {
			known = true
			break
		}
	}
	if known {
		if s.releaseFailures == nil {
			s.releaseFailures = map[string]uint64{}
		}
		s.releaseFailures[accountID]++
	}
	s.scheduleMu.Unlock()
	result.Set(events.ReleaseFailureResult{}, nil)
}
