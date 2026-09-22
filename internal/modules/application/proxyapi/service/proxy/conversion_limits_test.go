package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	archive "aetherrelay/internal/pkg/aetherrelayarchive"
)

// Structural reproduction of 001454-001456, without customer prompts/arguments.
func longConversionHistory() []any {
	items := make([]any, 52)
	for i := range items {
		n := 2
		if i < 39 {
			n = 3
		}
		content := make([]any, n)
		for j := range content {
			content[j] = map[string]any{"type": "text", "text": "synthetic"}
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		items[i] = map[string]any{"role": role, "content": content}
	}
	args := make([]any, 64)
	for i := range args {
		args[i] = map[string]any{"value": i}
	}
	items[1].(map[string]any)["content"].([]any)[0] = map[string]any{"type": "tool_use", "id": "call_1", "name": "lookup", "input": map[string]any{"rows": args}}
	results := make([]any, 4)
	for i := range results {
		results[i] = map[string]any{"type": "text", "text": "result"}
	}
	items[2].(map[string]any)["content"].([]any)[0] = map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": results}
	return items
}

func TestLongConversionHistoryDoesNotCountBusinessObjectsAsBlocks(t *testing.T) {
	for _, removed := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(264-removed), func(t *testing.T) {
			items := longConversionHistory()
			for i := 0; i < removed; i++ {
				m := items[50+i].(map[string]any)
				m["content"] = m["content"].([]any)[:1]
			}
			var objects int
			var count func(any)
			count = func(v any) {
				switch x := v.(type) {
				case map[string]any:
					objects++
					for _, c := range x {
						count(c)
					}
				case []any:
					for _, c := range x {
						count(c)
					}
				}
			}
			count(items)
			if objects != 264-removed {
				t.Fatalf("fixture objects=%d", objects)
			}
			calls := 0
			h, _ := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{complete: func(_ context.Context, r codexresponses.Request) (codexresponses.Result, error) {
				calls++
				var body map[string]any
				if err := json.Unmarshal(r.Body, &body); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, raw := range body["input"].([]any) {
					item := raw.(map[string]any)
					if item["type"] == "function_call" {
						var args map[string]any
						if err := json.Unmarshal([]byte(item["arguments"].(string)), &args); err != nil {
							t.Fatal(err)
						}
						if len(args["rows"].([]any)) != 64 {
							t.Fatal("tool arguments lost")
						}
						found = true
					}
				}
				if !found {
					t.Fatal("tool call lost")
				}
				return codexresponses.Result{Body: []byte(`{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)}, nil
			}})
			body, _ := json.Marshal(map[string]any{"model": "gpt-5.2-codex", "messages": items})
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body)))
			r.Header.Set("Authorization", "Bearer test-client-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
		})
	}
}

func TestConversionLimitsAreIndependent(t *testing.T) {
	for _, label := range []string{"messages", "input"} {
		items := make([]any, maxConversionMessages)
		for i := range items {
			items[i] = map[string]any{"role": "user", "content": "text"}
		}
		if err := validateConversionTree(items, label); err != nil {
			t.Fatal(err)
		}
		assertConversionLimit(t, validateConversionTree(append(items, items[0]), label), "messages", maxConversionMessages+1, maxConversionMessages)
	}
	blocks := make([]any, maxConversionContentBlocks)
	for i := range blocks {
		blocks[i] = map[string]any{"type": "text", "text": "x"}
	}
	items := []any{map[string]any{"role": "user", "content": blocks}}
	if err := validateConversionTree(items, "messages"); err != nil {
		t.Fatal(err)
	}
	items[0].(map[string]any)["content"] = append(blocks, blocks[0])
	assertConversionLimit(t, validateConversionTree(items, "messages"), "content_blocks", maxConversionContentBlocks+1, maxConversionContentBlocks)
	// Business arrays can exceed the content-block budget; their nodes have a separate total budget.
	items = []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "input": make([]any, 300)}}}}
	if err := validateConversionTree(items, "messages"); err != nil {
		t.Fatal(err)
	}
	items[0].(map[string]any)["content"].([]any)[0].(map[string]any)["input"] = make([]any, maxConversionTreeNodes)
	assertConversionLimit(t, validateConversionTree(items, "messages"), "tree_nodes", maxConversionTreeNodes+1, maxConversionTreeNodes)
	var deep any = "leaf"
	for i := 0; i < maxConversionSchemaDepth+1; i++ {
		deep = []any{deep}
	}
	assertConversionLimit(t, validateConversionTree(deep, "messages"), "depth", 33, 32)
	// Nested tool_result content is protocol content, not arbitrary business JSON.
	items = []any{map[string]any{"content": []any{map[string]any{"type": "tool_result", "content": blocks}}}}
	assertConversionLimit(t, validateConversionTree(items, "messages"), "content_blocks", maxConversionContentBlocks+1, maxConversionContentBlocks)
}

func assertConversionLimit(t *testing.T, err error, kind string, actual, limit int) {
	t.Helper()
	var e *conversionLimitError
	if !errors.As(err, &e) || e.Kind != kind || e.Actual != actual || e.Limit != limit {
		t.Fatalf("error=%v want=%s %d/%d", err, kind, actual, limit)
	}
	api := conversionAPIError(TransportPlan{}, err)
	if api.Code != ErrorCodeConversionLimitExceeded || api.Param == "" || api.Actual != actual || api.Limit != limit || len(api.UnsupportedFeatures) != 0 || statusForAPIError(&api) != 400 || anthropicErrorType(api.Code) != "invalid_request_error" {
		t.Fatalf("api=%+v", api)
	}
}

func TestConversionLimitRecordedBeforeUpstream(t *testing.T) {
	h, root := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
		t.Fatal("limit reached upstream")
		return codexresponses.Result{}, nil
	}})
	items := make([]any, maxConversionMessages+1)
	for i := range items {
		items[i] = map[string]any{"role": "user", "content": "text"}
	}
	body, _ := json.Marshal(map[string]any{"model": "gpt-5.2-codex", "messages": items})
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer test-client-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "actual=257, limit=256") || strings.Contains(w.Body.String(), "unsupported_feature") {
		t.Fatalf("response=%s", w.Body.String())
	}
	events := usageEvents(t, h.usageStore)
	if len(events) != 1 || events[0].ErrorCode != ErrorCodeConversionLimitExceeded || events[0].Outcome != "limit_exceeded" || len(events[0].UnsupportedFeatures) != 0 {
		t.Fatalf("events=%+v", events)
	}
	data, err := os.ReadFile(filepath.Join(root, "test-client", "000001", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta archive.Metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.ErrorCode != ErrorCodeConversionLimitExceeded || meta.ConversionErrorPath != "messages" || meta.UpstreamStatus != 0 {
		t.Fatalf("metadata=%+v", meta)
	}
}

// 线上 rounds 253/276 的形态:99 条消息累计 259 个协议内容块。旧预算 256 拒绝,
// 新预算 512 必须放行,这是本次放宽的回归锚点。
func TestContentBlockBudgetAdmitsObservedSessionShape(t *testing.T) {
	for _, label := range []string{"messages", "input"} {
		items := make([]any, 99)
		blocks := 0
		for i := range items {
			n := 2
			if i < 61 {
				n = 3
			}
			content := make([]any, n)
			for j := range content {
				content[j] = map[string]any{"type": "text", "text": "synthetic"}
			}
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			items[i] = map[string]any{"role": role, "content": content}
			blocks += n
		}
		if blocks != 259 {
			t.Fatalf("fixture blocks=%d", blocks)
		}
		if err := validateConversionTree(items, label); err != nil {
			t.Fatalf("%s: 259 content blocks must be admitted: %v", label, err)
		}
	}
}

// 内容块预算在整条消息集上累计,不是逐条判定。
func TestContentBlocksAccumulateAcrossMessages(t *testing.T) {
	message := func(n int) map[string]any {
		content := make([]any, n)
		for i := range content {
			content[i] = map[string]any{"type": "text", "text": "x"}
		}
		return map[string]any{"role": "user", "content": content}
	}
	half := maxConversionContentBlocks / 2
	items := []any{message(half), message(half)}
	if err := validateConversionTree(items, "messages"); err != nil {
		t.Fatalf("cumulative blocks at the limit must be admitted: %v", err)
	}
	items[1] = message(half + 2)
	assertConversionLimit(t, validateConversionTree(items, "messages"), "content_blocks", maxConversionContentBlocks+2, maxConversionContentBlocks)
}

// system 数组保留独立预算,不随协议内容块放宽一起变化。
func TestSystemContentBlocksBudgetIsIndependent(t *testing.T) {
	parts := make([]any, maxConversionSystemBlocks)
	for i := range parts {
		parts[i] = map[string]any{"type": "text", "text": "x"}
	}
	if _, err := anthropicSystemText(parts); err != nil {
		t.Fatalf("system at its own limit must be admitted: %v", err)
	}
	parts = append(parts, map[string]any{"type": "text", "text": "x"})
	_, err := anthropicSystemText(parts)
	assertConversionLimit(t, err, "content_blocks", maxConversionSystemBlocks+1, maxConversionSystemBlocks)
}

// Chat→Responses 的顶层消息条数走消息预算,不随协议内容块放宽。
func TestChatMessageBudgetStaysAtMessageLimit(t *testing.T) {
	messages := make([]any, maxConversionMessages)
	for i := range messages {
		messages[i] = map[string]any{"role": "user", "content": "hello"}
	}
	if _, err := buildCodexResponsesFromChat(map[string]any{"model": "gpt-5.2-codex", "messages": messages}, "gpt-5.2-codex"); err != nil {
		t.Fatalf("chat history at the message limit must be admitted: %v", err)
	}
	messages = append(messages, map[string]any{"role": "user", "content": "hello"})
	if _, err := buildCodexResponsesFromChat(map[string]any{"model": "gpt-5.2-codex", "messages": messages}, "gpt-5.2-codex"); err == nil {
		t.Fatal("chat history above the message limit must be rejected")
	}
}

// 边界在 handler 层同样成立:达到上限调用上游,越过上限本地拒绝且不触上游。
func TestContentBlockBudgetBoundaryReachesUpstream(t *testing.T) {
	calls := 0
	h, _ := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
		calls++
		return codexresponses.Result{Body: []byte(`{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)}, nil
	}})
	content := make([]any, maxConversionContentBlocks)
	for i := range content {
		content[i] = map[string]any{"type": "text", "text": "x"}
	}
	post := func(items []any) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]any{"model": "gpt-5.2-codex", "messages": items})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body)))
		r.Header.Set("Authorization", "Bearer test-client-key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post([]any{map[string]any{"role": "user", "content": content}}); w.Code != 200 || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	over := post([]any{map[string]any{"role": "user", "content": append(content, map[string]any{"type": "text", "text": "x"})}})
	if over.Code != 400 || calls != 1 || !strings.Contains(over.Body.String(), "actual=513, limit=512") || strings.Contains(over.Body.String(), "unsupported_feature") {
		t.Fatalf("status=%d calls=%d body=%s", over.Code, calls, over.Body.String())
	}
}
