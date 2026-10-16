package biz

import (
	"context"
	"fmt"
	"strings"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/google/uuid"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

func (s *Account) handleProbeAdmission(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.ProbeAdmissionCommand)
	cmd.AccountID = strings.TrimSpace(cmd.AccountID)
	if !ok || cmd.AccountID == "" || strings.TrimSpace(cmd.Model) == "" || (cmd.Acquire && cmd.LeaseID != "") {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid probe admission command"))
		return
	}
	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
	if s.stopping.Load() {
		result.Set(events.ProbeAdmissionResult{Reason: "pool_stopping"}, nil)
		return
	}
	inflight := s.inflight[cmd.AccountID]
	leaseValid := cmd.LeaseID == "" || (s.leases[cmd.LeaseID] == cmd.AccountID && inflight > 0)
	if cmd.LeaseID != "" && leaseValid {
		inflight--
	}
	out := s.store.ProbeAdmission(cmd, inflight)
	if !leaseValid {
		out.Allowed, out.Reason = false, "probe_lease_unavailable"
	}
	if out.Allowed && cmd.Acquire {
		lease := uuid.NewString()
		s.leases[lease] = out.Credential.AccountID
		s.inflight[out.Credential.AccountID]++
		out.Credential.LeaseID = lease
	}
	result.Set(out, nil)
}

// A probe-created refresh executes in its bounded command lane. An existing
// owner refresh is shared, but its independent lifetime is not canceled.
func (s *Account) refreshTokenForProbe(ctx context.Context, cmd events.RefreshTokenCommand) (events.RefreshTokenResult, error) {
	if ctx.Err() != nil || s.stopping.Load() {
		return events.RefreshTokenResult{}, context.Canceled
	}
	s.scheduleMu.Lock()
	leased := s.leases[cmd.ProbeLeaseID] == cmd.AccountID
	s.scheduleMu.Unlock()
	if !leased || cmd.ProbeConfigVersion == "" {
		return events.RefreshTokenResult{}, fmt.Errorf("probe lease unavailable")
	}
	if _, ok := s.store.RefreshCredentialVersion(cmd.AccountID, cmd.ProbeConfigVersion); !ok {
		return events.RefreshTokenResult{}, fmt.Errorf("probe account changed")
	}
	flight, created := s.beginRefresh(cmd.AccountID)
	if created {
		refreshCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(s.shutdownCtx, cancel)
		result, err := s.refreshTokenOnceVersion(refreshCtx, cmd.AccountID, cmd.ProbeConfigVersion)
		stop()
		cancel()
		s.finishRefresh(cmd.AccountID, flight, result, err)
		return result, err
	}
	select {
	case <-flight.done:
		return flight.result, flight.err
	case <-ctx.Done():
		return events.RefreshTokenResult{}, ctx.Err()
	}
}
