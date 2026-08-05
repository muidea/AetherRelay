package store

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	events "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/events"
	"ai-proxy/internal/pkg/aiproxycredential"
)

func encryptedTestCodec(t *testing.T) *aiproxycredential.Codec {
	t.Helper()
	codec, err := aiproxycredential.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func TestOpenRequiresCredentialCodec(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "ai-proxy.duckdb"), "256MB", 1, nil); err == nil {
		t.Fatal("store accepted a missing credential codec")
	}
}

func TestEncryptedAccountPersistenceDoesNotExposeTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai-proxy.duckdb")
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

func ReadOnlyCredential() events.CredentialInput {
	return events.CredentialInput{AccessToken: "codex-access-secret", RefreshToken: "codex-refresh-secret", IDToken: "codex-id-secret"}
}
