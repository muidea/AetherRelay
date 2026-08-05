package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptfail"
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptimage"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetrics"
	"ai-proxy/internal/pkg/aiproxyusage"
	"ai-proxy/internal/pkg/chatgpttokenusage"
)

func TestParseChatGPTImageMultipartReadsImageAndMask(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("prompt", "edit"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("n", "1"); err != nil {
		t.Fatal(err)
	}
	image, err := writer.CreateFormFile("image", "image.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(image, "image-bytes"); err != nil {
		t.Fatal(err)
	}
	mask, err := writer.CreateFormFile("mask", "mask.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(mask, "mask-bytes"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/images/edits", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	parsed, images, masks, err := parseChatGPTImageMultipart(rec, req, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Prompt != "edit" || parsed.N != 1 || len(images) != 1 || len(masks) != 1 || string(images[0]) != "image-bytes" || string(masks[0]) != "mask-bytes" {
		t.Fatalf("parsed=%+v images=%q masks=%q", parsed, images, masks)
	}
}

type chatGPTImageExecutorStub struct {
	generate func(context.Context, chatgptimage.Request) (chatgptimage.Result, error)
	edit     func(context.Context, chatgptimage.Request) (chatgptimage.Result, error)
}

func (s chatGPTImageExecutorStub) GenerateImage(ctx context.Context, req chatgptimage.Request) (chatgptimage.Result, error) {
	if s.generate != nil {
		return s.generate(ctx, req)
	}
	return chatgptimage.Result{
		Created: 1,
		Data:    []chatgptimage.Data{{B64JSON: "aaa"}},
		Usage:   &tokenusage.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
	}, nil
}

func (s chatGPTImageExecutorStub) EditImage(ctx context.Context, req chatgptimage.Request) (chatgptimage.Result, error) {
	if s.edit != nil {
		return s.edit(ctx, req)
	}
	return chatgptimage.Result{
		Created: 1,
		Data:    []chatgptimage.Data{{B64JSON: "bbb"}},
		Usage:   &tokenusage.Usage{InputTokens: 5, OutputTokens: 7, TotalTokens: 12},
	}, nil
}

func newChatGPTImageHandler(t *testing.T, store usage.Store, exec chatgptimage.Executor) *Handler {
	t.Helper()
	cfg := mustHandlerConfig(config.Config{ChatGPTWeb: config.ChatGPTWebConfig{}})
	h := NewHandler(cfg, store, nil, metrics.NewRegistry()).WithChatGPTImageExecutor(exec)
	h.ReplaceEffectiveCatalog(effectivecatalog.Build(cfg, 1, 1, []effectivecatalog.PoolModel{{
		ID: "gpt-image-2",
	}}, "2026-07-26T00:00:00Z"))
	return h
}

func TestChatGPTImageSuccessSettlesUsage(t *testing.T) {
	store := usage.NewMemoryStore()
	h := newChatGPTImageHandler(t, store, chatGPTImageExecutorStub{})
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"a cat","n":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "prompt_tokens") {
		t.Fatalf("usage must not leak into client JSON: %s", resp.Body.String())
	}
	events := usageEvents(t, store)
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	ev := events[0]
	if ev.Outcome != "success" || ev.Provider != "chatgptweb" || ev.Model != "gpt-image-2" {
		t.Fatalf("event=%+v", ev)
	}
	if ev.UpstreamEndpoint != "chatgptweb_images" || ev.UpstreamProtocol != "chatgptweb" {
		t.Fatalf("image transport labels=%+v", ev)
	}
	if ev.Estimated || ev.InputTokens != 10 || ev.OutputTokens != 20 {
		t.Fatalf("expected non-estimated upstream usage: %+v", ev)
	}
}

func TestChatGPTImageKeepsInternalMetadataWithoutLeakingIt(t *testing.T) {
	store := usage.NewMemoryStore()
	exec := chatGPTImageExecutorStub{generate: func(context.Context, chatgptimage.Request) (chatgptimage.Result, error) {
		return chatgptimage.Result{
			Created:        1,
			Data:           []chatgptimage.Data{{B64JSON: "aaa"}},
			Usage:          &tokenusage.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8},
			ConversationID: "conversation-1",
			AccountID:      "account-1",
		}, nil
	}}
	h := newChatGPTImageHandler(t, store, exec)
	trace := &featureExecutionTrace{}
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"a cat"}`))
	req = req.WithContext(context.WithValue(req.Context(), featureExecutionTraceKey{}, trace))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if trace.conversationID != "conversation-1" || trace.accountID != "account-1" || trace.usage == nil || trace.usage.TotalTokens != 8 {
		t.Fatalf("trace=%+v", trace)
	}
	for _, private := range []string{"conversation-1", "account-1", "total_tokens"} {
		if strings.Contains(resp.Body.String(), private) {
			t.Fatalf("public response leaked %q: %s", private, resp.Body.String())
		}
	}
}

func TestChatGPTImageFailureSettlesUsage(t *testing.T) {
	store := usage.NewMemoryStore()
	exec := chatGPTImageExecutorStub{
		generate: func(context.Context, chatgptimage.Request) (chatgptimage.Result, error) {
			return chatgptimage.Result{Usage: &tokenusage.Usage{PromptTokens: 3, CompletionTokens: 0, TotalTokens: 3}, ConversationID: "conversation-1", AccountID: "account-1"}, chatgptfail.New(chatgptfail.KindUpstream, errors.New("upstream boom"))
		},
	}
	h := newChatGPTImageHandler(t, store, exec)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"a cat"}`))
	trace := &featureExecutionTrace{}
	req = req.WithContext(context.WithValue(req.Context(), featureExecutionTraceKey{}, trace))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	events := usageEvents(t, store)
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	ev := events[0]
	if ev.Outcome != "upstream_failed" || ev.ErrorCode != "upstream" {
		t.Fatalf("event=%+v", ev)
	}
	if ev.InputTokens != 3 {
		t.Fatalf("partial usage lost: %+v", ev)
	}
	if trace.conversationID != "conversation-1" || trace.accountID != "account-1" {
		t.Fatalf("recovery trace=%+v", trace)
	}
}

