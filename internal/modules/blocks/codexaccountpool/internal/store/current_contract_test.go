package store

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"aetherrelay/internal/pkg/aetherrelaystate"
)

func TestAccountLoadRejectsNoncurrentFieldsWithoutRewritingDocuments(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*account)
		want   string
	}{
		{"removed mode", func(a *account) { a.FingerprintMode = "full" }, "fingerprint mode"},
		{"missing mode", func(a *account) { a.FingerprintMode = "" }, "fingerprint mode"},
		{"missing seed", func(a *account) { a.FingerprintSeed = "" }, "fingerprint seed"},
		{"missing concurrency", func(a *account) { a.MaxConcurrency = 0 }, "max concurrency"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, codec := filepath.Join(t.TempDir(), "state.duckdb"), encryptedTestCodec(t)
			s, err := Open(path, "128MB", 1, codec)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := s.Import([]events.CredentialInput{ReadOnlyCredential()}); err != nil {
				t.Fatal(err)
			}
			tc.change(s.items[s.order[0]])
			if err := s.saveLocked(); err != nil {
				t.Fatal(err)
			}
			before, err := s.documents.LoadSecureDocuments(secureDocumentScope)
			if err != nil || len(before) != 1 {
				t.Fatalf("rows=%d err=%v", len(before), err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if loaded, err := Open(path, "128MB", 1, codec); err == nil {
				_ = loaded.Close()
				t.Fatal("noncurrent record was accepted")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unexpected rejection: %v", err)
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
			if !bytes.Equal(before[0].Payload, after[0].Payload) || !before[0].UpdatedAt.Equal(after[0].UpdatedAt) || before[0].Position != after[0].Position {
				t.Fatal("rejected record was modified")
			}
		})
	}
}

func TestCurrentAccountRestartPreservesEncryptedDocumentsAndVersions(t *testing.T) {
	path, codec := filepath.Join(t.TempDir(), "state.duckdb"), encryptedTestCodec(t)
	s, err := Open(path, "128MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Import([]events.CredentialInput{ReadOnlyCredential()}); err != nil {
		t.Fatal(err)
	}
	item := s.items[s.order[0]]
	item.ModelSnapshot = &events.AccountModelSnapshot{ClientVersion: "0.162.1"}
	seed, id := item.FingerprintSeed, item.ID
	if !s.promoteSharedClientIdentityLocked(sharedIdentityCandidate("0.162.1")) {
		t.Fatal("observation not promoted")
	}
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	before, err := s.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil || len(before) != 1 {
		t.Fatalf("rows=%d err=%v", len(before), err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, "128MB", 1, codec)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := s.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil || len(after) != 1 {
		t.Fatalf("rows=%d err=%v", len(after), err)
	}
	if !bytes.Equal(before[0].Payload, after[0].Payload) || !before[0].UpdatedAt.Equal(after[0].UpdatedAt) {
		t.Fatal("startup rewrote a current account")
	}
	if s.items[id].ModelSnapshot == nil || s.items[id].ModelSnapshot.ClientVersion != "0.162.1" || s.items[id].FingerprintSeed != seed || s.SharedClientIdentity().Version != "0.162.1" {
		t.Fatal("startup changed current versions or identity")
	}
}
