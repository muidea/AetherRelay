package store

import (
	"reflect"
	"strings"
	"testing"

	"aetherrelay/internal/pkg/aetherrelaystate"
)

func TestNoncurrentConversationsRemainReadableButCannotStartTurns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*aetherrelaystate.TemporaryConversationRow)
	}{
		{"pinned account", func(row *aetherrelaystate.TemporaryConversationRow) { row.AccountID = "account-1" }},
		{"missing provider", func(row *aetherrelaystate.TemporaryConversationRow) { row.Provider = "" }},
		{"upstream conversation", func(row *aetherrelaystate.TemporaryConversationRow) { row.UpstreamConversationID = "upstream-1" }},
		{"parent message", func(row *aetherrelaystate.TemporaryConversationRow) { row.ParentMessageID = "message-1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			created, err := s.CreateConversation("ops", "gpt-5", "", "", "chatgptweb")
			if err != nil {
				t.Fatal(err)
			}
			before, _, err := s.LoadConversationRow("ops", created.ID)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(&before)
			if err := s.docs.UpdateTemporaryConversation(before); err != nil {
				t.Fatal(err)
			}
			if _, err := s.StartTurn("ops", created.ID, "hello"); err == nil || !strings.Contains(err.Error(), "create a new conversation") {
				t.Fatalf("expected explicit rejection: %v", err)
			}
			after, _, err := s.LoadConversationRow("ops", created.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected conversation changed: %v", err)
			}
			detail, err := s.GetConversation("ops", created.ID, nil, 10)
			if err != nil || len(detail.Messages) != 0 {
				t.Fatalf("record unreadable or messages inserted: %+v err=%v", detail, err)
			}
		})
	}
}
