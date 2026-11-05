package store

import (
	"testing"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

func TestRefreshSnapshotSurvivesPolicyChangeButFencesRotation(t *testing.T) {
	s := openTestStore(t)
	_, _, _, ids, err := s.ImportWithIDs([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}})
	if err != nil {
		t.Fatal(err)
	}
	id := ids[0]
	snapshot, ok := s.RefreshCredentialSnapshot(id, "")
	if !ok {
		t.Fatal("refresh snapshot unavailable")
	}
	concurrency := events.DefaultMaxConcurrency + 1
	if _, err := s.UpdateAccount(events.UpdateCommand{ID: id, MaxConcurrency: &concurrency}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyRefreshSnapshot(snapshot, events.CredentialInput{AccessToken: "rotated-access", RefreshToken: "rotated-refresh"}); err != nil {
		t.Fatal("policy change discarded rotated tokens", err)
	}
	if _, err := s.ApplyRefreshSnapshot(snapshot, events.CredentialInput{AccessToken: "stale-access"}); err == nil {
		t.Fatal("snapshot could be reused after rotation")
	}
	if _, err := s.RecordRefreshFailureSnapshot(snapshot, events.ErrorInvalidToken, true); err == nil {
		t.Fatal("old credential failure poisoned rotated tokens")
	}
	current, ok := s.RefreshCredentialSnapshot(id, "")
	if !ok || current.Credential.AccessToken != "rotated-access" {
		t.Fatal("rotation was lost")
	}
	if _, err := s.RecordRefreshFailureSnapshot(current, events.ErrorInvalidToken, true); err != nil {
		t.Fatal("current failure was rejected", err)
	}
	view, _ := s.View(id)
	if !view.PermanentAuthFailure {
		t.Fatal("current credential failure was not recorded")
	}
}

func TestRefreshSnapshotRetainsProbeConfigFence(t *testing.T) {
	s := openTestStore(t)
	_, _, _, ids, err := s.ImportWithIDs([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}})
	if err != nil {
		t.Fatal(err)
	}
	id := ids[0]
	version := s.ProbeAdmission(events.ProbeAdmissionCommand{AccountID: id, Model: "gpt-test"}, 0).ConfigVersion
	snapshot, ok := s.RefreshCredentialSnapshot(id, version)
	if !ok {
		t.Fatal("probe snapshot unavailable")
	}
	proxy := "http://localhost:8080"
	if _, err := s.UpdateAccount(events.UpdateCommand{ID: id, Proxy: &proxy}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyRefreshSnapshot(snapshot, events.CredentialInput{AccessToken: "stale-access"}); err == nil {
		t.Fatal("probe refresh ignored changed config")
	}
	if _, err := s.RecordRefreshFailureSnapshot(snapshot, events.ErrorInvalidToken, true); err == nil {
		t.Fatal("probe failure ignored changed config")
	}
}
