// Package store tests account-pool persistence compatibility.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPythonAccountsFixturePreservesUnknownFieldsOnWrite(t *testing.T) {
	source := filepath.Join("testdata", "accounts_compat.json")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(path, 1)
	if len(s.order) == 0 {
		t.Fatal("expected Python accounts to load")
	}
	token := s.order[0]
	quota := s.items[token].Quota
	if _, ok, err := s.Update(token, "", "", &quota, s.items[token].Proxy); err != nil || !ok {
		t.Fatalf("write account fixture: ok=%v err=%v", ok, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var accounts []map[string]any
	if err := json.Unmarshal(after, &accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != len(s.order) {
		t.Fatalf("account count changed: got=%d want=%d", len(accounts), len(s.order))
	}
	if _, ok := accounts[0]["id_token"]; !ok {
		t.Fatal("Python-owned id_token was dropped during Go write")
	}
}
