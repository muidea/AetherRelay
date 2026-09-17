package aetherrelaystate

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenRejectsIncompleteSchemaWithoutResettingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incomplete.duckdb")
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE chatgpt_images (
path VARCHAR PRIMARY KEY,
size BIGINT NOT NULL,
width INTEGER NOT NULL,
height INTEGER NOT NULL,
created_at VARCHAR NOT NULL,
payload JSON NOT NULL,
updated_at TIMESTAMP NOT NULL DEFAULT current_timestamp
)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO chatgpt_images(path, size, width, height, created_at, payload)
VALUES ('historic.png', 1, 1, 1, '2026-09-15T00:00:00Z', '{}')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path, "128MB", 1); err == nil || !strings.Contains(err.Error(), "does not match the final schema") {
		t.Fatalf("incomplete schema error=%v", err)
	}

	db, err = sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM chatgpt_images WHERE path='historic.png'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("incompatible state database was modified: count=%d err=%v", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_name IN (
'secure_documents', 'chatgpt_image_tasks', 'chatgpt_images', 'chatgpt_image_tags',
'chatgpt_temporary_conversations', 'chatgpt_temporary_messages',
'chatgpt_temporary_message_images', 'chatgpt_temporary_message_attachments',
'chatgpt_web_search_history')`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("incompatible state database gained final tables: count=%d err=%v", count, err)
	}
}

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
