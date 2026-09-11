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

func TestUnchangedAccountSaveDoesNotResealWholeScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	store, err := Open(path, "256MB", 1, encryptedTestCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, _, err := store.Import([]events.CredentialInput{ReadOnlyCredential()}); err != nil {
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
	store, err := Open(path, "256MB", 1, encryptedTestCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	inputs := []events.CredentialInput{
		{AccessToken: "access-first", RefreshToken: "refresh-first"},
		{AccessToken: "access-second", RefreshToken: "refresh-second"},
	}
	if _, _, _, err := store.Import(inputs); err != nil {
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
	changedID := store.order[0]
	store.items[changedID].LastUsedAt = "2026-09-11T00:00:00Z"
	if err := store.saveLocked(); err != nil {
		t.Fatal(err)
	}
	after, err := store.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil || len(after) != 2 {
		t.Fatalf("after=%#v err=%v", after, err)
	}
	for _, row := range after {
		unchanged := bytes.Equal(beforePayload[row.ID], row.Payload)
		if row.ID == changedID && unchanged {
			t.Fatal("changed account was not persisted")
		}
		if row.ID != changedID && !unchanged {
			t.Fatal("unrelated account was re-encrypted")
		}
	}
}

func TestFailedAccountSaveDoesNotAdvancePersistedRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	store, err := Open(path, "256MB", 1, encryptedTestCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Import([]events.CredentialInput{ReadOnlyCredential()}); err != nil {
		t.Fatal(err)
	}
	id := store.order[0]
	before := store.persisted[id]
	store.items[id].LastUsedAt = "2026-09-11T00:00:00Z"
	if err := store.documents.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.saveLocked(); err == nil {
		t.Fatal("save unexpectedly succeeded after closing state database")
	}
	if after := store.persisted[id]; after != before {
		t.Fatal("failed save advanced the persisted revision")
	}
}

func TestRepeatedCompleteImportPreservesUnchangedAccountState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	store, err := Open(path, "256MB", 1, encryptedTestCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := events.CredentialInput{
		CredentialType: "codex_cli", AccountID: "account", Email: "user@example.invalid",
		AccessToken: "access", RefreshToken: "refresh", IDToken: "id-token",
		Proxy: "http://127.0.0.1:8080", FingerprintMode: events.FingerprintModeSession,
	}
	if added, updated, skipped, err := store.Import([]events.CredentialInput{input}); err != nil || added != 1 || updated != 0 || skipped != 0 {
		t.Fatalf("first import added=%d updated=%d skipped=%d err=%v", added, updated, skipped, err)
	}
	id := store.order[0]
	supported := true
	store.items[id].ModelSnapshot = &events.AccountModelSnapshot{}
	store.items[id].UsageSnapshot = &events.AccountUsageSnapshot{}
	store.items[id].CompactSupported = &supported
	if err := store.saveLocked(); err != nil {
		t.Fatal(err)
	}
	beforeVersion := store.catalogVersion
	before, err := store.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil || len(before) != 1 {
		t.Fatalf("before=%#v err=%v", before, err)
	}
	if added, updated, skipped, ids, err := store.ImportWithIDs([]events.CredentialInput{input}); err != nil || added != 0 || updated != 0 || skipped != 1 || len(ids) != 0 {
		t.Fatalf("repeat import added=%d updated=%d skipped=%d ids=%v err=%v", added, updated, skipped, ids, err)
	}
	after, err := store.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil || len(after) != 1 {
		t.Fatalf("after=%#v err=%v", after, err)
	}
	item := store.items[id]
	if item.ModelSnapshot == nil || item.UsageSnapshot == nil || item.CompactSupported == nil || !*item.CompactSupported || store.catalogVersion != beforeVersion {
		t.Fatal("unchanged import invalidated account state")
	}
	if !bytes.Equal(before[0].Payload, after[0].Payload) || !before[0].UpdatedAt.Equal(after[0].UpdatedAt) {
		t.Fatal("unchanged complete import rewrote the account")
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
