package aetherrelaystate

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestDocumentsPersistAcrossOwners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	accounts, err := Open(path, "128MB", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer accounts.Close()
	if err := accounts.ReplaceSecureDocuments("accounts", []SecureDocumentRow{{ID: "account", Position: 0, Payload: []byte("sealed")}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := Open(path, "128MB", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer tasks.Close()
	if err := tasks.ReplaceImageTasks([]ImageTaskRow{{OwnerID: "owner", TaskID: "task", Payload: []byte(`{"id":"task"}`)}}); err != nil {
		t.Fatal(err)
	}
	rows, err := tasks.LoadSecureDocuments("accounts")
	if err != nil || len(rows) != 1 || rows[0].ID != "account" {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
}

func TestApplySecureDocumentsDoesNotRetainEveryBlobVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	documents, err := Open(path, "128MB", 1)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 8*1024)
	for index := 0; index < 1000; index++ {
		if _, err := rand.Read(payload); err != nil {
			t.Fatal(err)
		}
		if err := documents.ApplySecureDocuments("accounts", []SecureDocumentRow{{
			ID: "account", Payload: append([]byte(nil), payload...),
		}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := documents.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 1000 historical payloads contain 8 MiB of incompressible bytes. A
	// bounded file proves checkpoints retained the current row rather than
	// every encrypted version that was superseded by UPSERT.
	if info.Size() > 4*1024*1024 {
		t.Fatalf("incremental secure document updates retained historical blobs: %d bytes", info.Size())
	}
}

func TestApplySecureDocumentsUpdatesOnlyExplicitDelta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	documents, err := Open(path, "128MB", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer documents.Close()
	if err := documents.ApplySecureDocuments("accounts", []SecureDocumentRow{
		{ID: "first", Position: 0, Payload: []byte("sealed-first")},
		{ID: "second", Position: 1, Payload: []byte("sealed-second")},
	}, nil); err != nil {
		t.Fatal(err)
	}
	before, err := documents.LoadSecureDocuments("accounts")
	if err != nil || len(before) != 2 {
		t.Fatalf("before=%#v err=%v", before, err)
	}
	if err := documents.ApplySecureDocuments("accounts", []SecureDocumentRow{
		{ID: "first", Position: 2, Payload: []byte("sealed-first-updated")},
	}, []string{"second"}); err != nil {
		t.Fatal(err)
	}
	after, err := documents.LoadSecureDocuments("accounts")
	if err != nil || len(after) != 1 {
		t.Fatalf("after=%#v err=%v", after, err)
	}
	if after[0].ID != "first" || after[0].Position != 2 || !bytes.Equal(after[0].Payload, []byte("sealed-first-updated")) {
		t.Fatalf("unexpected delta result: %#v", after[0])
	}
	if err := documents.ApplySecureDocuments("accounts",
		[]SecureDocumentRow{{ID: "same", Payload: []byte("value")}}, []string{"same"}); err == nil {
		t.Fatal("accepted one document in both update and delete sets")
	}
}
