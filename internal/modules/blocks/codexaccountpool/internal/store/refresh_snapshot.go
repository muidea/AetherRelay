package store

import (
	"fmt"
	"strings"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/google/uuid"
)

// RefreshSnapshot binds both successful rotation and failure feedback to the
// credential read before the network call. Ordinary configuration changes do
// not invalidate that credential; probes additionally retain their config fence.
type RefreshSnapshot struct {
	Credential   events.CredentialInput
	accountID    string
	version      string
	probeVersion string
}

func (s *Store) RefreshCredentialSnapshot(id, probeVersion string) (RefreshSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[strings.TrimSpace(id)]
	if item == nil || probeVersion != "" && item.ProbeConfigVersion != probeVersion {
		return RefreshSnapshot{}, false
	}
	if item.CredentialVersion == "" {
		item.CredentialVersion = uuid.NewString()
	}
	return RefreshSnapshot{Credential: refreshCredential(item), accountID: item.ID, version: item.CredentialVersion, probeVersion: probeVersion}, true
}

func (s *Store) ApplyRefreshSnapshot(snapshot RefreshSnapshot, input events.CredentialInput) (events.RefreshTokenResult, error) {
	if snapshot.version == "" {
		return events.RefreshTokenResult{}, fmt.Errorf("refresh credential snapshot is required")
	}
	return s.applyRefresh(snapshot.accountID, input, snapshot.probeVersion, snapshot.version)
}

func (s *Store) RecordRefreshFailureSnapshot(snapshot RefreshSnapshot, errorClass string, permanent bool) (events.RefreshTokenResult, error) {
	if snapshot.version == "" {
		return events.RefreshTokenResult{}, fmt.Errorf("refresh credential snapshot is required")
	}
	return s.recordRefreshFailure(snapshot.accountID, errorClass, permanent, snapshot.probeVersion, snapshot.version)
}
