package store

import (
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

func TestProbeAdmissionLocalReasons(t *testing.T) {
	s := openTestStore(t)
	_, _, _, ids, err := s.ImportWithIDs([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}})
	if err != nil {
		t.Fatal(err)
	}
	id := ids[0]
	_, _, err = s.PutModelSnapshot(id, events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-test"}}, ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, reason string
		alter        func(*account)
	}{
		{"disabled", "account_disabled", func(a *account) { a.Status = events.StatusDisabled }},
		{"abnormal", "account_abnormal", func(a *account) { a.Status = events.StatusAbnormal }},
		{"missing_credential", "credential_missing", func(a *account) { a.AccessToken = "" }},
		{"permanent", "credential_permanently_invalid", func(a *account) { a.PermanentAuthFailure = true }},
		{"blacklist", "model_blocked", func(a *account) { a.BlockedModels = events.ModelBlacklist{"gpt-test"} }},
		{"expired_snapshot", "snapshot_expired", func(a *account) {
			copy := *a.ModelSnapshot
			copy.ExpiresAt = time.Now().Add(-time.Hour).Format(time.RFC3339)
			a.ModelSnapshot = &copy
		}},
		{"missing_snapshot", "snapshot_expired", func(a *account) { a.ModelSnapshot = nil }},
		{"cooldown", "rate_limit", func(a *account) {
			a.Cooldowns = map[string]cooldown{"gpt-test": {Until: time.Now().Add(time.Hour), ErrorClass: "rate_limit"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.mu.Lock()
			old := *s.items[id]
			tc.alter(s.items[id])
			s.mu.Unlock()
			out := s.ProbeAdmission(events.ProbeAdmissionCommand{AccountID: id, Model: "gpt-test", Acquire: true}, 0)
			if out.Allowed || out.Reason != tc.reason || out.Credential.AccessToken != "" {
				t.Fatal(out)
			}
			s.mu.Lock()
			*s.items[id] = old
			s.mu.Unlock()
		})
	}
	if out := s.ProbeAdmission(events.ProbeAdmissionCommand{AccountID: "missing", Model: "gpt-test"}, 0); out.Allowed || out.Reason != "account_missing" {
		t.Fatal(out)
	}
	// Ordinary traffic can avoid an old negative WS snapshot; a manual probe
	// must still be able to observe whether that upstream capability changed.
	if _, err := s.RecordTransportCapability(id, "websocket", false); err != nil {
		t.Fatal(err)
	}
	if out := s.ProbeAdmission(events.ProbeAdmissionCommand{AccountID: id, Model: "gpt-test", Acquire: true}, 0); !out.Allowed || out.Credential.AccountID != id {
		t.Fatal("old transport snapshot prevented explicit detection", out)
	}
}

func TestProbeConfigVersionTracksExecutionNotNormalRefresh(t *testing.T) {
	s := openTestStore(t)
	_, _, _, ids, err := s.ImportWithIDs([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}})
	if err != nil {
		t.Fatal(err)
	}
	id := ids[0]
	cmd := events.ProbeAdmissionCommand{AccountID: id, Model: "gpt-test"}
	initial := s.ProbeAdmission(cmd, 0).ConfigVersion
	if _, err := s.ApplyRefresh(id, events.CredentialInput{AccessToken: "fresh-access", RefreshToken: "fresh-refresh"}); err != nil {
		t.Fatal(err)
	}
	if s.ProbeAdmission(cmd, 0).ConfigVersion != initial {
		t.Fatal("normal refresh invalidated execution epoch")
	}
	proxy := "http://localhost:8080"
	if _, err := s.UpdateAccount(events.UpdateCommand{ID: id, Proxy: &proxy}); err != nil {
		t.Fatal(err)
	}
	changed := s.ProbeAdmission(cmd, 0).ConfigVersion
	if changed == initial || changed == "" {
		t.Fatal("proxy change retained epoch")
	}
	if _, err := s.ApplyRefreshVersion(id, events.CredentialInput{AccessToken: "retired-access"}, initial); err == nil {
		t.Fatal("stale probe refresh overwrote current credentials")
	}
	if _, err := s.RecordRefreshFailureVersion(id, events.ErrorInvalidToken, true, initial); err == nil {
		t.Fatal("stale refresh modified current health")
	}
	credential, ok := s.RefreshCredential(id)
	if !ok || credential.AccessToken != "fresh-access" || s.List()[0].PermanentAuthFailure {
		t.Fatal("stale refresh changed account")
	}
	if _, err := s.UpdateAccount(events.UpdateCommand{ID: id, Proxy: &proxy}); err != nil {
		t.Fatal(err)
	}
	if s.ProbeAdmission(cmd, 0).ConfigVersion != changed {
		t.Fatal("unchanged update invalidated epoch")
	}
	if _, _, _, _, err := s.ImportWithIDs([]events.CredentialInput{{TargetID: id, AccessToken: "reauth-access", RefreshToken: "reauth-refresh"}}); err != nil {
		t.Fatal(err)
	}
	if s.ProbeAdmission(cmd, 0).ConfigVersion == changed {
		t.Fatal("reauthorization retained epoch")
	}
}
