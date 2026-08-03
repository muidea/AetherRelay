package searchhistory

import (
	"path/filepath"
	"testing"
)

func TestStorePersistsOwnerScopedHistoryAndBoundsListProjection(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.duckdb"), "64MB", 1, Config{RetentionDays: 30, MaxItems: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first, err := store.Record(Record{OwnerID: "alice", Model: "gpt-5", ActualModel: "gpt-5-search", Query: "first query", OutputText: "first answer", Provider: "chatgptweb", Sources: []Source{{Title: "First", URL: "https://example.test/first"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Record(Record{OwnerID: "alice", Model: "gpt-5", Query: "second query", OutputText: "second answer", Provider: "chatgptweb"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Record(Record{OwnerID: "bob", Model: "gpt-5", Query: "private query", OutputText: "private answer", Provider: "chatgptweb"}); err != nil {
		t.Fatal(err)
	}

	alice, err := store.List("alice", 50)
	if err != nil || len(alice) != 2 || alice[0].Query != "second query" || alice[0].ID == "" || alice[0].CreatedAt == "" {
		t.Fatalf("alice=%+v err=%v", alice, err)
	}
	if detail, err := store.Get("alice", first.ID); err != nil || detail.OutputText != "first answer" || len(detail.Sources) != 1 || detail.Sources[0].URL != "https://example.test/first" {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	if _, err := store.Get("bob", first.ID); err == nil {
		t.Fatal("expected cross-owner history read to be rejected")
	}
}

func TestStoreEnforcesPerOwnerCapacity(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.duckdb"), "64MB", 1, Config{RetentionDays: 30, MaxItems: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.Record(Record{OwnerID: "admin", Model: "gpt-5", Query: "old", OutputText: "old result", Provider: "chatgptweb"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Record(Record{OwnerID: "admin", Model: "gpt-5", Query: "new", OutputText: "new result", Provider: "chatgptweb"}); err != nil {
		t.Fatal(err)
	}
	items, err := store.List("admin", 50)
	if err != nil || len(items) != 1 || items[0].Query != "new" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if _, err := store.Get("admin", first.ID); err == nil {
		t.Fatal("expected evicted entry to be unavailable")
	}
}
