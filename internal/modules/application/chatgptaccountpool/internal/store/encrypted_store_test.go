package store

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	events "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
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

func TestUnchangedAccountSaveDoesNotResealWholeScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	store, err := Open(path, "256MB", 1, 3, encryptedTestCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, err := store.AddOAuth("access", "refresh", "id-token"); err != nil {
		t.Fatal(err)
	}
	before, err := store.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil || len(before) != 1 {
		t.Fatalf("before=%#v err=%v", before, err)
	}
	if err := store.saveLocked(); err != nil {
		t.Fatal(err)
	}
	after, err := store.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil || len(after) != 1 {
		t.Fatalf("after=%#v err=%v", after, err)
	}
	if !bytes.Equal(before[0].Payload, after[0].Payload) || !before[0].UpdatedAt.Equal(after[0].UpdatedAt) {
		t.Fatal("unchanged account was re-encrypted and rewritten")
	}
}

func TestChangingOneAccountDoesNotRewriteOtherAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	store, err := Open(path, "256MB", 1, 3, encryptedTestCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, err := store.AddOAuth("access-first", "refresh-first", "id-first"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddOAuth("access-second", "refresh-second", "id-second"); err != nil {
		t.Fatal(err)
	}
	before, err := store.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil || len(before) != 2 {
		t.Fatalf("before=%#v err=%v", before, err)
	}
	beforePayload := make(map[string][]byte, len(before))
	for _, row := range before {
		beforePayload[row.ID] = append([]byte(nil), row.Payload...)
	}
	store.items["access-first"].LastUsedAt = "2026-09-11T00:00:00Z"
	if err := store.saveLocked(); err != nil {
		t.Fatal(err)
	}
	after, err := store.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil || len(after) != 2 {
		t.Fatalf("after=%#v err=%v", after, err)
	}
	for _, row := range after {
		unchanged := bytes.Equal(beforePayload[row.ID], row.Payload)
		if row.ID == store.items["access-first"].ID && unchanged {
			t.Fatal("changed account was not persisted")
		}
		if row.ID == store.items["access-second"].ID && !unchanged {
			t.Fatal("unrelated account was re-encrypted")
		}
	}
}

func TestFailedAccountSaveDoesNotAdvancePersistedRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	store, err := Open(path, "256MB", 1, 3, encryptedTestCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddOAuth("access", "refresh", "id-token"); err != nil {
		t.Fatal(err)
	}
	account := store.items["access"]
	before := store.persisted[account.ID]
	account.LastUsedAt = "2026-09-11T00:00:00Z"
	if err := store.documents.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.saveLocked(); err == nil {
		t.Fatal("save unexpectedly succeeded after closing state database")
	}
	if after := store.persisted[account.ID]; after != before {
		t.Fatal("failed save advanced the persisted revision")
	}
}

func TestRepeatedCompleteImportPreservesUnchangedAccountState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	store, err := Open(path, "256MB", 1, 3, encryptedTestCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := events.ExportItem{
		CredentialType: "chatgpt_web", Type: "codex", Email: "user@example.invalid",
		AccountID: "account", AccessToken: "access", RefreshToken: "refresh",
		IDToken: "id-token", Password: "password", Proxy: "http://127.0.0.1:8080",
	}
	if added, updated, skipped, err := store.Import(nil, []events.ExportItem{input}, "oauth_import"); err != nil || added != 1 || updated != 0 || skipped != 0 {
		t.Fatalf("first import added=%d updated=%d skipped=%d err=%v", added, updated, skipped, err)
	}
	account := store.items["access"]
	account.ModelSnapshot = &events.AccountModelSnapshot{}
	if err := store.saveLocked(); err != nil {
		t.Fatal(err)
	}
	beforeVersion := store.CatalogVersion()
	before, err := store.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil || len(before) != 1 {
		t.Fatalf("before=%#v err=%v", before, err)
	}
	if added, updated, skipped, err := store.Import(nil, []events.ExportItem{input}, "oauth_import"); err != nil || added != 0 || updated != 0 || skipped != 1 {
		t.Fatalf("repeat import added=%d updated=%d skipped=%d err=%v", added, updated, skipped, err)
	}
	after, err := store.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil || len(after) != 1 {
		t.Fatalf("after=%#v err=%v", after, err)
	}
	if account.ModelSnapshot == nil || store.CatalogVersion() != beforeVersion {
		t.Fatal("unchanged import invalidated account capabilities")
	}
	if !bytes.Equal(before[0].Payload, after[0].Payload) || !before[0].UpdatedAt.Equal(after[0].UpdatedAt) {
		t.Fatal("unchanged complete import rewrote the account")
	}
}
