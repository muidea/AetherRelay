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
	if err := accounts.Save("chatgpt.accounts", map[string]string{"account": "saved"}); err != nil {
		t.Fatal(err)
	}
	tasks, err := Open(path, "128MB", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.Save("chatgpt.image_tasks", map[string]string{"task": "saved"}); err != nil {
		t.Fatal(err)
	}
	var account map[string]string
	found, err := tasks.Load("chatgpt.accounts", &account)
	if err != nil || !found || account["account"] != "saved" {
		t.Fatalf("found=%v account=%#v err=%v", found, account, err)
	}
}
