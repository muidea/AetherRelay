package proxy

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aetherrelay/internal/pkg/aetherrelayarchive"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelaymetrics"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

func newArchiveTestRound(t *testing.T) *archive.Round {
	t.Helper()
	recorder, err := archive.NewRecorder(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	round, err := recorder.Start()
	if err != nil {
		t.Fatal(err)
	}
	return round
}

func TestArchiveResponseWriterSnapshotsFirstWrite(t *testing.T) {
	round := newArchiveTestRound(t)
	underlying := httptest.NewRecorder()
	writer := &archiveResponseWriter{ResponseWriter: underlying, round: round}

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	// 响应头已定型,之后的修改不会再发给客户端,快照必须保持不变。
	writer.Header().Set("Content-Type", "text/plain")
	writer.WriteHeader(http.StatusInternalServerError)

	snapshot, ok := round.ClientResponse()
	if !ok {
		t.Fatal("expected a captured snapshot")
	}
	if snapshot.Status != http.StatusOK || snapshot.Hijacked {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if got := snapshot.Headers["Content-Type"]; len(got) != 1 || got[0] != "application/json" {
		t.Fatalf("unexpected headers: %+v", snapshot.Headers)
	}
}

func TestArchiveResponseWriterCapturesImplicitOKOnWrite(t *testing.T) {
	round := newArchiveTestRound(t)
	writer := &archiveResponseWriter{ResponseWriter: httptest.NewRecorder(), round: round}

	if _, err := writer.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	snapshot, ok := round.ClientResponse()
	if !ok || snapshot.Status != http.StatusOK {
		t.Fatalf("unexpected snapshot: %+v ok=%t", snapshot, ok)
	}
}

// Flush 必须自己补捉隐式 200：底层 ResponseWriter 补写的那次 WriteHeader
// 不会回流到本包装器。
func TestArchiveResponseWriterFlushCapturesImplicitOK(t *testing.T) {
	round := newArchiveTestRound(t)
	underlying := httptest.NewRecorder()
	writer := &archiveResponseWriter{ResponseWriter: underlying, round: round}

	writer.Flush()

	if !underlying.Flushed {
		t.Fatal("expected the underlying writer to be flushed")
	}
	snapshot, ok := round.ClientResponse()
	if !ok || snapshot.Status != http.StatusOK {
		t.Fatalf("unexpected snapshot: %+v ok=%t", snapshot, ok)
	}
}

// Codex Responses WebSocket 依赖 w.(http.Hijacker) 直接断言,包装器遮蔽该接口
// 会让升级全线 500。
func TestArchiveResponseWriterPassthroughsHijack(t *testing.T) {
	round := newArchiveTestRound(t)
	underlying := &hijackableResponseRecorder{ResponseRecorder: httptest.NewRecorder()}
	underlying.Header().Set("X-Request-ID", "req-1")
	writer := &archiveResponseWriter{ResponseWriter: underlying, round: round}

	if _, _, err := writer.Hijack(); err != nil {
		t.Fatalf("hijack: %v", err)
	}
	if !underlying.hijacked {
		t.Fatal("hijack must be forwarded to the underlying writer")
	}
	snapshot, ok := round.ClientResponse()
	if !ok {
		t.Fatal("expected a snapshot after hijack")
	}
	// 101 由升级方直接写到底层连接,这里是推断值,必须带 Hijacked 标记。
	if snapshot.Status != http.StatusSwitchingProtocols || !snapshot.Hijacked {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if got := snapshot.Headers["X-Request-Id"]; len(got) != 1 || got[0] != "req-1" {
		t.Fatalf("unexpected headers: %+v", snapshot.Headers)
	}
}

func TestArchiveResponseWriterReportsUnsupportedHijack(t *testing.T) {
	round := newArchiveTestRound(t)
	// httptest.ResponseRecorder 不实现 http.Hijacker。
	writer := &archiveResponseWriter{ResponseWriter: httptest.NewRecorder(), round: round}

	if _, _, err := writer.Hijack(); !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("err = %v, want http.ErrNotSupported", err)
	}
	if _, ok := round.ClientResponse(); ok {
		t.Fatal("a failed hijack must not produce a snapshot")
	}
}

func TestArchiveResponseWriterUnwrapReachesUnderlyingWriter(t *testing.T) {
	underlying := httptest.NewRecorder()
	writer := &archiveResponseWriter{ResponseWriter: underlying}
	if writer.Unwrap() != http.ResponseWriter(underlying) {
		t.Fatal("Unwrap must return the underlying writer")
	}
	if err := http.NewResponseController(writer).Flush(); err != nil {
		t.Fatalf("response controller flush: %v", err)
	}
	if !underlying.Flushed {
		t.Fatal("expected the underlying writer to be flushed")
	}
}

func TestArchiveResponseMetaRecordsClientAndUpstreamHeaders(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`), nil
	})
	tmpDir := t.TempDir()
	handler := testHandler("https://upstream.test", tmpDir, "openai")
	handler.client.Transport = transport

	handler.ServeHTTP(newResponseRecorder(), newRequest(http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))

	interactionDir := filepath.Join(tmpDir, "interactions", "000001")
	responseMeta := filepath.Join(interactionDir, "response.meta.json")
	assertFileContains(t, responseMeta, `"status": 200`)
	assertFileContains(t, responseMeta, `"Content-Type"`)
	assertFileContains(t, responseMeta, `"X-Request-Id"`)
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"response_meta_path": "response.meta.json"`)
	// 上游响应方向必须带完整 header map,而不只是 content_type / content_length。
	assertFileContains(t, filepath.Join(interactionDir, "upstream_response.json"), `"headers"`)
	assertFileContains(t, filepath.Join(interactionDir, "upstream_response.json"), `"application/json"`)
}

// 客户端响应会经 copyResponseHeader 全量转发上游 header,因此归档必须在两个
// 方向都脱敏,同时保住 x-ratelimit-* 这类排查限流所必需的诊断头。
func TestArchiveRedactsSensitiveHeadersButKeepsRateLimit(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		response := jsonResponse(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		response.Header.Set("Set-Cookie", "sid=upstream-secret")
		response.Header.Set("WWW-Authenticate", `Bearer realm="upstream-secret"`)
		response.Header.Set("X-RateLimit-Remaining-Requests", "42")
		return response, nil
	})
	tmpDir := t.TempDir()
	handler := testHandler("https://upstream.test", tmpDir, "openai")
	handler.client.Transport = transport

	handler.ServeHTTP(newResponseRecorder(), newRequest(http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))

	interactionDir := filepath.Join(tmpDir, "interactions", "000001")
	for _, name := range []string{"response.meta.json", "upstream_response.json"} {
		path := filepath.Join(interactionDir, name)
		assertFileContains(t, path, `<redacted>`)
		assertFileNotContains(t, path, "upstream-secret")
	}
	assertFileContains(t, filepath.Join(interactionDir, "response.meta.json"), `"42"`)
	assertFileContains(t, filepath.Join(interactionDir, "upstream_response.json"), `"42"`)
}

