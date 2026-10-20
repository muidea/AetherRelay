package biz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func probeAdmissionTest(t *testing.T, s *Account, cmd events.ProbeAdmissionCommand) events.ProbeAdmissionResult {
	t.Helper()
	r := event.NewResult(events.TopicProbeAdmission, "test", s.ID())
	s.handleProbeAdmission(event.NewEvent(events.TopicProbeAdmission, "test", s.ID(), nil, cmd), r)
	v, err := r.Get()
	if err != nil {
		t.Fatal(err)
	}
	return v.(events.ProbeAdmissionResult)
}

func TestProbeRefreshHonorsDeadlineAndLease(t *testing.T) {
	s, _, ids := newSchedulingAccount(t)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			t.Error("unexpected proxy request")
		}
		time.Sleep(150 * time.Millisecond)
		http.Error(w, "unavailable", 502)
	}))
	defer proxy.Close()
	proxyURL := proxy.URL
	if _, err := s.store.UpdateAccount(events.UpdateCommand{ID: ids[0], Proxy: &proxyURL}); err != nil {
		t.Fatal(err)
	}
	admitted := probeAdmissionTest(t, s, events.ProbeAdmissionCommand{AccountID: ids[0], Model: "gpt-test", Acquire: true})
	cmd := events.RefreshTokenCommand{AccountID: ids[0], ProbeLeaseID: admitted.Credential.LeaseID, ProbeConfigVersion: admitted.ConfigVersion}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	out, err := s.refreshTokenForProbe(ctx, cmd)
	if err == nil || !out.RequestSent || time.Since(started) > time.Second {
		t.Fatal("probe refresh escaped its context", out, err)
	}
	if len(s.refreshes) != 0 {
		t.Fatal("probe refresh left a flight")
	}
	releaseForTest(t, s, cmd.ProbeLeaseID)
	if _, err := s.refreshTokenForProbe(context.Background(), cmd); err == nil {
		t.Fatal("refresh accepted a released lease")
	}
}
func TestStrictProbeAdmissionAndLeaseIsolation(t *testing.T) {
	s, _, ids := newSchedulingAccount(t)
	before, _ := json.Marshal(s.store.List())
	cmd := events.ProbeAdmissionCommand{AccountID: ids[0], Model: "gpt-test"}
	pre := probeAdmissionTest(t, s, cmd)
	if !pre.Allowed || pre.ConfigVersion == "" || pre.Credential.AccessToken != "" || len(s.leases) != 0 {
		t.Fatal(pre)
	}
	cmd.Acquire = true
	first := probeAdmissionTest(t, s, cmd)
	second := probeAdmissionTest(t, s, cmd)
	third := probeAdmissionTest(t, s, cmd)
	if first.Credential.AccountID != ids[0] || second.Credential.AccountID != ids[0] || third.Allowed || third.Reason != "accounts_busy" || third.Credential.AccountID != "" {
		t.Fatal("switched to idle account or exceeded limit", first, second, third)
	}
	if len(s.sessions) != 0 || len(s.sessionCounts) != 0 {
		t.Fatal("production affinity changed")
	}
	releaseForTest(t, s, first.Credential.LeaseID)
	releaseForTest(t, s, second.Credential.LeaseID)
	after, _ := json.Marshal(s.store.List())
	if string(before) != string(after) || len(s.leases) != 0 || len(s.inflight) != 0 {
		t.Fatal("probe modified health, last_used or leaked lease")
	}
	if _, err := s.store.UpdateAccount(events.UpdateCommand{ID: ids[0], BlockedModels: events.ModelBlacklist{"gpt-test"}}); err != nil {
		t.Fatal(err)
	}
	denied := probeAdmissionTest(t, s, cmd)
	if denied.Allowed || denied.Reason != "model_blocked" || denied.Credential.AccessToken != "" || len(s.leases) != 0 {
		t.Fatal(denied)
	}
}

func TestProbeAdmissionCanonicalSlotConcurrency(t *testing.T) {
	s, _, ids := newSchedulingAccount(t)
	var leases []string
	for i := 0; i < events.DefaultMaxConcurrency; i++ {
		out := probeAdmissionTest(t, s, events.ProbeAdmissionCommand{AccountID: " \t" + ids[0] + " \n", Model: "gpt-test", Acquire: true})
		if !out.Allowed || out.Credential.AccountID != ids[0] || out.Credential.LeaseID == "" {
			t.Fatal(out)
		}
		leases = append(leases, out.Credential.LeaseID)
	}
	for _, slot := range []string{ids[0], " " + ids[0] + " ", "\t" + ids[0] + "\n"} {
		out := probeAdmissionTest(t, s, events.ProbeAdmissionCommand{AccountID: slot, Model: "gpt-test", Acquire: true})
		if out.Allowed || out.Reason != "accounts_busy" || out.Credential.LeaseID != "" {
			t.Fatal("slot alias bypassed concurrency", out)
		}
	}
	for _, lease := range leases {
		releaseForTest(t, s, lease)
	}
	if len(s.inflight) != 0 || len(s.leases) != 0 {
		t.Fatal("slot aliases leaked reservations")
	}
}

