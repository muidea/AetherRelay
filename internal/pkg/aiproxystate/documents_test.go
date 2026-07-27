package aiproxystate

import (
	"path/filepath"
	"testing"
)

func TestDocumentsPersistAcrossOwners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.duckdb")
	accounts, err := Open(path, "128MB", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer accounts.Close()
	if err := accounts.ReplaceAccounts([]AccountRow{{AccessToken: "account", Position: 0, Payload: []byte(`{"id":"saved"}`)}}); err != nil {
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
	rows, err := tasks.LoadAccounts()
	if err != nil || len(rows) != 1 || rows[0].AccessToken != "account" {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
}