func TestArchiveSSEResponseMetaKeepsNegotiatedHeaders(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return sseResponse(strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"he"}}]}`,
			"",
			`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n")), nil
	})
	tmpDir := t.TempDir()
	handler := testHandler("https://upstream.test", tmpDir, "openai")
	handler.client.Transport = transport

	response := newResponseRecorder()
	handler.ServeHTTP(response, newRequest(http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`))

	interactionDir := filepath.Join(tmpDir, "interactions", "000001")
	responseMeta := filepath.Join(interactionDir, "response.meta.json")
	// 快照取在 WriteHeader 之前已由 prepareSSEHeaders 定型的头。
	assertFileContains(t, responseMeta, `"status": 200`)
	assertFileContains(t, responseMeta, `"text/event-stream"`)
	assertFileContains(t, responseMeta, `"X-Accel-Buffering"`)
	assertFileNotContains(t, responseMeta, `"Content-Length"`)
	// Flush 透传未破坏原始流式归档。
	assertFileContains(t, filepath.Join(interactionDir, "response.sse"), "data: [DONE]")
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"response_meta_path": "response.meta.json"`)
}

// 客户端取消等早退路径不写 metadata.json,响应 header 仍必须靠兜底 defer 归档。
func TestArchiveResponseMetaSurvivesEarlyReturnWithoutMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	handler := testHandler("https://upstream.test", tmpDir, "openai")
	// beginUsage 在 usage store 缺失时写 503 并直接返回,该路径不写 metadata.json。
	handler.usageStore = nil

	response := newResponseRecorder()
	handler.ServeHTTP(response, newRequest(http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))

	interactionDir := filepath.Join(tmpDir, "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "response.meta.json"), `"status": 503`)
	if _, err := os.Stat(filepath.Join(interactionDir, "metadata.json")); !os.IsNotExist(err) {
		t.Fatalf("metadata.json must not exist on this path, stat err = %v", err)
	}
}

