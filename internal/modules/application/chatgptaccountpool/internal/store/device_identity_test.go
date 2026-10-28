package store

import (
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

func TestLegacyDeviceMigrationPreservesStateAndUnknownFields(t *testing.T) {
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
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	migrated := New(path, 1, codec)
	acc := migrated.items["token"]
	requireDevice(t, acc.DeviceID)
	device := acc.DeviceID
	if acc.ID != row.ID || acc.Quota != 7 || acc.Extra["unknown_extension"] != "keep" {
		t.Fatal("migration changed existing state")
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := New(path, 1, codec)
	defer reopened.Close()
	if reopened.items["token"].DeviceID != device {
		t.Fatal("migration did not persist identity")
	}
}

func TestDeviceIdentityIsPublishedOnlyAfterSuccessfulSave(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state.duckdb"), 1, encryptedTestCodec(t))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	acc := &Account{ID: "test", AccessToken: "token", Status: StatusNormal, SourceType: "web"}
	s.items["token"] = acc
	s.order = []string{"token"}
	if err := s.saveLocked(); err == nil {
		t.Fatal("expected persistence failure")
	}
	if acc.DeviceID != "" {
		t.Fatal("uncommitted device identity published")
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
				if _, ok := pool.AcquireTextAccount(bad.ID, "gpt-6-sol", events.ModelCapabilityTextGeneration); ok {
					t.Fatal("identity block bypassed through targeted text acquisition")
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
