package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	events "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	codexidentity "aetherrelay/internal/pkg/aetherrelaycodexidentity"
)

func sharedIdentityCandidate(version string) events.ClientIdentityCandidate {
	return events.ClientIdentityCandidate{UserAgent: fmt.Sprintf("codex-tui/%s (Ubuntu; x86_64) terminal (codex-tui; %s)", version, version), Originator: "codex-tui"}
}

func addSharedIdentityAccounts(t *testing.T, s *Store) []string {
	t.Helper()
	if added, _, _, err := s.Import([]events.CredentialInput{
		{AccountID: "account-a", Email: "first@example.com", AccessToken: "access-a", RefreshToken: "refresh-a", FingerprintMode: events.FingerprintModeScoped},
		{AccountID: "account-b", Email: "second@example.com", AccessToken: "access-b", RefreshToken: "refresh-b", FingerprintMode: events.FingerprintModeOff},
	}); err != nil || added != 2 {
		t.Fatalf("import added=%d err=%v", added, err)
	}
	var ids []string
	for _, view := range s.List() {
		ids = append(ids, view.ID)
		for _, transport := range []string{events.TransportCompact, events.TransportWebsocket} {
			if _, err := s.RecordTransportCapability(view.ID, transport, true); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := s.PutModelSnapshot(view.ID, events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-test"}}, DiscoveredAt: time.Now().Format(time.RFC3339), ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func TestSharedHighestIdentityAppliesAcrossAccountsModesAndBackgroundRequests(t *testing.T) {
	s := openTestStore(t)
	ids := addSharedIdentityAccounts(t, s)
	newest := sharedIdentityCandidate("0.162.1")
	first, err := s.AcquirePreferredTransportWithBusyIdentity("gpt-test", nil, nil, ids[0], events.TransportResponses, newest)
	if err != nil || first.ClientIdentity.Version != "0.162.1" {
		t.Fatalf("first admission=%+v err=%v", first, err)
	}
	// CP-OBS-013: the admitted slot carries its safe management label.
	if first.AccountLabel != "first@example.com" {
		t.Fatalf("admission label=%q", first.AccountLabel)
	}
	for _, candidate := range []events.ClientIdentityCandidate{
		{},
		sharedIdentityCandidate("0.155.0"),
		{UserAgent: "claude-cli/99.0.0", Originator: "claude-cli"},
		{UserAgent: "codex-tui/99.0.0 (Ubuntu; x86_64) terminal (codex-tui; 99.0.0)"},
		{UserAgent: "codex-tui/0.162.1 (Windows; x86_64) alternate (codex-tui; 0.162.1)", Originator: "codex-tui"},
	} {
		for _, id := range ids {
			for _, transport := range []string{events.TransportResponses, events.TransportCompact, events.TransportWebsocket} {
				lease, err := s.AcquirePreferredTransportWithBusyIdentity("gpt-test", nil, nil, id, transport, candidate)
				if err != nil || lease.AccountID != id || lease.ClientIdentity != first.ClientIdentity {
					t.Fatalf("shared identity lost: lease=%+v err=%v", lease, err)
				}
			}
		}
	}
	for _, candidate := range s.ListDiscoveryCandidates(nil).Candidates {
		if candidate.ClientIdentity != first.ClientIdentity || !candidate.DiscoveryDue {
			t.Fatalf("old model snapshot was not rescheduled with highest identity: %+v", candidate)
		}
	}
	for _, candidate := range s.ListUsageCandidates(nil).Candidates {
		if candidate.ClientIdentity != first.ClientIdentity {
			t.Fatalf("usage read lost highest identity: %+v", candidate)
		}
	}
	if s.SharedClientIdentity() != first.ClientIdentity {
		t.Fatal("credential request lost highest identity")
	}
	if catalog := s.CatalogSnapshot(); len(catalog.Models) != 1 || len(catalog.Models[0].AccountIDs) != 2 {
		t.Fatalf("promotion discarded last good model membership: %+v", catalog)
	}
	// A result from a discovery started before promotion must not be stamped as
	// though it had used the new version, or the missing model would stay cached.
	old := events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-test"}}, ClientVersion: "0.155.0", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}
	if _, _, err := s.PutModelSnapshot(ids[1], old); err != nil {
		t.Fatal(err)
	}
	if !s.ListDiscoveryCandidates([]string{ids[1]}).Candidates[0].DiscoveryDue {
		t.Fatal("late old discovery result hid the need to resync")
	}
	old.ClientVersion = "0.162.1"
	old.Models = append(old.Models, events.AccountModelEntry{ID: "gpt-6.1-sol"})
	if _, _, err := s.PutModelSnapshot(ids[1], old); err != nil {
		t.Fatal(err)
	}
	if s.ListDiscoveryCandidates([]string{ids[1]}).Candidates[0].DiscoveryDue {
		t.Fatal("new-version discovery did not settle the resync")
	}
	refreshed, err := s.ApplyRefresh(ids[1], events.CredentialInput{AccessToken: "fresh-access"})
	if err != nil || refreshed.ClientIdentity != first.ClientIdentity {
		t.Fatalf("refresh result lost maximum: %+v err=%v", refreshed, err)
	}

	if payload, err := json.Marshal(s.ExportByIDs(ids)); err != nil || contains(string(payload), newest.UserAgent) || contains(string(payload), "client_identity") {
		t.Fatal("shared identity entered ordinary credential exports")
	}
}

func TestHighestIdentityRecoversLegacyProfilesAndSurvivesAccountDeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	codec := encryptedTestCodec(t)
	s, err := Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	ids := addSharedIdentityAccounts(t, s)
	newest := sharedIdentityCandidate("0.162.1")
	profile, _ := codexidentity.ParseObserved(newest.UserAgent, newest.Originator)
	s.items[ids[0]].ClientIdentityProfile = &profile
	older := sharedIdentityCandidate("0.155.0")
	oldProfile, _ := codexidentity.ParseObserved(older.UserAgent, older.Originator)
	s.items[ids[1]].ClientIdentityProfile = &oldProfile
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.SharedClientIdentity(); got.UserAgent != newest.UserAgent || got.Version != "0.162.1" {
		t.Fatalf("startup failed to recover highest legacy observation: %+v", got)
	}
	for _, id := range ids {
		if _, err := s.Delete([]string{id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ids = addSharedIdentityAccounts(t, s)
	lease, err := s.AcquirePreferredTransport("gpt-test", nil, ids[1], events.TransportResponses)
	if err != nil || lease.ClientIdentity.UserAgent != newest.UserAgent {
		t.Fatalf("account deletion/restart downgraded shared identity: %+v err=%v", lease, err)
	}
	rows, err := s.documents.LoadSecureDocuments(sharedClientIdentityScope)
	if err != nil || len(rows) != 1 || contains(string(rows[0].Payload), newest.UserAgent) {
		t.Fatalf("shared profile must remain encrypted: rows=%d err=%v", len(rows), err)
	}
}

func TestConcurrentClientObservationsKeepTheGlobalMaximum(t *testing.T) {
	s := openTestStore(t)
	ids := addSharedIdentityAccounts(t, s)
	var wg sync.WaitGroup
	for _, version := range []string{"0.159.2", "0.162.1", "0.155.0", "0.164.0", "0.161.0", "0.163.0"} {
		wg.Add(1)
		go func(version string) {
			defer wg.Done()
			if _, err := s.AcquirePreferredTransportWithBusyIdentity("gpt-test", nil, nil, ids[0], events.TransportResponses, sharedIdentityCandidate(version)); err != nil {
				t.Error(err)
			}
		}(version)
	}
	wg.Wait()
	if got := s.SharedClientIdentity(); got.Version != "0.164.0" {
		t.Fatalf("concurrent observations lost maximum: %+v", got)
	}
}

// Seed historical encrypted account records for migration coverage.
func seedLegacyClientIdentityProfile(item *account, candidate events.ClientIdentityCandidate) bool {
	if item == nil || item.FingerprintMode != events.FingerprintModeScoped {
		return false
	}
	current := codexidentity.ObservedProfile{}
	if item.ClientIdentityProfile != nil {
		current = *item.ClientIdentityProfile
	}
	next, promoted := codexidentity.PromoteObserved(current, codexidentity.ObservedProfile{UserAgent: candidate.UserAgent, Originator: candidate.Originator})
	if promoted {
		item.ClientIdentityProfile = &next
	}
	return promoted
}
