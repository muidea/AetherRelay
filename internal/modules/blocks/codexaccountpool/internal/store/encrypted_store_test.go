package store

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	events "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"aetherrelay/internal/pkg/aetherrelaycredential"
)

func encryptedTestCodec(t *testing.T) *aetherrelaycredential.Codec {
	t.Helper()
	codec, err := aetherrelaycredential.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func TestOpenRequiresCredentialCodec(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "aetherrelay.duckdb"), "256MB", 1, nil); err == nil {
		t.Fatal("store accepted a missing credential codec")
	}
}

func TestEncryptedAccountPersistenceDoesNotExposeTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	store, err := Open(path, "256MB", 1, encryptedTestCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = store.Import([]events.CredentialInput{ReadOnlyCredential()})
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range [][]byte{[]byte("codex-access-secret"), []byte("codex-refresh-secret"), []byte("codex-id-secret")} {
		if bytes.Contains(raw, value) {
			t.Fatalf("DuckDB file contains plaintext credential %q", value)
		}
	}
	store, err = Open(path, "256MB", 1, encryptedTestCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if len(store.List()) != 1 {
		t.Fatalf("restored accounts=%+v", store.List())
	}
}

func TestEncryptedAccountLoadMigratesFingerprintDefaultAndCompactProtocol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	codec := encryptedTestCodec(t)
	store, err := Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Import([]events.CredentialInput{ReadOnlyCredential()}); err != nil {
		t.Fatal(err)
	}
	item := store.items[store.order[0]]
	legacySupported := false
	item.CompactSupported = &legacySupported
	item.CompactProtocol = ""
	item.FingerprintMode = "legacy-default"
	if err := store.saveLocked(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	view := restored.List()[0]
	if view.FingerprintMode != events.FingerprintModeOff || view.CompactSupported != nil {
		t.Fatalf("migrated view=%+v", view)
	}
	loaded := restored.items[restored.order[0]]
	if loaded.CompactProtocol != nativeCompactProtocol {
		t.Fatalf("compact protocol=%q", loaded.CompactProtocol)
	}
}

func ReadOnlyCredential() events.CredentialInput {
	return events.CredentialInput{AccessToken: "codex-access-secret", RefreshToken: "codex-refresh-secret", IDToken: "codex-id-secret"}
}
