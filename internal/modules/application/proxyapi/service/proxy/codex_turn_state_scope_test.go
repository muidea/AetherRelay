package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

// CP-HDR-022: the protocol adapters reach the same Codex executor, so an inbound
// turn state must reach it unchanged instead of being replaced by the fallback.
func TestCodexTurnStateAdaptersKeepClientValue(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			body: `{"model":"gpt-5.2-codex","messages":[{"role":"user","content":"hello"}]}`,
		},
		{
			name: "anthropic messages",
			path: "/v1/messages",
			body: `{"model":"gpt-5.2-codex","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var received codexresponses.Request
			handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{complete: func(_ context.Context, request codexresponses.Request) (codexresponses.Result, error) {
				received = request
				return codexresponses.Result{Body: []byte(`{"id":"resp_adapter","model":"gpt-5.2-codex","status":"completed","output":[],"usage":{"input_tokens":4,"output_tokens":2}}`)}, nil
			}})
			request := httptest.NewRequest(http.MethodPost, testCase.path, bytes.NewBufferString(testCase.body))
			request.Header.Set("Authorization", "Bearer test-client-key")
			request.Header.Set(codexTurnStateHeader, "state-from-client")
			request.Header.Set("Session-Id", "adapter-conversation")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("CP-EP-007..008 status=%d body=%s", response.Code, response.Body.String())
			}
			if received.TurnState != "state-from-client" {
				t.Fatalf("CP-HDR-022 adapter turn state=%q", received.TurnState)
			}
			if received.SessionScope == "" {
				t.Fatal("CP-HDR-022 adapter request declared no record unit")
			}
		})
	}
}

// CP-HDR-003/004: the inference path carries the downstream client's bounded
// identity all the way to the executor, on the native and adapter entrypoints.
func TestCodexClientIdentityReachesExecutor(t *testing.T) {
	const clientAgent = "codex-tui/0.154.0 (Ubuntu 24.4.0; x86_64) WindowsTerminal (codex-tui; 0.154.0)"
	for name, testCase := range map[string]struct {
		path         string
		body         string
		userAgent    string
		originator   string
		wantAgent    string
		wantOriginat string
	}{
		"native responses": {
			path: "/v1/responses", body: `{"model":"gpt-5.2-codex","input":"hello"}`,
			userAgent: clientAgent, originator: "codex-tui", wantAgent: clientAgent, wantOriginat: "codex-tui",
		},
		"absent identity": {
			path: "/v1/responses", body: `{"model":"gpt-5.2-codex","input":"hello"}`,
			wantAgent: "", wantOriginat: "",
		},
		"oversized identity": {
			path: "/v1/responses", body: `{"model":"gpt-5.2-codex","input":"hello"}`,
			userAgent: strings.Repeat("x", codexClientUserAgentLimit+1), originator: strings.Repeat("y", codexClientOriginatorLimit+1),
			wantAgent: "", wantOriginat: "",
		},
		"chat adapter": {
			path: "/v1/chat/completions", body: `{"model":"gpt-5.2-codex","messages":[{"role":"user","content":"hello"}]}`,
			userAgent: clientAgent, originator: "codex-tui", wantAgent: clientAgent, wantOriginat: "codex-tui",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var received codexresponses.Request
			handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{complete: func(_ context.Context, request codexresponses.Request) (codexresponses.Result, error) {
				received = request
				return codexresponses.Result{Body: []byte(`{"id":"resp_identity","model":"gpt-5.2-codex","status":"completed","output":[],"usage":{"input_tokens":4,"output_tokens":2}}`)}, nil
			}})
			request := httptest.NewRequest(http.MethodPost, testCase.path, bytes.NewBufferString(testCase.body))
			request.Header.Set("Authorization", "Bearer test-client-key")
			if testCase.userAgent != "" {
				request.Header.Set("User-Agent", testCase.userAgent)
			}
			if testCase.originator != "" {
				request.Header.Set("Originator", testCase.originator)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if received.ClientUserAgent != testCase.wantAgent || received.ClientOriginator != testCase.wantOriginat {
				t.Fatalf("CP-HDR-003/004 user-agent=%q originator=%q", received.ClientUserAgent, received.ClientOriginator)
			}
		})
	}
}

// CP-HDR-012/CP-HDR-022: an oversized adapter turn state is rejected before an
// account is selected, exactly like on the native entrypoint.
func TestCodexTurnStateAdaptersRejectInvalidValue(t *testing.T) {
	handler := newCodexResponsesHandler(t, usage.NewMemoryStore(), codexResponsesExecutorStub{})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-5.2-codex","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	request.Header.Set(codexTurnStateHeader, strings.Repeat("x", codexTurnStateLimit+1))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("CP-HDR-012 status=%d body=%s", response.Code, response.Body.String())
	}
}

// CP-HDR-022: the record unit is the conversation the client declared, so two
// conversations of the same client credential and model never share a turn
// state, and a client that declares none gets no record unit at all.
func TestCodexTurnStateScopeDigestFollowsDeclaredConversation(t *testing.T) {
	session := func(sessionID, threadID string) map[string]any {
		metadata := map[string]any{}
		if sessionID != "" {
			metadata["session_id"] = sessionID
		}
		if threadID != "" {
			metadata["thread_id"] = threadID
		}
		return map[string]any{"client_metadata": metadata}
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	first := codexTurnStateScopeDigest(request, "gpt-5.2-codex", session("conversation-a", "thread-a"))
	second := codexTurnStateScopeDigest(request, "gpt-5.2-codex", session("conversation-b", "thread-a"))
	if first == "" || second == "" || first == second {
		t.Fatalf("CP-HDR-022 declared conversations collided: %q %q", first, second)
	}
	// Another thread of the same conversation is a different record unit.
	if thread := codexTurnStateScopeDigest(request, "gpt-5.2-codex", session("conversation-a", "thread-b")); thread == first {
		t.Fatalf("CP-HDR-022 threads collided: %q", thread)
	}
	// The field name is part of the identity: a session-only declaration must
	// not collide with a thread-only declaration that happens to use the same
	// opaque value.
	sessionOnly := codexTurnStateScopeDigest(request, "gpt-5.2-codex", session("shared-value", ""))
	threadOnly := codexTurnStateScopeDigest(request, "gpt-5.2-codex", session("", "shared-value"))
	if sessionOnly == "" || threadOnly == "" || sessionOnly == threadOnly {
		t.Fatalf("CP-HDR-022 session/thread identities collided: %q %q", sessionOnly, threadOnly)
	}
	// Another model is not the same unit either.
	if other := codexTurnStateScopeDigest(request, "gpt-5.5", session("conversation-a", "thread-a")); other == first {
		t.Fatalf("CP-HDR-022 models collided: %q", other)
	}
	// A declared session header also names the unit, so a client that never fills
	// client_metadata still gets one. The scheduling digest substitutes a shared
	// synthetic signal here, which must never become a turn state bucket.
	headerRequest := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	headerRequest.Header.Set("session_id", "conversation-a")
	if header := codexTurnStateScopeDigest(headerRequest, "gpt-5.2-codex", nil); header == "" {
		t.Fatal("CP-HDR-022 session header did not name a record unit")
	}
}

// CP-HDR-022: requests that declare no conversation are not a shared bucket, even
// though the scheduling session hash substitutes a synthetic signal for them.
func TestCodexTurnStateScopeDigestRejectsUndeclaredConversation(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := map[string]any{"input": "hello"}
	if scope := codexTurnStateScopeDigest(request, "gpt-5.2-codex", body); scope != "" {
		t.Fatalf("CP-HDR-022 stateless scope=%q", scope)
	}
	if scope := codexTurnStateScopeDigest(request, "gpt-5.2-codex", nil); scope != "" {
		t.Fatalf("CP-HDR-022 nil body scope=%q", scope)
	}
	if scope := codexTurnStateScopeDigest(nil, "gpt-5.2-codex", body); scope != "" {
		t.Fatalf("CP-HDR-022 nil request scope=%q", scope)
	}
	// The scheduling digest still produces a stable shared session for the same
	// request, which is what account stickiness relies on.
	if session := codexSessionHash(request, "gpt-5.2-codex", body); session == "" {
		t.Fatal("CP-SCHED-002 scheduling session must stay non-empty")
	}
}

// CP-HDR-022: the declared conversation outranks the shared scheduling signal, so
// a client that declares one keeps its own record unit even without a session
// header.
func TestCodexTurnStateScopeDigestPrefersDeclaredConversation(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("Session-Id", "header-session")
	withMetadata := codexTurnStateScopeDigest(request, "gpt-5.2-codex", map[string]any{
		"client_metadata": map[string]any{"session_id": "conversation-a"},
	})
	withoutMetadata := codexTurnStateScopeDigest(request, "gpt-5.2-codex", nil)
	if withMetadata == "" || withoutMetadata == "" {
		t.Fatalf("CP-HDR-022 scopes=%q %q", withMetadata, withoutMetadata)
	}
	// Both are valid units; the declared conversation must win over the prompt
	// cache fallback so two conversations never collapse onto one unit.
	withCache := codexTurnStateScopeDigest(request, "gpt-5.2-codex", map[string]any{
		"prompt_cache_key": "shared",
		"client_metadata":  map[string]any{"session_id": "conversation-a"},
	})
	if withCache != withMetadata {
		t.Fatalf("CP-HDR-022 declared conversation lost to the cache signal: %q vs %q", withCache, withMetadata)
	}
}
