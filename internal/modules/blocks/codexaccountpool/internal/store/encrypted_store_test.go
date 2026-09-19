package store

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
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

func TestEncryptedAccountPersistenceKeepsPrivateFingerprintSeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	codec := encryptedTestCodec(t)
	store, err := Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	input := ReadOnlyCredential()
	input.FingerprintMode = events.FingerprintModeScoped
	if _, _, _, err := store.Import([]events.CredentialInput{input}); err != nil {
		t.Fatal(err)
	}
	id := store.order[0]
	seed := store.items[id].FingerprintSeed
	observedUserAgent := "codex-tui/0.155.0 (Ubuntu 24.4.0; x86_64) gnome-terminal (codex-tui; 0.155.0)"
	if !promoteClientIdentityProfile(store.items[id], events.ClientIdentityCandidate{UserAgent: observedUserAgent, Originator: "codex-tui"}) {
		t.Fatal("valid observed identity was not selected")
	}
	if err := store.saveLocked(); err != nil {
		t.Fatal(err)
	}
	if seed == "" {
		t.Fatal("enabled account has no private fingerprint seed")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(observedUserAgent)) {
		t.Fatal("DuckDB file contains plaintext observed client identity")
	}

	restored, err := Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.items[id].FingerprintSeed != seed {
		t.Fatalf("fingerprint seed changed across restart: before=%q after=%q", seed, restored.items[id].FingerprintSeed)
	}
	if profile := restored.items[id].ClientIdentityProfile; profile == nil || profile.UserAgent != observedUserAgent || profile.Version != "0.155.0" {
		t.Fatalf("observed client identity changed across restart: %+v", profile)
	}
}

func TestEncryptedAccountLoadMigratesRemovedFingerprintModeToScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	codec := encryptedTestCodec(t)
	store, err := Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Import([]events.CredentialInput{ReadOnlyCredential()}); err != nil {
		t.Fatal(err)
	}
	id := store.order[0]
	store.items[id].FingerprintMode = "full"
	store.items[id].FingerprintSeed = ""
	if err := store.saveLocked(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := Open(path, "256MB", 1, codec)
	if err != nil {
		t.Fatalf("migrate removed fingerprint mode: %v", err)
	}
	defer restored.Close()
	if restored.items[id].FingerprintMode != events.FingerprintModeScoped || restored.items[id].FingerprintSeed == "" {
		t.Fatalf("removed fingerprint mode was not migrated: %+v", restored.items[id])
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
		Proxy: "http://127.0.0.1:8080", FingerprintMode: events.FingerprintModeScoped,
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

func TestEncryptedAccountLoadRejectsNonFinalCompactProtocol(t *testing.T) {
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
	supported := false
	item.CompactSupported = &supported
	item.CompactProtocol = ""
	if err := store.saveLocked(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path, "256MB", 1, codec); err == nil || !strings.Contains(err.Error(), "does not use the final compact protocol") {
		t.Fatalf("non-final compact protocol error=%v", err)
	}
}

func ReadOnlyCredential() events.CredentialInput {
	return events.CredentialInput{AccessToken: "codex-access-secret", RefreshToken: "codex-refresh-secret", IDToken: "codex-id-secret"}
}
