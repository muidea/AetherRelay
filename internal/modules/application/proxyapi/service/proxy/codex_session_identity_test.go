package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	clientauth "aetherrelay/internal/pkg/aetherrelayclientauth"
	"github.com/google/uuid"
)

func codexIdentityRequest(keyID, model string, headers map[string]string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	request = request.WithContext(clientauth.WithClientIdentity(request.Context(), clientauth.ClientIdentity{KeyID: keyID}))
	return request
}

// CP-HDR-007..010: 出站会话身份是确定性的 UUID，按客户端身份与模型命名空间隔离，
// 并且同一会话跨请求稳定。
func TestCodexSessionIdentityIsStableUUID(t *testing.T) {
	const conversation = "01a080cb-abf8-7900-97a7-7af78ed32b94"
	identity := codexSessionHash(codexIdentityRequest("key-a", "gpt-5.6-sol", map[string]string{"Session-Id": conversation}), "gpt-5.6-sol", nil)
	parsed, err := uuid.Parse(identity)
	if err != nil || len(identity) != 36 || parsed.Version() != 4 {
		t.Fatalf("CP-HDR-007 identity=%q err=%v", identity, err)
	}
	repeated := codexSessionHash(codexIdentityRequest("key-a", "gpt-5.6-sol", map[string]string{"Session-Id": conversation}), "gpt-5.6-sol", nil)
	if repeated != identity {
		t.Fatalf("CP-HDR-007 identity not stable: %q vs %q", identity, repeated)
	}
	// 会话信号不同、客户端不同、模型不同都必须得到不同的会话身份。
	for name, other := range map[string]string{
		"other conversation": codexSessionHash(codexIdentityRequest("key-a", "gpt-5.6-sol", map[string]string{"Session-Id": "01a080cb-abf8-7900-97a7-7af78ed32b95"}), "gpt-5.6-sol", nil),
		"other client":       codexSessionHash(codexIdentityRequest("key-b", "gpt-5.6-sol", map[string]string{"Session-Id": conversation}), "gpt-5.6-sol", nil),
		"other model":        codexSessionHash(codexIdentityRequest("key-a", "gpt-5.5", map[string]string{"Session-Id": conversation}), "gpt-5.5", nil),
	} {
		if other == identity {
			t.Fatalf("CP-HDR-007 %s reused the identity %q", name, identity)
		}
	}
	if codexSessionDigest(nil, "gpt-5.6-sol", nil, true) != "" {
		t.Fatal("CP-HDR-007 nil request produced an identity")
	}
}

func TestCodexSessionIdentityIsRequestScopedWhenConversationIsMissing(t *testing.T) {
	first := codexIdentityRequest("key-a", "gpt-5.6-sol", nil)
	second := codexIdentityRequest("key-a", "gpt-5.6-sol", nil)
	firstHash := codexSessionHash(first, "gpt-5.6-sol", nil)
	if repeated := codexSessionHash(first, "gpt-5.6-sol", nil); repeated != firstHash {
		t.Fatalf("one request changed its fallback conversation: %q vs %q", firstHash, repeated)
	}
	if secondHash := codexSessionHash(second, "gpt-5.6-sol", nil); secondHash == firstHash {
		t.Fatalf("stateless requests shared a fallback conversation: %q", firstHash)
	}

	// A public request ID is client-controlled and must not restore cross-request
	// affinity. Production middleware supplies a separate server nonce.
	first = first.WithContext(withRequestScopeID(withRequestID(first.Context(), "reused-public-id"), "server-scope-a"))
	second = second.WithContext(withRequestScopeID(withRequestID(second.Context(), "reused-public-id"), "server-scope-b"))
	if codexSessionHash(first, "gpt-5.6-sol", nil) == codexSessionHash(second, "gpt-5.6-sol", nil) {
		t.Fatal("client-controlled request id merged stateless conversations")
	}
}

func TestCodexLogicalThreadHashOnlyCapturesDistinctThread(t *testing.T) {
	request := codexIdentityRequest("key-a", "gpt-5.6-sol", map[string]string{"Session-Id": "conversation-a", "Thread-Id": "conversation-a"})
	if got := codexLogicalThreadHash(request, "gpt-5.6-sol", nil); got != "" {
		t.Fatalf("equal client session/thread produced a distinct logical thread: %q", got)
	}
	request.Header.Set("Thread-Id", "thread-a")
	first := codexLogicalThreadHash(request, "gpt-5.6-sol", nil)
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("distinct logical thread hash=%q err=%v", first, err)
	}
	if repeated := codexLogicalThreadHash(request, "gpt-5.6-sol", nil); repeated != first {
		t.Fatalf("logical thread hash changed: %q vs %q", first, repeated)
	}
	request.Header.Set("Thread-Id", "thread-b")
	if other := codexLogicalThreadHash(request, "gpt-5.6-sol", nil); other == first {
		t.Fatalf("distinct client threads collided: %q", first)
	}
}

func TestCodexBodyIdentitySeparatesConversationFromThread(t *testing.T) {
	request := codexIdentityRequest("key-a", "gpt-5.6-sol", nil)
	firstBody := map[string]any{"client_metadata": map[string]any{"session_id": "conversation-a", "thread_id": "thread-a"}}
	secondBody := map[string]any{"client_metadata": map[string]any{"session_id": "conversation-a", "thread_id": "thread-b"}}
	if first, second := codexSessionHash(request, "gpt-5.6-sol", firstBody), codexSessionHash(request, "gpt-5.6-sol", secondBody); first != second {
		t.Fatalf("same logical conversation produced different sessions: %q vs %q", first, second)
	}
	if first, second := codexLogicalThreadHash(request, "gpt-5.6-sol", firstBody), codexLogicalThreadHash(request, "gpt-5.6-sol", secondBody); first == "" || second == "" || first == second {
		t.Fatalf("distinct logical threads were not separated: %q vs %q", first, second)
	}
}

// CP-REQ-016: body 归一化不得改变客户端的字符串字节——转义 <, >, & 会让每个
// 请求体膨胀 5 字节/处，Rust 客户端本身不发送这种形式。
func TestCodexNormalizedBodyKeepsClientBytesVerbatim(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"if a < b && c > d { f(&x); }"}]}],"big":12345678901234567890,"ratio":1.0}`)
	normalized, _, _, err := normalizeCodexRequest(raw, false)
	if err != nil {
		t.Fatalf("normalizeCodexRequest: %v", err)
	}
	body := string(normalized)
	if !strings.Contains(body, `if a < b && c > d { f(&x); }`) {
		t.Fatalf("CP-REQ-016 client bytes were escaped: %s", body)
	}
	for _, escaped := range []string{"u003c", "u003e", "u0026"} {
		if strings.Contains(body, escaped) {
			t.Fatalf("CP-REQ-016 body still carries the %s escape: %s", escaped, body)
		}
	}
	// 大整数与原始字面量按 decodeCodexJSON 的 UseNumber 语义保持原文。
	for _, want := range []string{"12345678901234567890", `"ratio":1.0`} {
		if !strings.Contains(body, want) {
			t.Fatalf("CP-REQ-016 lost literal %s: %s", want, body)
		}
	}
	// 归一化本身声明的结构变更仍然生效。
	for _, want := range []string{`"store":false`, `"stream":true`, `"instructions":""`} {
		if !strings.Contains(body, want) {
			t.Fatalf("CP-REQ-016 missing normalization %s: %s", want, body)
		}
	}
}
