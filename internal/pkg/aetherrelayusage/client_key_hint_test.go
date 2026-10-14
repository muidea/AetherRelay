package usage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	"aetherrelay/internal/pkg/aetherrelayconfig"
)

func TestClientAPIKeyHintLifecycle(t *testing.T) {
	for _, backend := range []string{"memory", "duckdb"} {
		t.Run(backend, func(t *testing.T) {
			var store Store = NewMemoryStore()
			cfg := testCfg(filepath.Join(t.TempDir(), "hint.duckdb"))
			if backend == "duckdb" {
				var err error
				store, err = OpenDuckDB(cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _ = store.Close() })
			ctx := context.Background()
			at := time.Now().UTC().Truncate(time.Microsecond)
			record := ClientAPIKeyRecord{ID: "hint-key", Hash: "sha256:old", Hint: "sk_sx****ab", Enabled: true, CreatedAt: at, ProviderAccess: clientaccess.All()}
			if err := store.CreateClientAPIKey(ctx, record); err != nil {
				t.Fatal(err)
			}
			assertHint := func(hash, hint string) {
				t.Helper()
				keys, err := store.ListClientAPIKeys(ctx)
				if err != nil || keys[record.ID].Hash != hash || keys[record.ID].Hint != hint {
					t.Fatalf("credential pair mismatch: %+v err=%v", keys[record.ID], err)
				}
			}
			assertHint(record.Hash, record.Hint)
			if err := store.SetClientAPIKeyEnabled(ctx, record.ID, false); err != nil {
				t.Fatal(err)
			}
			policy, err := clientaccess.Selected([]string{"provider"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetClientAPIKeyProviderAccess(ctx, record.ID, policy); err != nil {
				t.Fatal(err)
			}
			if err := store.TouchClientAPIKey(ctx, record.ID, at); err != nil {
				t.Fatal(err)
			}
			assertHint(record.Hash, record.Hint)
			if err := store.RotateClientAPIKey(ctx, record.ID, "sha256:new", "sk_K9****X2", at); err != nil {
				t.Fatal(err)
			}
			assertHint("sha256:new", "sk_K9****X2")
			if backend == "duckdb" {
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				store, err = OpenDuckDB(cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertHint("sha256:new", "sk_K9****X2")
			}
			keys, err := store.ListClientAPIKeys(ctx)
			got := keys[record.ID]
			if err != nil || !got.Enabled || got.LastRotatedAt == nil || !got.LastRotatedAt.Equal(at) || !got.ProviderAccess.Allows("provider") || got.ProviderAccess.Mode != clientaccess.ModeSelected {
				t.Fatalf("rotation changed lifecycle/policy: %+v err=%v", got, err)
			}
			if err := store.BeginClientAPIKeyDeletion(ctx, record.ID, at); err != nil {
				t.Fatal(err)
			}
			if err := store.RotateClientAPIKey(ctx, record.ID, "sha256:rejected", "sk_no****pe", at); err == nil {
				t.Fatal("pending deletion accepted rotation")
			}
			assertHint("sha256:new", "sk_K9****X2")

			record.ID = config.BuiltinClientAPIKeyID
			if err := store.CreateClientAPIKey(ctx, record); err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureClientAPIKey(ctx, record.ID, at); err != nil {
				t.Fatal(err)
			}
			assertHint("", "")
		})
	}
}

func TestSchemaAddsClientAPIKeyHintWithoutChangingExistingKeys(t *testing.T) {
	ctx := context.Background()
	cfg := testCfg(filepath.Join(t.TempDir(), "legacy-hint.duckdb"))
	store, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	policy, err := clientaccess.Selected([]string{"provider"})
	if err != nil {
		t.Fatal(err)
	}
	record := ClientAPIKeyRecord{ID: "legacy", Hash: "sha256:legacy", Enabled: false, CreatedAt: at, ProviderAccess: policy}
	if err := store.CreateClientAPIKey(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := store.Start(ctx, StartRecord{EventID: "preserved", APIKeyID: record.ID, StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE client_api_key_metadata DROP COLUMN hint`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		store, err = OpenDuckDB(cfg)
		if err != nil {
			t.Fatal(err)
		}
		keys, err := store.ListClientAPIKeys(ctx)
		got := keys[record.ID]
		if err != nil || got.Hash != record.Hash || got.Hint != "" || got.Enabled || !got.CreatedAt.Equal(at) || got.ProviderAccess.Mode != clientaccess.ModeSelected || !got.ProviderAccess.Allows("provider") {
			t.Fatalf("existing key changed: %+v err=%v", got, err)
		}
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM usage_events WHERE event_id='preserved'`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("existing usage changed: count=%d err=%v", count, err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	store, err = OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RotateClientAPIKey(ctx, record.ID, "sha256:rotated", "sk_sx****ab", at); err != nil {
		t.Fatal(err)
	}
	keys, err := store.ListClientAPIKeys(ctx)
	if err != nil || keys[record.ID].Hint != "sk_sx****ab" || keys[record.ID].Hash != "sha256:rotated" {
		t.Fatalf("legacy rotation did not populate hint: %+v err=%v", keys[record.ID], err)
	}
}
