package store

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	events "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	"aetherrelay/internal/pkg/aetherrelaystate"
	"github.com/google/uuid"
)

func requireDevice(t *testing.T, value string) {
	t.Helper()
	id, err := uuid.Parse(value)
	if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 {
		t.Fatalf("invalid device identity %q", value)
	}
}

func TestDeviceIdentityIsStableAcrossConcurrencyRotationAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.duckdb")
	s := New(path, 6, encryptedTestCodec(t))
	var wg sync.WaitGroup
	devices := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.Add([]string{"token-a"}, "web"); err != nil {
				t.Error(err)
				return
			}
			view, ok := s.ViewForAccessToken("token-a")
			if !ok {
				t.Error("missing account")
				return
			}
			devices <- view.DeviceID
		}()
	}
	wg.Wait()
	close(devices)
	var device string
	for got := range devices {
		requireDevice(t, got)
		if device != "" && device != got {
			t.Fatal("concurrent initialization changed device")
		}
		device = got
	}
	before, _ := s.ViewForAccessToken("token-a")
	if _, _, err := s.Add([]string{"token-b"}, "web"); err != nil {
		t.Fatal(err)
	}
	other, _ := s.ViewForAccessToken("token-b")
	if other.DeviceID == device {
		t.Fatal("different accounts share device")
	}
	if _, rotated, err := s.ApplyRefreshedToken("token-a", "token-new", "refresh", ""); err != nil || !rotated {
		t.Fatalf("rotate: %v", err)
	}
	after, _ := s.ViewForAccessToken("token-a")
	if after.DeviceID != device || after.ID != before.ID {
		t.Fatal("rotation changed stable identity")
	}
	candidates, err := s.ListDiscoveryCandidates()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range candidates.Candidates {
		if c.AccountID == after.ID && c.DeviceID != device {
			t.Fatal("discovery identity changed")
		}
	}
	raw, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), device) {
		t.Fatal("internal device leaked through account JSON")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restored := New(path, 6, encryptedTestCodec(t))
	defer restored.Close()
	view, ok := restored.ViewForAccessToken("token-new")
	if !ok || view.DeviceID != device {
		t.Fatal("restart changed device")
	}
}

