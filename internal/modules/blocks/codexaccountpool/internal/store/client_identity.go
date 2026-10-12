package store

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	events "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	codexidentity "aetherrelay/internal/pkg/aetherrelaycodexidentity"
	"aetherrelay/internal/pkg/aetherrelaystate"
)

const sharedClientIdentityScope = "codex_client_identity"
const sharedClientIdentityID = "highest"

// The global client profile is encrypted separately from account credentials:
// deleting, replacing or disabling an account must not downgrade the service.
func (s *Store) loadClientIdentityLocked() error {
	s.clientIdentity = codexidentity.HighestObserved()
	rows, err := s.documents.LoadSecureDocuments(sharedClientIdentityScope)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.ID != sharedClientIdentityID {
			return fmt.Errorf("unexpected shared Codex client identity document")
		}
		payload, err := s.credentials.Open(sharedClientIdentityScope, row.ID, row.Payload)
		if err != nil {
			return fmt.Errorf("decrypt shared Codex client identity: %w", err)
		}
		var profile codexidentity.ObservedProfile
		if err := json.Unmarshal(payload, &profile); err != nil {
			return fmt.Errorf("decode shared Codex client identity: %w", err)
		}
		s.clientIdentity = codexidentity.HighestObserved(profile)
		s.persistedClientIdentity = sha256.Sum256(payload)
	}
	return nil
}

func (s *Store) saveClientIdentityLocked() error {
	if s.clientIdentity == codexidentity.HighestObserved() && s.persistedClientIdentity == ([sha256.Size]byte{}) {
		return nil
	}
	payload, err := json.Marshal(s.clientIdentity)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)
	if digest == s.persistedClientIdentity {
		return nil
	}
	sealed, err := s.credentials.Seal(sharedClientIdentityScope, sharedClientIdentityID, payload)
	if err != nil {
		return err
	}
	if err := s.documents.ApplySecureDocuments(sharedClientIdentityScope, []aetherrelaystate.SecureDocumentRow{{ID: sharedClientIdentityID, Payload: sealed}}, nil); err != nil {
		return err
	}
	s.persistedClientIdentity = digest
	return nil
}

func (s *Store) promoteSharedClientIdentityLocked(candidate events.ClientIdentityCandidate) bool {
	next := codexidentity.HighestObserved(s.clientIdentity, codexidentity.ObservedProfile{UserAgent: candidate.UserAgent, Originator: candidate.Originator})
	if next == s.clientIdentity {
		return false
	}
	s.clientIdentity = next
	return true
}

func (s *Store) sharedClientIdentityProfileLocked() events.ClientIdentityProfile {
	profile := codexidentity.HighestObserved(s.clientIdentity)
	return events.ClientIdentityProfile{UserAgent: profile.UserAgent, Originator: profile.Originator, Family: profile.Family, Version: profile.Version}
}

func (s *Store) modelSnapshotIdentityOutdatedLocked(snapshot *events.AccountModelSnapshot) bool {
	version := s.sharedClientIdentityProfileLocked().Version
	// Existing snapshots made at the built-in baseline remain usable until
	// their normal TTL. A learned newer version schedules another discovery
	// while preserving the prior model membership during the refresh.
	if snapshot.ClientVersion == "" {
		return version != codexidentity.Current().ClientVersion
	}
	return snapshot.ClientVersion != version
}
