package store

import (
	"path/filepath"
	"testing"

	events "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
)

func TestImageTasksPersistInOwnerTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai-proxy.duckdb")
	s := New(path)
	defer s.Close()
	if _, created, err := s.GetOrCreateGeneration("owner", "task", "prompt", "gpt-image-2", "", "auto"); err != nil || !created {
		t.Fatalf("create created=%v err=%v", created, err)
	}
	s.SetAccountID("owner", "task", "account-hash")
	s.MarkError("owner", "task", "poll timed out", "conversation-1")

	reloaded := New(path)
	defer reloaded.Close()
	resume, ok := reloaded.GetResumeInfo("owner", "task")
	if !ok || resume.AccountID != "account-hash" || resume.Task.ConversationID != "conversation-1" || resume.Task.Status != events.StatusError {
		t.Fatalf("resume=%+v ok=%v", resume, ok)
	}
}

func TestRetryGenerationResetsOnlyPreConversationGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai-proxy.duckdb")
	s := New(path)
	defer s.Close()
	if _, created, err := s.GetOrCreateGeneration("owner", "task", "prompt", "gpt-image-2", "1024x1024", "high"); err != nil || !created {
		t.Fatalf("create created=%v err=%v", created, err)
	}
	s.SetAccountID("owner", "task", "account-hash")
	s.MarkError("owner", "task", "bootstrap: tls: EOF", "")

	view, retried, err := s.RetryGeneration("owner", "task")
	if err != nil || !retried || view.Status != events.StatusQueued || view.Progress != "retrying_submission" || view.Error != "" || view.Prompt != "prompt" || view.Model != "gpt-image-2" {
		t.Fatalf("retry view=%+v retried=%v err=%v", view, retried, err)
	}

	s.MarkError("owner", "task", "poll timed out", "conversation-1")
	if _, retried, err := s.RetryGeneration("owner", "task"); err != nil || retried {
		t.Fatalf("conversation task retried=%v err=%v", retried, err)
	}
}

func TestListUsesStableNewestFirstOrder(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "ai-proxy.duckdb"))
	defer s.Close()
	for _, id := range []string{"task-b", "task-a"} {
		if _, created, err := s.GetOrCreateGeneration("owner", id, "prompt", "gpt-image-2", "", ""); err != nil || !created {
			t.Fatalf("create %s: created=%v err=%v", id, created, err)
		}
	}
	for attempt := 0; attempt < 5; attempt++ {
		items, missing := s.List("owner", nil)
		if len(missing) != 0 || len(items) != 2 || items[0].ID != "task-a" || items[1].ID != "task-b" {
			t.Fatalf("attempt %d items=%+v missing=%v", attempt, items, missing)
		}
	}
}
