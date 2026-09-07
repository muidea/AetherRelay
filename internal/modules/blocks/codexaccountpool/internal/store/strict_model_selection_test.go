package store

import (
	"slices"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

func TestStrictModelSelectionMatchesAvailableList(t *testing.T) {
	// CP-SCHED-009: transport preference and sticky IDs never bypass exact
	// account-local model admission, including on the next failover acquire.
	for _, transport := range []string{events.TransportResponses, events.TransportCompact, events.TransportWebsocket} {
		for _, reason := range []string{"unsupported", "case mismatch", "alias", "unknown", "expired", "model cooldown", "account cooldown", "disabled", "credential missing", "usage limit"} {
			t.Run(transport+"/"+reason, func(t *testing.T) {
				s := openTestStore(t)
				_, _, _, ids, err := s.ImportWithIDs([]events.CredentialInput{{AccessToken: "bad-access", RefreshToken: "bad-refresh"}, {AccessToken: "good-access", RefreshToken: "good-refresh"}})
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				for _, id := range ids {
					_, _, err = s.PutModelSnapshot(id, events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-5.5"}}, ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)})
					if err != nil {
						t.Fatal(err)
					}
				}
				bad := s.items[ids[0]]
				supported := true
				bad.CompactSupported, bad.WebsocketSupported = &supported, &supported
				switch reason {
				case "unsupported":
					bad.ModelSnapshot.Models[0].ID = "gpt-6-astra"
				case "case mismatch":
					bad.ModelSnapshot.Models[0].ID = "GPT-5.5"
				case "alias":
					bad.ModelSnapshot.Models[0].ID = "gpt-5.5-latest"
				case "unknown":
					bad.ModelSnapshot = nil
				case "expired":
					bad.ModelSnapshot.ExpiresAt = now.Add(-time.Second).Format(time.RFC3339)
				case "model cooldown":
					bad.Cooldowns = map[string]cooldown{"gpt-5.5": {Until: now.Add(time.Minute), ErrorClass: events.ErrorModelNotFound}}
				case "account cooldown":
					bad.Cooldowns = map[string]cooldown{"": {Until: now.Add(time.Minute), ErrorClass: events.ErrorRateLimit}}
				case "disabled":
					bad.Status = events.StatusDisabled
				case "credential missing":
					bad.AccessToken = ""
				case "usage limit":
					bad.UsageSnapshot = &events.AccountUsageSnapshot{ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), Windows: []events.UsageWindow{{ID: "primary", LimitReached: true}}}
				}
				for _, model := range []string{"gpt-5.5", " gpt-5.5 "} {
					got, err := s.AcquirePreferredTransport(model, nil, ids[0], transport)
					if err != nil || got.AccountID != ids[1] {
						t.Fatalf("selected unsupported/sticky account: %+v %v", got, err)
					}
					view, _ := s.View(got.AccountID)
					if !slices.Contains(view.AvailableModels, "gpt-5.5") {
						t.Fatalf("selected outside available list: %+v", view)
					}
					// Candidate exhaustion cannot relax model admission.
					if got, err := s.AcquirePreferredTransport(model, []string{ids[1]}, ids[0], transport); err == nil || got.AccountID != "" {
						t.Fatalf("unsafe exhausted fallback: %+v %v", got, err)
					}
				}
			})
		}
	}
}

func TestStrictModelSelectionRechecksChangedState(t *testing.T) {
	s := openTestStore(t)
	_, _, _, ids, err := s.ImportWithIDs([]events.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.PutModelSnapshot(ids[0], events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-5.5"}}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.View(ids[0])
	if !slices.Contains(before.AvailableModels, "gpt-5.5") {
		t.Fatal("missing initial model")
	}
	_, err = s.RecordResult(ids[0], "gpt-5.5", false, events.ErrorModelNotFound, 0, false, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquirePreferred("gpt-5.5", nil, ids[0]); err == nil {
		t.Fatal("stale UI snapshot allowed account selection")
	}
	if got := modelAvailability(s.items[ids[0]], "gpt-6-astra", time.Now()); got.Available || got.Reason != "model_not_supported" {
		t.Fatalf("unlisted model: %+v", got)
	}
}