func TestMissingDeviceIdentityIsRejectedWithoutRewritingState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.duckdb")
	codec := encryptedTestCodec(t)
	s := New(path, 1, codec)
	if _, _, err := s.Add([]string{"token"}, "web"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil {
		t.Fatal(err)
	}
	row := rows[0]
	payload, err := codec.Open(secureDocumentScope, row.ID, row.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	delete(document, "web_device_id")
	document["unknown_extension"] = "keep"
	document["quota"] = float64(7)
	payload, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	row.Payload, err = codec.Seal(secureDocumentScope, row.ID, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.documents.ApplySecureDocuments(secureDocumentScope, []aetherrelaystate.SecureDocumentRow{row}, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := s.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil || len(stored) != 1 {
		t.Fatalf("rows=%d err=%v", len(stored), err)
	}
	row = stored[0]
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if pool, err := Open(path, "128MB", 1, 1, codec); err == nil {
		_ = pool.Close()
		t.Fatal("missing device identity was accepted")
	}
	docs, err := aetherrelaystate.Open(path, "128MB", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer docs.Close()
	after, err := docs.LoadSecureDocuments(secureDocumentScope)
	if err != nil || len(after) != 1 {
		t.Fatalf("rows=%d err=%v", len(after), err)
	}
	if !bytes.Equal(after[0].Payload, row.Payload) || !after[0].UpdatedAt.Equal(row.UpdatedAt) {
		t.Fatal("rejected account was rewritten")
	}
}

func TestDeviceIdentityIsPublishedOnlyAfterSuccessfulSave(t *testing.T) {
	for _, tc := range []struct {
		name   string
		create func(*Store) error
	}{
		{"add", func(s *Store) error { _, _, err := s.Add([]string{"new-token"}, "web"); return err }},
		{"token import", func(s *Store) error { _, _, _, err := s.Import([]string{"new-token"}, nil, "web"); return err }},
		{"oauth import", func(s *Store) error {
			_, _, _, err := s.Import(nil, []events.ExportItem{{AccessToken: "new-token", RefreshToken: "refresh"}}, "oauth_import")
			return err
		}},
		{"oauth login", func(s *Store) error { _, _, err := s.AddOAuth("new-token", "refresh", ""); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, codec := filepath.Join(t.TempDir(), "state.duckdb"), encryptedTestCodec(t)
			s := New(path, 1, codec)
			defer s.Close()
			if _, _, err := s.AddOAuth("existing-token", "existing-refresh", ""); err != nil {
				t.Fatal(err)
			}
			before, version := s.List(), s.CatalogVersion()
			if err := s.documents.Close(); err != nil {
				t.Fatal(err)
			}
			if err := tc.create(s); err == nil {
				t.Fatal("expected persistence failure")
			}
			if _, found := s.ViewForAccessToken("new-token"); found {
				t.Fatal("failed creation published an uncommitted account or device identity")
			}
			if !reflect.DeepEqual(s.List(), before) || s.CatalogVersion() != version {
				t.Fatal("failed creation changed existing accounts or catalog version")
			}
			candidates, err := s.ListDiscoveryCandidates()
			if err != nil || len(candidates.Candidates) != 1 || candidates.Candidates[0].AccountID != before[0].ID {
				t.Fatalf("failed creation reached discovery: %+v err=%v", candidates, err)
			}
			s.documents, err = aetherrelaystate.Open(path, "128MB", 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.create(s); err != nil {
				t.Fatalf("retry after storage recovery: %v", err)
			}
			created, found := s.ViewForAccessToken("new-token")
			if !found {
				t.Fatal("successful retry did not publish the new account")
			}
			requireDevice(t, created.DeviceID)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			restored := New(path, 1, codec)
			defer restored.Close()
			persisted, found := restored.ViewForAccessToken("new-token")
			if !found || persisted.DeviceID != created.DeviceID || persisted.ID != created.ID {
				t.Fatal("successful retry published an identity that was not persisted")
			}
		})
	}
}

func TestFailedCredentialReplacementPreservesAccountAndSchedulingState(t *testing.T) {
	for _, oauth := range []bool{false, true} {
		t.Run(map[bool]string{false: "import", true: "oauth"}[oauth], func(t *testing.T) {
			s := New(filepath.Join(t.TempDir(), "state.duckdb"), 2, encryptedTestCodec(t))
			defer s.Close()
			before, _, err := s.AddOAuth("old-token", "old-refresh", "")
			if err != nil {
				t.Fatal(err)
			}
			beforeDocument := accountToMap(s.items["old-token"])
			s.imageInflight["old-token"] = 1
			version := s.CatalogVersion()
			if err := s.documents.Close(); err != nil {
				t.Fatal(err)
			}
			if oauth {
				_, _, err = s.UpsertOAuth("new-token", "new-refresh", "", before.ID)
			} else {
				_, _, _, err = s.Import(nil, []events.ExportItem{
					{AccessToken: "new-token", RefreshToken: "new-refresh", TargetID: before.ID},
					{AccessToken: "another-token", RefreshToken: "another-refresh"},
				}, "oauth_import")
			}
			if err == nil {
				t.Fatal("expected persistence failure")
			}
			after, found := s.ViewForAccessToken("old-token")
			if !found || !reflect.DeepEqual(after, before) || !reflect.DeepEqual(accountToMap(s.items["old-token"]), beforeDocument) || len(s.List()) != 1 {
				t.Fatal("failed replacement changed the original account")
			}
			if _, found := s.ViewForAccessToken("new-token"); found || len(s.aliases) != 0 || !reflect.DeepEqual(s.imageInflight, map[string]int{"old-token": 1}) || s.CatalogVersion() != version {
				t.Fatal("failed replacement published credentials, aliases, leases or catalog changes")
			}
		})
	}
}

func TestCredentialExportsDoNotCloneDeviceIdentity(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "source.duckdb"), 1, encryptedTestCodec(t))
	defer s.Close()
	item := events.ExportItem{AccessToken: "token", RefreshToken: "refresh", AccountID: "upstream", Email: "a@example.com"}
	if _, _, _, err := s.Import(nil, []events.ExportItem{item}, "oauth_import"); err != nil {
		t.Fatal(err)
	}
	original, _ := s.ViewForAccessToken("token")
	exports := s.Export(nil)
	data, err := json.Marshal(exports)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), original.DeviceID) {
		t.Fatal("ordinary export copied device")
	}
	target := New(filepath.Join(t.TempDir(), "target.duckdb"), 1, encryptedTestCodec(t))
	defer target.Close()
	if _, _, _, err := target.Import(nil, exports, "oauth_import"); err != nil {
		t.Fatal(err)
	}
	copied, _ := target.ViewForAccessToken("token")
	requireDevice(t, copied.DeviceID)
	if copied.DeviceID == original.DeviceID {
		t.Fatal("credential copy reused source device")
	}
	item.AccessToken = "new-token"
	item.TargetID = original.ID
	if _, _, _, err := s.Import(nil, []events.ExportItem{item}, "oauth_import"); err != nil {
		t.Fatal(err)
	}
	updated, _ := s.ViewForAccessToken("new-token")
	if updated.DeviceID != original.DeviceID {
		t.Fatal("credential replacement changed target device")
	}
}

func TestMalformedDeviceIsQuarantinedWithoutRotationOrBlockingHealthyAccounts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"malformed", "malformed-device"},
		{"wrong_version", "00000000-0000-1000-8000-000000000000"},
		{"noncanonical", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"},
		{"wrong_type", float64(123)},
		{"null", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, codec := filepath.Join(t.TempDir(), "state.duckdb"), encryptedTestCodec(t)
			seed := New(path, 6, codec)
			for _, token := range []string{"bad-token", "good-token"} {
				if _, _, err := seed.AddOAuth(token, "refresh-"+token, ""); err != nil {
					t.Fatal(err)
				}
				quota := 5
				view, _, err := seed.Update(token, "plus", StatusNormal, &quota, "")
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := seed.PutModelSnapshot(view.ID, events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-6-sol", Capabilities: []string{events.ModelCapabilityTextGeneration, events.ModelCapabilityImageGeneration}}}, DiscoveredAt: time.Now().UTC().Format(time.RFC3339), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}); err != nil {
					t.Fatal(err)
				}
			}
			bad, _ := seed.ViewForAccessToken("bad-token")
			good, _ := seed.ViewForAccessToken("good-token")
			rows, err := seed.documents.LoadSecureDocuments(secureDocumentScope)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.ID != bad.ID {
					continue
				}
				payload, err := codec.Open(secureDocumentScope, row.ID, row.Payload)
				if err != nil {
					t.Fatal(err)
				}
				var doc map[string]any
				if err := json.Unmarshal(payload, &doc); err != nil {
					t.Fatal(err)
				}
				doc["web_device_id"] = tc.value
				payload, err = json.Marshal(doc)
				if err != nil {
					t.Fatal(err)
				}
				row.Payload, err = codec.Seal(secureDocumentScope, row.ID, payload)
				if err != nil {
					t.Fatal(err)
				}
				if err := seed.documents.ApplySecureDocuments(secureDocumentScope, []aetherrelaystate.SecureDocumentRow{row}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := seed.Close(); err != nil {
				t.Fatal(err)
			}
			pool := New(path, 6, codec)
			defer pool.Close()
			view, _ := pool.ViewForAccessToken("bad-token")
			if view.Status != StatusAbnormal || view.DeviceIdentityError != "invalid_device_identity" || pool.Health().Abnormal != 1 || pool.Health().Normal != 1 {
				t.Fatalf("invalid identity not diagnosed: %+v, health=%+v", view, pool.Health())
			}
			for _, status := range []string{StatusNormal, StatusDisabled} {
				if _, _, err := pool.Update("bad-token", "plus", status, nil, ""); err != nil {
					t.Fatal(err)
				}
				if _, ok := pool.AcquireTextToken([]string{"good-token"}, "gpt-6-sol", events.ModelCapabilityTextGeneration); ok {
					t.Fatal("identity block bypassed through text acquisition")
				}
				if _, ok := pool.AcquireImageAccount(bad.ID); ok {
					t.Fatal("identity block bypassed through image resume")
				}
				if got, ok := pool.AcquireTextToken(nil, "gpt-6-sol", events.ModelCapabilityTextGeneration); !ok || got.ID != good.ID {
					t.Fatal("healthy text account blocked")
				}
				if got, ok := pool.AcquireImageToken("", "", nil, "gpt-6-sol", events.ModelCapabilityImageGeneration); !ok || got.ID != good.ID {
					t.Fatal("healthy image account blocked")
				}
				pool.ReleaseImageSlot("good-token")
				if got := pool.RefreshCandidatesFor([]string{"bad-token"}); len(got) != 0 {
					t.Fatal("invalid identity admitted to explicit Web refresh")
				}
				candidates, err := pool.ListDiscoveryCandidates()
				if err != nil || len(candidates.Candidates) != 1 || candidates.Candidates[0].AccountID != good.ID || pool.CatalogSnapshot().AvailableAccounts != 1 {
					t.Fatal("invalid identity advertised in model discovery/catalog")
				}
			}
			if _, ok := pool.OAuthRefreshCredentialFor("bad-token"); ok {
				t.Fatal("invalid identity triggered credential rotation")
			}
			rows, err = pool.documents.LoadSecureDocuments(secureDocumentScope)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.ID != bad.ID {
					continue
				}
				payload, err := codec.Open(secureDocumentScope, row.ID, row.Payload)
				if err != nil {
					t.Fatal(err)
				}
				var doc map[string]any
				if err := json.Unmarshal(payload, &doc); err != nil || !reflect.DeepEqual(doc["web_device_id"], tc.value) {
					t.Fatal("malformed identity was silently replaced")
				}
			}
			if err := pool.Close(); err != nil {
				t.Fatal(err)
			}
			restored := New(path, 6, codec)
			defer restored.Close()
			restoredBad, _ := restored.ViewForAccessToken("bad-token")
			restoredGood, _ := restored.ViewForAccessToken("good-token")
			if restoredBad.DeviceIdentityError != "invalid_device_identity" || restoredGood.DeviceID != good.DeviceID {
				t.Fatal("restart lost identity block or rotated healthy device")
			}
		})
	}
}
