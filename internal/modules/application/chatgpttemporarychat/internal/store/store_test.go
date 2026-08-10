package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/chatgpttemporarychat/pkg/common"
	events "aetherrelay/internal/modules/application/chatgpttemporarychat/pkg/events"
	"aetherrelay/internal/pkg/chatattachment"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	s, err := Open(path, "64MB", 1, Config{
		RetentionDays:              30,
		MaxConversations:           2,
		MaxMessagesPerConversation: 6,
		MaxMessageBytes:            1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStartTurnPersistsAndOwnerScopesFileAttachments(t *testing.T) {
	s := openTestStore(t)
	created, err := s.CreateConversation("admin", "gpt-5", "", "", "chatgptweb", "")
	if err != nil {
		t.Fatal(err)
	}
	started, err := s.StartTurnWithAttachments("admin", created.ID, "", nil, []chatattachment.File{{Name: "notes.md", ContentType: "text/markdown", Bytes: []byte("# hello")}})
	if err != nil {
		t.Fatal(err)
	}
	if started.Conversation.Title != "附件对话" || len(started.Files) != 1 || len(started.UserMessage.Attachments) != 1 {
		t.Fatalf("started=%+v", started)
	}
	attachmentID := started.UserMessage.Attachments[0].ID
	stored, found, err := s.GetMessageAttachment("admin", created.ID, started.UserMessage.ID, attachmentID)
	if err != nil || !found || stored.FileName != "notes.md" || stored.ContentType != "text/markdown" || string(stored.Bytes) != "# hello" {
		t.Fatalf("attachment=%+v found=%v err=%v", stored, found, err)
	}
	if _, found, err := s.GetMessageAttachment("other-admin", created.ID, started.UserMessage.ID, attachmentID); err != nil || found {
		t.Fatalf("cross-owner attachment access found=%v err=%v", found, err)
	}
	if err := s.DeleteConversation("admin", created.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.GetMessageAttachment("admin", created.ID, started.UserMessage.ID, attachmentID); err != nil || found {
		t.Fatalf("deleted attachment found=%v err=%v", found, err)
	}
}

func TestCreateListGetAndDeleteConversation(t *testing.T) {
	s := openTestStore(t)
	created, err := s.CreateConversation("admin", "gpt-5", "medium", "be concise", "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Status != common.StatusIdle || created.AccountID != "" || created.AccountDisplay == "" {
		t.Fatalf("created=%+v", created)
	}
	list, err := s.ListConversations("admin", "", 10)
	if err != nil || len(list.Items) != 1 || list.Items[0].ID != created.ID {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	detail, err := s.GetConversation("admin", created.ID, nil, 10)
	if err != nil || detail.Conversation.ID != created.ID || len(detail.Messages) != 0 {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	if err := s.DeleteConversation("admin", created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConversation("admin", created.ID, nil, 10); err == nil {
		t.Fatal("expected deleted conversation to be unreadable")
	}
}

func TestStartTurnAndCompleteTurnPersistAnchors(t *testing.T) {
	s := openTestStore(t)
	created, err := s.CreateConversation("admin", "gpt-5", "", "", "account-1")
	if err != nil {
		t.Fatal(err)
	}
	started, err := s.StartTurn("admin", created.ID, "hello world from temporary chat")
	if err != nil {
		t.Fatal(err)
	}
	if started.Conversation.Status != common.StatusStreaming || started.TurnID == "" {
		t.Fatalf("started=%+v", started)
	}
	if started.UserMessage.Role != common.RoleUser || started.AssistantMessage.Role != common.RoleAssistant {
		t.Fatalf("messages=%+v / %+v", started.UserMessage, started.AssistantMessage)
	}
	if !strings.Contains(started.Conversation.Title, "hello world") {
		t.Fatalf("title should come from first user message: %q", started.Conversation.Title)
	}
	if _, err := s.StartTurn("admin", created.ID, "second while streaming"); err == nil {
		t.Fatal("concurrent start must fail while streaming")
	}
	completed, err := s.CompleteTurn("admin", created.ID, started.UserSequence, started.AssistantSequence, "assistant reply", "gpt-5-5", "upstream-conv-1", "assistant-msg-1", false, false, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Conversation.Status != common.StatusIdle || completed.Message.Content != "assistant reply" {
		t.Fatalf("completed=%+v", completed)
	}
	if completed.Conversation.ActualModel != "gpt-5-5" || completed.Message.ActualModel != "gpt-5-5" {
		t.Fatalf("actual model was not projected: %+v", completed)
	}
	row, found, err := s.LoadConversationRow("admin", created.ID)
	if err != nil || !found {
		t.Fatalf("load row found=%v err=%v", found, err)
	}
	if row.UpstreamConversationID != "upstream-conv-1" || row.ParentMessageID != "assistant-msg-1" {
		t.Fatalf("anchors not persisted: %+v", row)
	}
	second, err := s.StartTurn("admin", created.ID, "follow up")
	if err != nil {
		t.Fatal(err)
	}
	if second.UpstreamConversationID != "upstream-conv-1" || second.ParentMessageID != "assistant-msg-1" {
		t.Fatalf("second turn anchors=%+v", second)
	}
}

func TestStartTurnPersistsAndOwnerScopesImageAttachments(t *testing.T) {
	s := openTestStore(t)
	created, err := s.CreateConversation("admin", "gpt-5", "", "", "account-1")
	if err != nil {
		t.Fatal(err)
	}
	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0xf0, 0x1f, 0x00, 0x05, 0x80, 0x02, 0x3f, 0x91, 0xc3, 0xf3, 0xe1, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}
	started, err := s.StartTurn("admin", created.ID, "", []events.ImageInput{{Bytes: pngBytes, ContentType: "image/png"}})
	if err != nil {
		t.Fatal(err)
	}
	if started.Conversation.Title != "图片对话" || len(started.Images) != 1 || len(started.UserMessage.Images) != 1 {
		t.Fatalf("started=%+v", started)
	}
	imageID := started.UserMessage.Images[0].ID
	stored, found, err := s.GetMessageImage("admin", created.ID, started.UserMessage.ID, imageID)
	if err != nil || !found || stored.ContentType != "image/png" || string(stored.Bytes) != string(pngBytes) {
		t.Fatalf("image=%+v found=%v err=%v", stored, found, err)
	}
	if _, found, err := s.GetMessageImage("other-admin", created.ID, started.UserMessage.ID, imageID); err != nil || found {
		t.Fatalf("cross-owner image access found=%v err=%v", found, err)
	}
	if err := s.DeleteConversation("admin", created.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.GetMessageImage("admin", created.ID, started.UserMessage.ID, imageID); err != nil || found {
		t.Fatalf("deleted image found=%v err=%v", found, err)
	}
}

func TestInterruptStreamingMarksRecoveryRequired(t *testing.T) {
	s := openTestStore(t)
	created, err := s.CreateConversation("admin", "gpt-5", "", "", "account-1")
	if err != nil {
		t.Fatal(err)
	}
	started, err := s.StartTurn("admin", created.ID, "please answer")
	if err != nil {
		t.Fatal(err)
	}
	count, err := s.InterruptStreaming()
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	detail, err := s.GetConversation("admin", created.ID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Conversation.Status != common.StatusRecoveryRequired {
		t.Fatalf("status=%s", detail.Conversation.Status)
	}
	for _, message := range detail.Messages {
		if message.Status != common.MessageStatusInterrupted {
			t.Fatalf("message=%+v", message)
		}
	}
	if _, err := s.StartTurn("admin", created.ID, "retry"); err == nil {
		t.Fatal("recovery_required must reject new turns")
	}
	_ = started
}

func TestUncertainUpstreamFailureRequiresRecovery(t *testing.T) {
	s := openTestStore(t)
	created, err := s.CreateConversation("admin", "gpt-5", "", "", "account-1")
	if err != nil {
		t.Fatal(err)
	}
	started, err := s.StartTurn("admin", created.ID, "please answer")
	if err != nil {
		t.Fatal(err)
	}
	completed, err := s.CompleteTurn("admin", created.ID, started.UserSequence, started.AssistantSequence, "partial", "", "", "", false, true, true, "timeout", "upstream request timed out")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Conversation.Status != common.StatusRecoveryRequired || completed.Message.Status != common.MessageStatusInterrupted {
		t.Fatalf("completed=%+v", completed)
	}
	if _, err := s.StartTurn("admin", created.ID, "continue"); err == nil {
		t.Fatal("uncertain upstream turn must not be continued")
	}
}

func TestExpiredConversationCannotBeReadOrListed(t *testing.T) {
	s := openTestStore(t)
	created, err := s.CreateConversation("admin", "gpt-5", "", "", "account-1")
	if err != nil {
		t.Fatal(err)
	}
	row, found, err := s.LoadConversationRow("admin", created.ID)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	row.ExpiresAt = time.Now().UTC().Add(-time.Second)
	if err := s.docs.UpdateTemporaryConversation(row); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConversation("admin", created.ID, nil, 10); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired read rejection, err=%v", err)
	}
	list, err := s.ListConversations("admin", "", 10)
	if err != nil || len(list.Items) != 0 {
		t.Fatalf("expired conversation must not be listed: %+v err=%v", list, err)
	}
}

func TestSystemPromptUsesMessageBound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateConversation("admin", "gpt-5", "", strings.Repeat("x", 1025), "account-1"); err == nil {
		t.Fatal("expected oversized system prompt to be rejected")
	}
}

func TestConversationLimitAndPurgeExpired(t *testing.T) {
	s := openTestStore(t)
	first, err := s.CreateConversation("admin", "gpt-5", "", "", "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateConversation("admin", "gpt-5", "", "", "account-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateConversation("admin", "gpt-5", "", "", "account-1"); err == nil {
		t.Fatal("max conversations must reject silent growth")
	}
	row, found, err := s.LoadConversationRow("admin", first.ID)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	row.ExpiresAt = time.Now().UTC().Add(-time.Hour)
	row.Status = common.StatusIdle
	if err := s.docs.UpdateTemporaryConversation(row); err != nil {
		t.Fatal(err)
	}
	purged, err := s.PurgeExpired(time.Now().UTC())
	if err != nil || purged != 1 {
		t.Fatalf("purged=%d err=%v", purged, err)
	}
	if _, err := s.GetConversation("admin", first.ID, nil, 10); err == nil {
		t.Fatal("purged conversation must be gone")
	}
}
