package usage

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
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

func TestFinalSchemaRejectsMissingClientAPIKeyHintWithoutChangingData(t *testing.T) {
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
		if reopened, err := OpenDuckDB(cfg); err == nil {
			_ = reopened.Close()
			t.Fatal("missing hint column must reject startup")
		} else if !strings.Contains(err.Error(), "does not match the final schema") {
			t.Fatalf("unexpected schema error: %v", err)
		}
	}
	db, err := sql.Open("duckdb", cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var hash, mode string
	var enabled bool
	var createdAt time.Time
	if err := db.QueryRowContext(ctx, `SELECT key_hash,enabled,created_at,provider_access_mode FROM client_api_key_metadata WHERE api_key_id=?`, record.ID).Scan(&hash, &enabled, &createdAt, &mode); err != nil {
		t.Fatal(err)
	}
	if hash != record.Hash || enabled != record.Enabled || !createdAt.Equal(at) || mode != string(policy.Mode) {
		t.Fatalf("existing key changed: hash=%q enabled=%v created_at=%v mode=%q", hash, enabled, createdAt, mode)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM client_api_key_provider_access WHERE api_key_id=? AND provider_id='provider'`, record.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("existing provider access changed: count=%d err=%v", count, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM client_api_key_provider_access WHERE api_key_id=?`, record.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("provider access gained rows: count=%d err=%v", count, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM usage_events WHERE event_id='preserved' AND api_key_id=? AND state='started' AND started_at=?`, record.ID, at).Scan(&count); err != nil || count != 1 {
		t.Fatalf("existing usage changed: count=%d err=%v", count, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='client_api_key_metadata' AND column_name='hint'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("missing hint column was added: count=%d err=%v", count, err)
	}
}

func TestFinalSchemaPreservesNullClientAPIKeyHintUntilRotation(t *testing.T) {
	ctx := context.Background()
	cfg := testCfg(filepath.Join(t.TempDir(), "null-hint.duckdb"))
	store, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	record := ClientAPIKeyRecord{ID: "existing", Hash: "sha256:existing", Enabled: true, CreatedAt: at, ProviderAccess: clientaccess.All()}
	if err := store.CreateClientAPIKey(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE client_api_key_metadata SET hint=NULL WHERE api_key_id=?`, record.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	keys, err := store.ListClientAPIKeys(ctx)
	if err != nil || keys[record.ID].Hint != "" || keys[record.ID].Hash != record.Hash {
		t.Fatalf("NULL hint record changed: %+v err=%v", keys[record.ID], err)
	}
	var hint sql.NullString
	if err := store.db.QueryRowContext(ctx, `SELECT hint FROM client_api_key_metadata WHERE api_key_id=?`, record.ID).Scan(&hint); err != nil || hint.Valid {
		t.Fatalf("NULL hint was rewritten at startup: %+v err=%v", hint, err)
	}
	if err := store.RotateClientAPIKey(ctx, record.ID, "sha256:rotated", "sk_sx****ab", at); err != nil {
		t.Fatal(err)
	}
	keys, err = store.ListClientAPIKeys(ctx)
	if err != nil || keys[record.ID].Hint != "sk_sx****ab" || keys[record.ID].Hash != "sha256:rotated" {
		t.Fatalf("rotation did not update credential pair: %+v err=%v", keys[record.ID], err)
	}
}
