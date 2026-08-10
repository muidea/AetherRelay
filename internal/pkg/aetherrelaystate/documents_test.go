package aetherrelaystate

import (
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