// header 属元数据层:archive_full_content=false 时正文缺席,四个方向的 header
// 仍必须齐全。
func TestArchiveHeadersWithoutFullContent(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := mustHandlerConfig(config.Config{
		ListenAddr:     ":0",
		InteractionDir: filepath.Join(tmpDir, "interactions"),
		Providers: map[string]config.Provider{
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://upstream.test", APIKey: "test-key"},
		},
	})
	recorder, err := archive.NewRecorderOptions(cfg.InteractionDir, archive.RecorderOptions{
		MaxRounds: 500, FullContent: false, ScopeByAPIKey: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(cfg, usage.NewMemoryStore(), recorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), nil
	})

	handler.ServeHTTP(newResponseRecorder(), newRequest(http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))

	interactionDir := filepath.Join(cfg.InteractionDir, "test-client", "000001")
	for _, name := range []string{"request.meta.json", "upstream_request.json", "upstream_response.json", "response.meta.json", "metadata.json"} {
		if _, err := os.Stat(filepath.Join(interactionDir, name)); err != nil {
			t.Fatalf("expected metadata file %s: %v", name, err)
		}
	}
	for _, name := range []string{"request.json", "response.json", "response.sse"} {
		if _, err := os.Stat(filepath.Join(interactionDir, name)); !os.IsNotExist(err) {
			t.Fatalf("body file %s must be absent when archive_full_content=false, stat err = %v", name, err)
		}
	}
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"response_meta_path": "response.meta.json"`)
}

// 归档关闭时 wrapper 不得生效,更不能把 w 替换成 nil。
func TestArchiveDisabledLeavesResponseUntouched(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := mustHandlerConfig(config.Config{
		ListenAddr:     ":0",
		InteractionDir: filepath.Join(tmpDir, "interactions"),
		Providers: map[string]config.Provider{
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://upstream.test", APIKey: "test-key"},
		},
	})
	// recorder 为 nil ⇔ archive_interactions 关闭。
	handler := NewHandler(cfg, usage.NewMemoryStore(), nil, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), nil
	})

	response := newResponseRecorder()
	handler.ServeHTTP(response, newRequest(http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(cfg.InteractionDir, "000001")); !os.IsNotExist(err) {
		t.Fatalf("interaction archive must stay empty, stat err = %v", err)
	}
}

func TestIsSensitiveHeaderCoversBothDirections(t *testing.T) {
	sensitive := []string{
		"Authorization", "authorization", "proxy-authorization", "Proxy-Authenticate",
		"WWW-Authenticate", "Authentication-Info", "X-API-Key", "x-api-key", "Api-Key",
		"X-Auth-Token", "X-Access-Token", "X-Goog-Api-Key", "X-Amz-Security-Token",
		"Cookie", "Set-Cookie",
	}
	// 这些是排查限流与上游行为时最需要看到的头,任何模糊后缀规则都会误伤它们。
	readable := []string{
		"X-RateLimit-Remaining-Requests", "x-ratelimit-remaining-tokens",
		"anthropic-ratelimit-input-tokens-remaining", "X-RateLimit-Reset-Tokens",
		"Retry-After", "OpenAI-Processing-Ms", "X-Request-Id",
		"Content-Type", "Content-Length", "Sec-WebSocket-Accept", "Sec-WebSocket-Key",
	}
	for _, key := range sensitive {
		if !isSensitiveHeader(key) {
			t.Errorf("isSensitiveHeader(%q) = false, want true", key)
		}
	}
	for _, key := range readable {
		if isSensitiveHeader(key) {
			t.Errorf("isSensitiveHeader(%q) = true, want false", key)
		}
	}
}

func assertFileNotContains(t *testing.T, path, unwanted string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), unwanted) {
		t.Fatalf("%s must not contain %q: %s", path, unwanted, body)
	}
}

// hijackableResponseRecorder 补上 httptest.ResponseRecorder 缺失的 Hijack。
type hijackableResponseRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackableResponseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}