func TestChatGPTImageEditSuccessSettlesUsage(t *testing.T) {
	store := usage.NewMemoryStore()
	h := newChatGPTImageHandler(t, store, chatGPTImageExecutorStub{})
	// minimal 1x1 png-ish base64 content is not required by stub edit path when JSON images empty?
	// Edit path requires images via JSON; provide a tiny data URL-ish base64 payload that DecodeBase64Images accepts.
	body := `{"model":"gpt-image-2","prompt":"edit me","image":"aGVsbG8="}`
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	events := usageEvents(t, store)
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	ev := events[0]
	if ev.Outcome != "success" || ev.Provider != "chatgptweb" || ev.Estimated {
		t.Fatalf("event=%+v", ev)
	}
	// edit stub uses Input/Output tokens
	if ev.InputTokens != 5 || ev.OutputTokens != 7 {
		t.Fatalf("edit usage mapping: %+v", ev)
	}
}

func TestChatGPTImagePartialNFailureKeepsUsage(t *testing.T) {
	store := usage.NewMemoryStore()
	// At the Executor/HTTP boundary, partial n accumulation is already projected into
	// Result.Usage alongside a non-nil error (design §4.6 / Phase 2).
	exec := chatGPTImageExecutorStub{
		generate: func(_ context.Context, req chatgptimage.Request) (chatgptimage.Result, error) {
			if req.N < 2 {
				t.Fatalf("expected n>=2, got %d", req.N)
			}
			return chatgptimage.Result{
				Data:  []chatgptimage.Data{{B64JSON: "one"}},
				Usage: &tokenusage.Usage{PromptTokens: 11, CompletionTokens: 13, TotalTokens: 24},
			}, chatgptfail.New(chatgptfail.KindUpstream, errors.New("second of n failed"))
		},
	}
	h := newChatGPTImageHandler(t, store, exec)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"cats","n":2}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-client-key")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	events := usageEvents(t, store)
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	ev := events[0]
	if ev.Outcome != "upstream_failed" {
		t.Fatalf("event=%+v", ev)
	}
	if ev.InputTokens != 11 || ev.OutputTokens != 13 || ev.Estimated {
		t.Fatalf("expected accumulated non-estimated usage retained on failure: %+v", ev)
	}
}
