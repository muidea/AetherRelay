// Package store tests account-pool persistence compatibility.
package store

import (
	"path/filepath"
	"testing"
)

func TestAccountsPersistInStateDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.duckdb")
	s := New(path, 1)
	defer s.Close()
	if _, _, err := s.Add([]string{"token"}, "oauth_login"); err != nil {
		t.Fatal(err)
	}
	reloaded := New(path, 1)
	defer reloaded.Close()
	items := reloaded.List()
	if len(items) != 1 || items[0].SourceType != "oauth_login" {
		t.Fatalf("items=%#v", items)
	}
}
