package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
)

// 上游隐式前缀缓存的可用长度,是「相邻两次请求提示词」的公共前缀:Codex 后端
// 按 128 token 块计量,并把命中的 token 数回报为 cached_input_tokens。
// 线上核对(2026-09-22 rounds 524-528,同一 Claude Code 会话)显示 cached 恒为
// 17920(=140×128),而 input 从 29552 涨到 67119,即只有一段固定公共头被复用。
//
// 本文件用本地合成会话把两件事分开:网关转换是否逐轮引入变化(引入则缓存必然
// 失效,属网关缺陷),以及客户端提示词形态本身的可复用长度(属客户端形态)。
// 比较对象是按逻辑结构渲染的提示词——instructions + 工具定义 + 逐条 input 项——
// 而不是请求体原始字节:转换产物按键名字典序序列化,会话数组排在
// instructions/tools 之前,追加一项就会让后续字节整体位移,原始字节前缀
// 因此天然短于可复用前缀(见 append_only 子测试记录的实际字节数)。
// 字节→token 一律按 4 字节/token 估算,只用于与线上的 128 块台阶对齐量级。
const prefixCacheModel = "gpt-5.2-codex"

// Claude Code 声明的会话身份;带上它,prompt_cache_key 才按会话稳定派生
// (codexPromptCacheHash),否则退化为逐请求变化的键。
const claudeSessionHeader = "X-Claude-Code-Session-Id"

func cacheSystemBlocks() []any {
	return []any{map[string]any{
		"type":          "text",
		"text":          "You are a coding agent operating in a terminal." + strings.Repeat(" stable system guidance", 80),
		"cache_control": map[string]any{"type": "ephemeral"},
	}}
}

func cacheTools() []any {
	tool := func(name, description, property string) map[string]any {
		return map[string]any{
			"name":        name,
			"description": strings.Repeat(description, 40),
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{property: map[string]any{"type": "string"}},
				"required":   []any{property},
			},
		}
	}
	return []any{tool("Read", "read a file ", "file_path"), tool("Bash", "run a command ", "command")}
}

func cacheUserMessage(text string) map[string]any {
	return map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}
}

// cacheToolRound 追加一轮读文件工具调用;payload 放大工具结果正文,
// 使每轮的提示词增长足够明显,便于定位公共前缀的终点。
func cacheToolRound(turn, payload int) []any {
	return []any{
		cacheUserMessage(fmt.Sprintf("turn %d: inspect the workspace", turn)),
		map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": fmt.Sprintf("call_%d", turn), "name": "Read",
			"input": map[string]any{"file_path": fmt.Sprintf("/workspace/file_%d.go", turn)},
		}}},
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": fmt.Sprintf("call_%d", turn),
			"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", payload)}},
		}}},
	}
}

