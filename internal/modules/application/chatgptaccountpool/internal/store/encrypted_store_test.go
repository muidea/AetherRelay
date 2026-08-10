package store

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"aetherrelay/internal/pkg/aetherrelaycredential"
)

func encryptedTestCodec(t *testing.T) *aetherrelaycredential.Codec {
	t.Helper()
	codec, err := aetherrelaycredential.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func TestOpenRequiresCredentialCodec(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "aetherrelay.duckdb"), "256MB", 1, 3, nil); err == nil {
		t.Fatal("store accepted a missing credential codec")
	}
}

func TestEncryptedAccountPersistenceDoesNotExposeTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	store, err := Open(path, "256MB", 1, 3, encryptedTestCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddOAuth("chatgpt-access-secret", "chatgpt-refresh-secret", "chatgpt-id-secret"); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range [][]byte{[]byte("chatgpt-access-secret"), []byte("chatgpt-refresh-secret"), []byte("chatgpt-id-secret")} {
		if bytes.Contains(raw, value) {
			t.Fatalf("DuckDB file contains plaintext credential %q", value)
		}
	}
	store, err = Open(path, "256MB", 1, 3, encryptedTestCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if len(store.List()) != 1 {
		t.Fatalf("restored accounts=%+v", store.List())
	}
}