func TestProbeAdmissionRechecksOwnedLease(t *testing.T) {
	s, _, ids := newSchedulingAccount(t)
	limit := 1
	for _, id := range ids {
		if _, err := s.store.UpdateAccount(events.UpdateCommand{ID: id, MaxConcurrency: &limit}); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := json.Marshal(s.store.List())
	first := probeAdmissionTest(t, s, events.ProbeAdmissionCommand{AccountID: ids[0], Model: "gpt-test", Acquire: true})
	other := probeAdmissionTest(t, s, events.ProbeAdmissionCommand{AccountID: ids[1], Model: "gpt-test", Acquire: true})
	if !first.Allowed || !other.Allowed {
		t.Fatal(first, other)
	}
	for _, lease := range []string{"", "missing", other.Credential.LeaseID, first.Credential.LeaseID} {
		out := probeAdmissionTest(t, s, events.ProbeAdmissionCommand{AccountID: " " + ids[0] + " ", Model: "gpt-test", LeaseID: lease})
		wantAllowed := lease == first.Credential.LeaseID
		if out.Allowed != wantAllowed || out.ConfigVersion != first.ConfigVersion || out.Credential != (events.AcquireResult{}) {
			t.Fatal("invalid lease admission", lease, out)
		}
		if !wantAllowed {
			wantReason := "probe_lease_unavailable"
			if lease == "" {
				wantReason = "accounts_busy"
			}
			if out.Reason != wantReason {
				t.Fatal(out)
			}
		}
		if len(s.leases) != 2 || s.inflight[ids[0]] != 1 || s.inflight[ids[1]] != 1 {
			t.Fatal("recheck changed lease accounting")
		}
	}
	// An existing reservation cannot be reused to request a second lease.
	r := event.NewResult(events.TopicProbeAdmission, "test", s.ID())
	s.handleProbeAdmission(event.NewEvent(events.TopicProbeAdmission, "test", s.ID(), nil, events.ProbeAdmissionCommand{AccountID: ids[0], Model: "gpt-test", Acquire: true, LeaseID: first.Credential.LeaseID}), r)
	if _, err := r.Get(); err == nil {
		t.Fatal("acquire accepted a lease recheck command")
	}
	releaseForTest(t, s, first.Credential.LeaseID)
	if out := probeAdmissionTest(t, s, events.ProbeAdmissionCommand{AccountID: ids[0], Model: "gpt-test", LeaseID: first.Credential.LeaseID}); out.Allowed || out.Reason != "probe_lease_unavailable" {
		t.Fatal("released lease remained usable", out)
	}
	releaseForTest(t, s, other.Credential.LeaseID)
	after, _ := json.Marshal(s.store.List())
	if string(before) != string(after) || len(s.leases) != 0 || len(s.inflight) != 0 {
		t.Fatal("lease checks modified account health or leaked resources")
	}
}

func TestProbeLeaseRecheckStillEnforcesCooldown(t *testing.T) {
	s, _, ids := newSchedulingAccount(t)
	admitted := probeAdmissionTest(t, s, events.ProbeAdmissionCommand{AccountID: ids[0], Model: "gpt-test", Acquire: true})
	// Simulate an ordinary request learning a new limit after probe admission.
	if _, err := s.store.RecordResult(ids[0], "gpt-test", false, events.ErrorRateLimit, 60, false, "", false); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(s.store.List())
	out := probeAdmissionTest(t, s, events.ProbeAdmissionCommand{AccountID: ids[0], Model: "gpt-test", LeaseID: admitted.Credential.LeaseID})
	if out.Allowed || out.Reason != "rate_limit" || out.ConfigVersion != admitted.ConfigVersion || out.Credential.AccessToken != "" {
		t.Fatal("lease bypassed current policy", out)
	}
	releaseForTest(t, s, admitted.Credential.LeaseID)
	after, _ := json.Marshal(s.store.List())
	if string(before) != string(after) {
		t.Fatal("recheck modified observed health")
	}
}

func TestProbeLeaseRecheckExcludesOnlyOneReservation(t *testing.T) {
	s, _, ids := newSchedulingAccount(t)
	first := probeAdmissionTest(t, s, events.ProbeAdmissionCommand{AccountID: ids[0], Model: "gpt-test", Acquire: true})
	second := probeAdmissionTest(t, s, events.ProbeAdmissionCommand{AccountID: ids[0], Model: "gpt-test", Acquire: true})
	limit := 1
	if _, err := s.store.UpdateAccount(events.UpdateCommand{ID: ids[0], MaxConcurrency: &limit}); err != nil {
		t.Fatal(err)
	}
	out := probeAdmissionTest(t, s, events.ProbeAdmissionCommand{AccountID: ids[0], Model: "gpt-test", LeaseID: first.Credential.LeaseID})
	if out.Allowed || out.Reason != "accounts_busy" || len(s.leases) != 2 || s.inflight[ids[0]] != 2 {
		t.Fatal("recheck exempted other reservations", out)
	}
	releaseForTest(t, s, first.Credential.LeaseID)
	releaseForTest(t, s, second.Credential.LeaseID)
}