// convertedCodexBody 把 Anthropic 入站请求跑过真实转换路径(含 prompt_cache_key
// 注入),返回最终发往 Codex 上游的请求体。
func convertedCodexBody(t *testing.T, sessionID string, messages []any) []byte {
	t.Helper()
	var captured []byte
	handler, _ := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{
		stream: func(_ context.Context, request codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
			captured = bytes.Clone(request.Body)
			if err := started(codexresponses.StreamStart{}); err != nil {
				return err
			}
			if err := emit([]byte(`data: {"type":"response.created","response":{"object":"response","id":"resp_prefix","status":"in_progress"}}` + "\n\n")); err != nil {
				return err
			}
			return emit([]byte(`data: {"type":"response.completed","response":{"object":"response","id":"resp_prefix","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":100,"output_tokens":10}}}` + "\n\n"))
		}})
	body, err := json.Marshal(map[string]any{
		"model": prefixCacheModel, "max_tokens": 4096, "stream": true,
		"system": cacheSystemBlocks(), "tools": cacheTools(), "messages": messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-client-key")
	if sessionID != "" {
		r.Header.Set(claudeSessionHeader, sessionID)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(captured) == 0 {
		t.Fatal("未捕获到上游请求体")
	}
	return captured
}

type convertedPrompt struct {
	instructions string
	tools        []json.RawMessage
	items        []json.RawMessage
	cacheKey     string
}

func parseConvertedPrompt(t *testing.T, body []byte) convertedPrompt {
	t.Helper()
	var parsed struct {
		Instructions   string            `json:"instructions"`
		Tools          []json.RawMessage `json:"tools"`
		Input          []json.RawMessage `json:"input"`
		PromptCacheKey string            `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("解析转换产物失败: %v", err)
	}
	return convertedPrompt{instructions: parsed.Instructions, tools: parsed.Tools, items: parsed.Input, cacheKey: parsed.PromptCacheKey}
}

// render 把提示词按模型看到的逻辑顺序展开:system/instructions、工具定义、
// 会话项。上游重建提示词时不会沿用请求体的键序,因此可复用长度按这里比较。
func (p convertedPrompt) render() []byte {
	var out bytes.Buffer
	out.WriteString(p.instructions)
	for _, tool := range p.tools {
		out.Write(tool)
	}
	for _, item := range p.items {
		out.Write(item)
	}
	return out.Bytes()
}

func commonPrefixLen(a, b []byte) int {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	i := 0
	for i < limit && a[i] == b[i] {
		i++
	}
	return i
}

// estimatedBlocks 把字节估算成 128 token 块数,用于和线上台阶对照。
func estimatedBlocks(bytesLen int) int { return (bytesLen/4 + 127) / 128 }

func TestConvertedPrefixReuseAcrossTurns(t *testing.T) {
	const session = "11111111-2222-3333-4444-555555555555"
	base := []any{cacheUserMessage("synthetic session start")}

	// 网关不得逐轮引入变化:同一会话逐轮追加时,上一轮的完整提示词必须仍是
	// 下一轮的前缀,且缓存键稳定。这决定了"低命中"不能归因于转换本身。
	t.Run("append_only_reuses_whole_prefix", func(t *testing.T) {
		messages := append([]any{}, base...)
		var previous convertedPrompt
		var previousBody []byte
		for turn := 1; turn <= 3; turn++ {
			messages = append(messages, cacheToolRound(turn, 2048)...)
			body := convertedCodexBody(t, session, messages)
			current := parseConvertedPrompt(t, body)
			if turn > 1 {
				if previous.cacheKey != current.cacheKey {
					t.Fatalf("turn %d: 缓存键必须按会话稳定: %q → %q", turn, previous.cacheKey, current.cacheKey)
				}
				rendered, prior := current.render(), previous.render()
				if got := commonPrefixLen(prior, rendered); got != len(prior) {
					t.Fatalf("turn %d: 转换引入了逐轮变化,可复用前缀=%d/%d", turn, got, len(prior))
				}
				t.Logf("turn %d: 提示词可复用 %d 字节(≈%d tokens,≈%d 个 128 块);同一请求体原始字节前缀仅 %d 字节,差异来自会话数组排在 instructions/tools 之前",
					turn, len(prior), len(prior)/4, estimatedBlocks(len(prior)), commonPrefixLen(previousBody, body))
				if current.instructions != previous.instructions || len(current.tools) != len(previous.tools) {
					t.Fatalf("turn %d: instructions/tools 必须逐轮不变", turn)
				}
			}
			previous, previousBody = current, body
		}
	})

	// 同一 session 下多分支交替:与本分支上一轮仍整段可复用,但与"另一分支最近
	// 一次请求"只有公共头。线上 rounds 524-528 的 cached 恒为 17920(=140×128)
	// 且不随 input 增长,就是这个形态。
	t.Run("interleaved_branches_share_only_head", func(t *testing.T) {
		branchA := append(append([]any{}, base...), cacheToolRound(101, 2048)...)
		branchB := append(append([]any{}, base...), cacheToolRound(202, 4096)...)
		a1 := parseConvertedPrompt(t, convertedCodexBody(t, session, branchA))
		b1 := parseConvertedPrompt(t, convertedCodexBody(t, session, branchB))
		a2 := parseConvertedPrompt(t, convertedCodexBody(t, session, append(branchA, cacheToolRound(102, 2048)...)))
		b2 := parseConvertedPrompt(t, convertedCodexBody(t, session, append(branchB, cacheToolRound(203, 4096)...)))

		if got := commonPrefixLen(a1.render(), a2.render()); got != len(a1.render()) {
			t.Fatalf("分支 A 内追加仍应整段可复用: %d/%d", got, len(a1.render()))
		}
		if got := commonPrefixLen(b1.render(), b2.render()); got != len(b1.render()) {
			t.Fatalf("分支 B 内追加仍应整段可复用: %d/%d", got, len(b1.render()))
		}
		head := commonPrefixLen(a1.render(), b1.render())
		if head >= len(a1.render()) || head >= len(b1.render()) {
			t.Fatalf("夹具未构造出分支差异: head=%d A1=%d B1=%d", head, len(a1.render()), len(b1.render()))
		}
		if got := commonPrefixLen(b1.render(), a2.render()); got != head {
			t.Fatalf("A2 与最近一次请求 B1 之间只剩公共头: got=%d want=%d", got, head)
		}
		if got := commonPrefixLen(a1.render(), b2.render()); got != head {
			t.Fatalf("B2 与最近一次请求 A1 之间只剩公共头: got=%d want=%d", got, head)
		}
		t.Logf("公共头 %d 字节(≈%d tokens,≈%d 个 128 块)是分支交替时唯一可复用的部分;本分支上一轮为 %d 字节(≈%d tokens)",
			head, head/4, estimatedBlocks(head), len(a1.render()), len(a1.render())/4)
	})

	// 没有会话身份时键由稳定前缀指纹兜底:同一对话逐轮追加共享缓存身份,
	// 前缀被改写或换了对话才换键。否则键逐请求变化,上游命中必然为 0。
	t.Run("missing_session_identity_falls_back_to_stable_prefix", func(t *testing.T) {
		messages := append(append([]any{}, base...), cacheToolRound(1, 2048)...)
		first := parseConvertedPrompt(t, convertedCodexBody(t, "", messages))
		second := parseConvertedPrompt(t, convertedCodexBody(t, "", append(messages, cacheToolRound(2, 2048)...)))
		if first.cacheKey == "" || second.cacheKey == "" {
			t.Fatal("转换产物必须带 prompt_cache_key")
		}
		if first.cacheKey != second.cacheKey {
			t.Fatalf("同一对话逐轮追加必须共享缓存身份: %q → %q", first.cacheKey, second.cacheKey)
		}
		prior, rendered := first.render(), second.render()
		if got := commonPrefixLen(prior, rendered); got != len(prior) {
			t.Fatalf("提示词本身仍应逐轮追加: %d/%d", got, len(prior))
		}
		other := parseConvertedPrompt(t, convertedCodexBody(t, "", append([]any{cacheUserMessage("another conversation")}, cacheToolRound(1, 2048)...)))
		if other.cacheKey == first.cacheKey {
			t.Fatalf("不同对话不得共享缓存身份: %q", other.cacheKey)
		}
		rewritten := append([]any{}, messages...)
		rewritten[0] = cacheUserMessage("rewritten opening")
		if rewrittenPrompt := parseConvertedPrompt(t, convertedCodexBody(t, "", rewritten)); rewrittenPrompt.cacheKey == first.cacheKey {
			t.Fatalf("前缀被改写后必须换键: %q", rewrittenPrompt.cacheKey)
		}
		t.Logf("无会话身份时键 %q 按稳定前缀派生:逐轮追加复用,换对话或改写前缀即换键", first.cacheKey)
	})
}
