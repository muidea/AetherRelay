package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	events "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
)

func TestLoadPythonImageTasksFixture(t *testing.T) {
	path := copyWorkspaceFixture(t, "image_tasks.json")
	s := New(path)
	if !s.pythonLayout {
		t.Fatal("expected Python tasks wrapper to be detected")
	}
	if len(s.items) == 0 {
		t.Fatal("expected fixture tasks to load")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !containsTopLevelKey(data, "tasks") {
		t.Fatal("opening a completed Python fixture must not migrate its layout")
	}
	if _, created, err := s.GetOrCreateGeneration("go-owner", "go-task", "prompt", "gpt-image-2", "1024x1024", "standard"); err != nil || !created {
		t.Fatalf("create Go task: created=%v err=%v", created, err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Tasks) != len(s.items) {
		t.Fatalf("Python wrapper lost tasks: got=%d want=%d", len(envelope.Tasks), len(s.items))
	}
}

func TestRecoveryPreservesUnknownPythonTaskFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image_tasks.json")
	data := []byte(`{
  "tasks": [{
    "id": "legacy-task",
    "owner_id": "admin",
    "status": "running",
    "mode": "generate",
    "created_at": "2026-07-24T00:00:00Z",
    "updated_at": "2026-07-24T00:00:00Z",
    "data": [{"url": "https://example.invalid/image.png", "python_data": "keep"}],
    "usage": {"total_tokens": 42, "python_usage": "keep"},
    "updated_ts": 123.45
  }]
}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = New(path) // startup recovery changes status and therefore writes the task.
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Tasks []struct {
			Data      []map[string]json.RawMessage `json:"data"`
			Usage     map[string]json.RawMessage   `json:"usage"`
			UpdatedTS json.RawMessage              `json:"updated_ts"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(saved, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Tasks) != 1 {
		t.Fatalf("tasks=%d", len(envelope.Tasks))
	}
	if _, ok := envelope.Tasks[0].Data[0]["python_data"]; !ok {
		t.Fatalf("unknown image data lost: %s", saved)
	}
	if _, ok := envelope.Tasks[0].Usage["python_usage"]; !ok {
		t.Fatalf("unknown usage lost: %s", saved)
	}
	if len(envelope.Tasks[0].UpdatedTS) == 0 {
		t.Fatalf("unknown task field lost: %s", saved)
	}
}

func TestResumeInfoPersistsAccountReferenceWithoutToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image_tasks.json")
	s := New(path)
	if _, created, err := s.GetOrCreateGeneration("owner", "task", "prompt", "gpt-image-2", "", "auto"); err != nil || !created {
		t.Fatalf("create created=%v err=%v", created, err)
	}
	s.SetAccountID("owner", "task", "account-hash")
	s.MarkError("owner", "task", "poll timed out", "conversation-1")
	reloaded := New(path)
	resume, ok := reloaded.GetResumeInfo("owner", "task")
	if !ok || resume.AccountID != "account-hash" || resume.Task.ConversationID != "conversation-1" || resume.Task.Status != events.StatusError {
		t.Fatalf("resume=%+v ok=%v", resume, ok)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("access_token")) || bytes.Contains(data, []byte("token-a")) {
		t.Fatalf("task persistence leaked a token: %s", data)
	}
}

func copyWorkspaceFixture(t *testing.T, name string) string {
	t.Helper()
	source := filepath.Clean(filepath.Join("../../../../../../../chatgpt2api/data", name))
	data, err := os.ReadFile(source)
	if err != nil {
		t.Skipf("workspace compatibility fixture unavailable: %v", err)
	}
	target := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return target
}

func containsTopLevelKey(data []byte, key string) bool {
	return len(data) > 0 && data[0] == '{' && bytes.Contains(data, []byte(`"`+key+`"`))
}
