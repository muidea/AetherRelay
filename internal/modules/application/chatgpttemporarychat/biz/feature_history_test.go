package biz

import (
	"path/filepath"
	"testing"

	"ai-proxy/internal/modules/application/chatgpttemporarychat/internal/store"
	"ai-proxy/internal/pkg/chatattachment"
)

func TestBuildFeatureMessagesSkipsFailedTurnsAndReloadsHistoricalFiles(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state.duckdb"), "64MB", 1, store.Config{RetentionDays: 30, MaxConversations: 10, MaxMessagesPerConversation: 50, MaxMessageBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	conversation, err := s.CreateConversation("ops", "gpt-5", "", "", "chatgptweb", "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.StartTurnWithAttachments("ops", conversation.ID, "read first", nil, []chatattachment.File{{Name: "first.md", ContentType: "text/markdown", Bytes: []byte("# first")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteFeatureTurn("ops", conversation.ID, first.UserSequence, first.AssistantSequence, "first answer", "gpt-5", false, "", ""); err != nil {
		t.Fatal(err)
	}
	failed, err := s.StartTurnWithAttachments("ops", conversation.ID, "read failed", nil, []chatattachment.File{{Name: "failed.md", ContentType: "text/markdown", Bytes: []byte("# failed")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteFeatureTurn("ops", conversation.ID, failed.UserSequence, failed.AssistantSequence, "", "", false, "upstream", "failed"); err != nil {
		t.Fatal(err)
	}
	current, err := s.StartTurnWithAttachments("ops", conversation.ID, "", nil, []chatattachment.File{{Name: "current.md", ContentType: "text/markdown", Bytes: []byte("# current")}})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := s.GetConversation("ops", conversation.ID, nil, 200)
	if err != nil {
		t.Fatal(err)
	}
	temporary := &TemporaryChat{store: s}
	messages, err := temporary.buildFeatureMessages("ops", conversation.ID, detail, current)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages=%+v", messages)
	}
	if messages[0].Content != "read first" || len(messages[0].Files) != 1 || messages[0].Files[0].Name != "first.md" || string(messages[0].Files[0].Bytes) != "# first" {
		t.Fatalf("historical user message=%+v", messages[0])
	}
	if messages[1].Role != "assistant" || messages[1].Content != "first answer" {
		t.Fatalf("historical assistant message=%+v", messages[1])
	}
	if messages[2].Content != "" || len(messages[2].Files) != 1 || messages[2].Files[0].Name != "current.md" {
		t.Fatalf("current user message=%+v", messages[2])
	}
	for _, message := range messages {
		for _, file := range message.Files {
			if file.Name == "failed.md" {
				t.Fatalf("failed turn was replayed: %+v", messages)
			}
		}
	}
}
