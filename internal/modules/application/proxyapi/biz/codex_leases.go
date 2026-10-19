package biz

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/usagetarget"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func codexLeaseRequest(ctx context.Context) accevents.LeaseRequest {
	i := codexresponses.ExecutionIdentityFromContext(ctx)
	return accevents.LeaseRequest{RequestID: i.RequestID, EventID: i.EventID, APIKeyID: i.APIKeyID, Protocol: i.Protocol, Endpoint: i.Endpoint}
}

func (s *Proxy) observeCodexLeaseState(ctx context.Context, leaseID, state string) {
	if leaseID == "" {
		return
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	result := s.SendEvent(event.NewEventWithContext(accevents.TopicLeaseState, s.ID(), acccommon.UnitID, event.NewHeader(), cleanup, accevents.LeaseStateCommand{LeaseID: leaseID, State: state, Request: codexLeaseRequest(ctx)}))
	var err error
	if result == nil {
		err = fmt.Errorf("missing Codex lease state result")
	} else {
		value, getErr := result.Get()
		if getErr != nil {
			err = getErr
		} else {
			if out, ok := value.(accevents.LeaseStateResult); !ok || !out.Updated {
				err = fmt.Errorf("Codex lease state was not acknowledged: %v", value)
			}
		}
	}
	if err != nil {
		slog.Warn("Codex lease state observation failed", "lease_id", leaseID, "state", state, "request_id", codexLeaseRequest(ctx).RequestID, "error", err)
	}
}

// Release is idempotent at the owner. A valid Released=false acknowledgment
// means the lease is already absent; it must not decrement occupancy again.
// Retries share a single cleanup budget independent of client cancellation.
func (s *Proxy) releaseCodexAccount(ctx context.Context, leaseID string) {
	_ = s.releaseCodexAccountChecked(ctx, leaseID)
}

func (s *Proxy) releaseCodexAccountChecked(ctx context.Context, leaseID string) error {
	if strings.TrimSpace(leaseID) == "" {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	identity := codexLeaseRequest(ctx)
	accountID := usagetarget.Snapshot(ctx).AccountID
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		result := s.SendEvent(event.NewEventWithContext(accevents.TopicRelease, s.ID(), acccommon.UnitID, event.NewHeader(), cleanup, accevents.ReleaseCommand{LeaseID: leaseID}))
		if result == nil {
			lastErr = fmt.Errorf("missing Codex lease release result")
		} else {
			value, err := result.Get()
			lastErr = err
			if err == nil {
				if _, ok := value.(accevents.ReleaseResult); ok {
					if attempt > 1 {
						slog.Info("Codex lease release recovered", "lease_id", leaseID, "request_id", identity.RequestID, "attempt", attempt)
					}
					return nil
				}
				lastErr = fmt.Errorf("invalid Codex lease release response: %v", value)
			}
		}
		slog.Error("Codex lease release failed", "lease_id", leaseID, "account_id", accountID, "request_id", identity.RequestID, "event_id", identity.EventID, "api_key_id", identity.APIKeyID, "attempt", attempt, "error", lastErr)
		// Telemetry is best effort when the owner itself is unavailable. The
		// original failure above remains visible even if its counter cannot be sent.
		report := s.SendEvent(event.NewEventWithContext(accevents.TopicReleaseFailure, s.ID(), acccommon.UnitID, event.NewHeader(), cleanup, accevents.ReleaseFailureCommand{LeaseID: leaseID, AccountID: accountID}))
		if report != nil {
			value, err := report.Get()
			if err != nil {
				slog.Warn("Codex lease release failure counter unavailable", "lease_id", leaseID, "error", err)
			} else if _, ok := value.(accevents.ReleaseFailureResult); !ok {
				slog.Warn("Codex lease release failure counter unavailable", "lease_id", leaseID, "error", fmt.Errorf("invalid release failure counter response: %v", value))
			}
		} else {
			slog.Warn("Codex lease release failure counter unavailable", "lease_id", leaseID, "error", "missing result")
		}
		if attempt == 3 || cleanup.Err() != nil {
			break
		}
		timer := time.NewTimer(time.Duration(attempt) * 25 * time.Millisecond)
		select {
		case <-timer.C:
		case <-cleanup.Done():
			timer.Stop()
		}
		if cleanup.Err() != nil {
			break
		}
	}
	slog.Error("Codex lease release unresolved", "lease_id", leaseID, "account_id", accountID, "request_id", identity.RequestID, "error", lastErr)
	return lastErr
}
