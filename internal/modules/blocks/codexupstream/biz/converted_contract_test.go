package biz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
)

func TestConvertedRequestNativeCodexWireContract(t *testing.T) {
	for _, mode := range []string{"off", "scoped"} {
		t.Run(mode, func(t *testing.T) {
			profile := codexRequestProfile{sessionHash: "local-session", turnMetadata: events.TurnMetadata{TurnID: "turn", TurnStartedAtMS: 123}}
			if mode == "scoped" {
				profile.fingerprint = events.CodexFingerprint{Mode: "scoped", InstallationID: "installation", SessionID: "session", ThreadID: "thread", WindowID: "thread:0"}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, key := range []string{"Authorization", "Content-Type", "Accept", "User-Agent", "Originator", "Session-Id", "Thread-Id", "X-Client-Request-Id", "X-Codex-Window-Id", "X-Codex-Turn-Metadata", "X-Codex-Beta-Features", "ChatGPT-Account-ID"} {
					if r.Header.Get(key) == "" {
						t.Errorf("missing Codex header %s", key)
					}
				}
				if r.Header.Get("User-Agent") != currentIdentity.UserAgent || r.Header.Get("Originator") != currentIdentity.Originator {
					t.Error("missing native identity fallback")
				}
				if mode == "scoped" && r.Header.Get("X-Codex-Installation-Id") == "" {
					t.Error("missing scoped installation")
				}
				for _, key := range []string{"Anthropic-Version", "Anthropic-Beta", "X-Stainless-Runtime", "X-Claude-Code-Session-Id"} {
					if r.Header.Get(key) != "" {
						t.Errorf("source header leaked: %s", key)
					}
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				metadata, ok := body["client_metadata"].(map[string]any)
				if !ok || metadata["session_id"] != r.Header.Get("Session-Id") || metadata["thread_id"] != r.Header.Get("Thread-Id") || metadata["turn_id"] != "turn" {
					t.Error("body/header identity mismatch")
				}
				w.Header().Set("Content-Type", "text/event-stream")
			}))
			defer server.Close()
			response, _, _, _, err := performURL(context.Background(), server.URL, "text/event-stream", "test-token", "test-account", "", []byte(`{"model":"test","input":[],"instructions":"","stream":true,"store":false}`), profile)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
		})
	}
}
