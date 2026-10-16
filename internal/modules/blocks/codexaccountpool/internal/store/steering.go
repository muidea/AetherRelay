package store

import (
	"strings"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/google/uuid"
)

// ProbeAdmission is called under the owner scheduler lock, so concurrency
// observation and the subsequent owner lease reservation form one operation.
// For a lease recheck, only the owner may exclude the caller's verified lease
// from inflight; this store does not trust LeaseID or grant that exemption.
func (s *Store) ProbeAdmission(cmd events.ProbeAdmissionCommand, inflight int) events.ProbeAdmissionResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[strings.TrimSpace(cmd.AccountID)]
	view := modelAvailability(item, cmd.Model, time.Now().UTC())
	out := events.ProbeAdmissionResult{Allowed: view.Available, Reason: view.Reason}
	if item == nil {
		return out
	}
	if item.ProbeConfigVersion == "" {
		item.ProbeConfigVersion = uuid.NewString()
	}
	out.AccountLabel, out.ConfigVersion = item.Email, item.ProbeConfigVersion
	limit := item.MaxConcurrency
	if !validMaxConcurrency(limit) {
		limit = events.DefaultMaxConcurrency
	}
	if out.Allowed && inflight >= limit {
		out.Allowed, out.Reason = false, "accounts_busy"
	}
	if !out.Allowed {
		return out
	}
	expiry, known := accessTokenExpiry(item.AccessToken)
	if !known {
		expiry, known = parseExpiry(item.Expired)
	}
	out.RefreshNeeded = known && !expiry.After(time.Now().Add(5*time.Minute))
	if out.RefreshNeeded && strings.TrimSpace(item.RefreshToken) == "" {
		out.Allowed = false
		out.Reason = "refresh_credential_missing"
		return out
	}
	if cmd.Acquire {
		out.Credential = s.acquireResult(item)
	}
	return out
}
