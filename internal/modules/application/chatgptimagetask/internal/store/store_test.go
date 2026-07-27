package store

import (
	"path/filepath"
	"testing"

	events "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
)

func TestImageTasksPersistInOwnerTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.duckdb")
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
