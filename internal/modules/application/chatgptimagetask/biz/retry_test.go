package biz

import (
	"testing"

	events "aetherrelay/internal/modules/application/chatgptimagetask/pkg/events"
)

func TestRetryableBootstrapFailure(t *testing.T) {
	if !isRetryableBootstrapFailure(events.TaskView{Status: events.StatusError, Mode: "generate", Error: `code:6, message:bootstrap: tls: Get "https://chatgpt.com/": EOF`}) {
		t.Fatal("bootstrap TLS failure should be retryable")
	}
	for _, task := range []events.TaskView{
		{Status: events.StatusError, Mode: "generate", Error: "bootstrap: HTTP 403 (invalid_token)"},
		{Status: events.StatusError, Mode: "generate", Error: "bootstrap: timeout: deadline", ConversationID: "conversation-1"},
		{Status: events.StatusError, Mode: "edit", Error: "bootstrap: tls: EOF"},
		{Status: events.StatusSuccess, Mode: "generate", Error: "bootstrap: tls: EOF"},
	} {
		if isRetryableBootstrapFailure(task) {
			t.Fatalf("unexpected retryable task: %+v", task)
		}
	}
}
