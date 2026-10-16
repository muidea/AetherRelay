package store

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

func TestModelBlacklistAdmissionCatalogAndPersistence(t *testing.T) {
	path, codec := filepath.Join(t.TempDir(), "state.duckdb"), encryptedTestCodec(t)
	s, err := Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, _, _, err = s.Import([]events.CredentialInput{
		{AccessToken: "access-1", RefreshToken: "refresh-1", Email: "same@example.com"},
		{AccessToken: "access-2", RefreshToken: "refresh-2", Email: "same@example.com"},
	}); err != nil {
		t.Fatal(err)
	}
	ids := s.List()
	snapshot := events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-sol"}, {ID: "gpt-luna"}}, ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}
	for _, a := range ids {
		if _, _, err = s.PutModelSnapshot(a.ID, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	before := s.CatalogSnapshot().Version
	view, err := s.UpdateAccount(events.UpdateCommand{ID: ids[0].ID, BlockedModels: events.ModelBlacklist{" gpt-sol ", "future-model", "gpt-sol"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(view.BlockedModels, []string{"future-model", "gpt-sol"}) || view.ModelSnapshot == nil || len(view.ModelSnapshot.Models) != 2 || len(view.AvailableModels) != 1 || view.AvailableModels[0] != "gpt-luna" {
		t.Fatalf("view=%+v", view)
	}
	if s.CatalogSnapshot().Version <= before {
		t.Fatal("catalog generation did not change")
	}
	for _, transport := range []string{events.TransportResponses, events.TransportCompact, events.TransportWebsocket} {
		a, err := s.AcquirePreferredTransport(" gpt-sol ", nil, ids[0].ID, transport)
		if err != nil || a.AccountID != ids[1].ID {
			t.Fatalf("%s selected blocked preferred account: %+v %v", transport, a, err)
		}
		if _, err = s.AcquirePreferredTransport("gpt-luna", []string{ids[1].ID}, ids[0].ID, transport); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.CheckModelPolicy(ids[0].ID, "gpt-sol"); got.Allowed || got.Reason != "model_blocked" {
		t.Fatal(got)
	}
	if got := s.CheckModelPolicy(ids[0].ID, "GPT-sol"); !got.Allowed {
		t.Fatal("model policy matched case insensitively")
	}
	view.BlockedModels[0] = "mutated"
	if s.CheckModelPolicy(ids[0].ID, "future-model").Allowed {
		t.Fatal("management view aliases policy")
	}
	if _, err = s.UpdateAccount(events.UpdateCommand{ID: ids[1].ID, BlockedModels: events.ModelBlacklist{"gpt-sol"}}); err != nil {
		t.Fatal(err)
	}
	catalog := s.CatalogSnapshot()
	if len(catalog.Models) != 1 || catalog.Models[0].ID != "gpt-luna" {
		t.Fatalf("catalog=%+v", catalog)
	}
	for _, transport := range []string{events.TransportResponses, events.TransportCompact, events.TransportWebsocket} {
		a, err := s.AcquirePreferredTransport("gpt-sol", nil, ids[0].ID, transport)
		if err == nil || a.UnavailableReason != "model_blocked" || a.RetryAfterSeconds != 0 || a.AccessToken != "" {
			t.Fatalf("denial=%+v %v", a, err)
		}
	}
	for _, a := range s.List() {
		if a.Fail != 0 || len(a.Cooldowns) != 0 || a.Status != events.StatusNormal {
			t.Fatalf("policy poisoned health: %+v", a)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	if s.CheckModelPolicy(ids[0].ID, "gpt-sol").Allowed {
		t.Fatal("policy lost after restart")
	}
	if _, err = s.UpdateAccount(events.UpdateCommand{ID: ids[0].ID, BlockedModels: events.ModelBlacklist{}}); err != nil {
		t.Fatal(err)
	}
	if !s.CheckModelPolicy(ids[0].ID, "gpt-sol").Allowed || len(s.CatalogSnapshot().Models) != 2 {
		t.Fatal("clear did not restore capability")
	}
}

func TestModelBlacklistImportPreservesAndClears(t *testing.T) {
	s := openTestStore(t)
	input := events.CredentialInput{AccessToken: "access", RefreshToken: "refresh", AccountID: "upstream", Email: "same@example.com", BlockedModels: events.ModelBlacklist{"gpt-sol"}}
	if _, _, _, err := s.Import([]events.CredentialInput{input}); err != nil {
		t.Fatal(err)
	}
	id := s.List()[0].ID
	if _, _, err := s.PutModelSnapshot(id, events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-sol"}}}); err != nil {
		t.Fatal(err)
	}
	exported, ok := s.RefreshCredential(id)
	if !ok || !reflect.DeepEqual(exported.BlockedModels, events.ModelBlacklist{"gpt-sol"}) {
		t.Fatalf("export=%+v", exported)
	}
	input.BlockedModels = nil
	if _, _, _, err := s.Import([]events.CredentialInput{input}); err != nil {
		t.Fatal(err)
	}
	if s.CheckModelPolicy(id, "gpt-sol").Allowed {
		t.Fatal("omitted import cleared policy")
	}
	input.BlockedModels = events.ModelBlacklist{}
	if _, updated, _, err := s.Import([]events.CredentialInput{input}); err != nil || updated != 1 {
		t.Fatalf("clear updated=%d %v", updated, err)
	}
	view, _ := s.View(id)
	if len(view.BlockedModels) != 0 || view.ModelSnapshot == nil {
		t.Fatal("policy-only import changed capabilities", view)
	}
	input.BlockedModels = events.ModelBlacklist{"future-model"}
	if _, _, _, err := s.Import([]events.CredentialInput{input}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutModelSnapshot(id, events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-sol"}, {ID: "future-model"}}}); err != nil {
		t.Fatal(err)
	}
	if a, err := s.Acquire("future-model", nil); err == nil || a.UnavailableReason != "model_blocked" {
		t.Fatal("new discovery bypassed preconfigured blacklist", a, err)
	}
	if _, err := s.ApplyRefresh(id, events.CredentialInput{AccessToken: "rotated", BlockedModels: events.ModelBlacklist{}}); err != nil {
		t.Fatal(err)
	}
	if s.CheckModelPolicy(id, "future-model").Allowed {
		t.Fatal("token refresh cleared local policy")
	}
	input.TargetID = id
	input.Reauthenticate = true
	input.AccessToken = "new-access"
	input.RefreshToken = "new-refresh"
	input.BlockedModels = events.ModelBlacklist{}
	if _, _, _, err := s.Import([]events.CredentialInput{input}); err != nil {
		t.Fatal(err)
	}
	if s.CheckModelPolicy(id, "future-model").Allowed {
		t.Fatal("reauth cleared local policy")
	}
}

func TestInvalidModelBlacklistBatchDoesNotMutate(t *testing.T) {
	s := openTestStore(t)
	if _, _, _, err := s.Import([]events.CredentialInput{{AccessToken: "a", RefreshToken: "r"}, {AccessToken: "b", RefreshToken: "s", BlockedModels: events.ModelBlacklist{"bad\x00model"}}}); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if len(s.List()) != 0 {
		t.Fatal("invalid batch partially imported")
	}
}

func TestModelBlacklistKeepsOnlyPermittedRetryHints(t *testing.T) {
	s := openTestStore(t)
	if _, _, _, err := s.Import([]events.CredentialInput{{AccessToken: "a", RefreshToken: "r", BlockedModels: events.ModelBlacklist{"gpt-sol"}}, {AccessToken: "b", RefreshToken: "s"}}); err != nil {
		t.Fatal(err)
	}
	ids := s.List()
	for _, a := range ids {
		if _, _, err := s.PutModelSnapshot(a.ID, events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-sol"}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecordResult(ids[0].ID, "gpt-sol", false, events.ErrorRateLimit, 3600, false, "", false); err != nil {
		t.Fatal(err)
	}
	busy, err := s.AcquirePreferredTransportWithBusy("gpt-sol", nil, []string{ids[1].ID}, ids[0].ID, events.TransportResponses)
	if err == nil || busy.UnavailableReason != "accounts_busy" || busy.RetryAfterSeconds != 0 {
		t.Fatalf("busy=%+v %v", busy, err)
	}
	if _, err = s.RecordResult(ids[1].ID, "gpt-sol", false, events.ErrorRateLimit, 60, false, "", false); err != nil {
		t.Fatal(err)
	}
	cooling, err := s.Acquire("gpt-sol", nil)
	if err == nil || cooling.UnavailableReason != "accounts_cooling" || cooling.RetryAfterSeconds < 1 || cooling.RetryAfterSeconds > 60 {
		t.Fatalf("cooling=%+v %v", cooling, err)
	}
}
