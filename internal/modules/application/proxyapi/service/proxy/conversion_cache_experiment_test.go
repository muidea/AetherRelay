package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
)

// Candidates exist only in the test binary, never in production config.
func experimentProjection(mode string) (func(string) map[string]any, error) {
	switch mode {
	case "merged":
		return nil, nil
	case "developer":
		return historicalSystemDeveloperMessage, nil
	case "system":
		return func(text string) map[string]any {
			return map[string]any{"type": "message", "role": mode, "content": []any{map[string]any{"type": "input_text", "text": text}}}
		}, nil
	default:
		return nil, fmt.Errorf("unknown experiment mode")
	}
}

func experimentHandler(t *testing.T, mode string, executor codexresponses.Executor) *Handler {
	t.Helper()
	projection, err := experimentProjection(mode)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := newArchivedCodexResponsesHandler(t, executor)
	h.cfg.ModelMetadata = map[string]config.ModelMetadata{"gpt-6-luna": {ReasoningDeclared: true, ReasoningSupported: true, ReasoningDefaultEffort: "low", ReasoningEfforts: []string{"low"}}}
	h.ReplaceEffectiveCatalog(effectivecatalog.BuildWithCodex(h.cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: "gpt-6-luna"}}}))
	h.anthropicResponsesBuilder = func(body map[string]any, model string, stream bool, capability config.ConversionCapability) ([]byte, []string, error) {
		return buildResponsesFromAnthropicWithHistoryProjection(body, model, stream, capability, projection)
	}
	return h
}

func experimentRequest(t *testing.T, h *Handler, session, system string, messages []any, tools []any) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"model": "gpt-6-luna", "system": system, "messages": messages, "max_tokens": 128, "stream": true, "thinking": map[string]any{"type": "adaptive"}, "output_config": map[string]any{"effort": "low"}}
	if tools != nil {
		body["tools"] = tools
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer test-client-key")
	r.Header.Set("X-Claude-Code-Session-Id", session)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type experimentResult struct {
	ProtocolOK bool   `json:"protocol_ok"`
	SemanticOK bool   `json:"semantic_ok"`
	Input      int    `json:"input_tokens"`
	Cached     int    `json:"cached_tokens"`
	CacheKnown bool   `json:"cache_known"`
	Text       string `json:"-"`
}

func evaluateExperiment(w *httptest.ResponseRecorder, expected string) experimentResult {
	var result experimentResult
	started, stopped, failed := false, false, false
	var usage tokenUsage
	scanner := bufio.NewScanner(bytes.NewReader(w.Body.Bytes()))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			failed = true
			continue
		}
		switch event["type"] {
		case "message_start":
			started = true
			if msg, ok := event["message"].(map[string]any); ok {
				mergeAnthropicUsage(&usage, msg["usage"])
			}
		case "content_block_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				if text, ok := delta["text"].(string); ok {
					result.Text += text
				}
			}
		case "content_block_start":
			if block, ok := event["content_block"].(map[string]any); ok {
				if text, ok := block["text"].(string); ok {
					result.Text += text
				}
			}
		case "message_delta":
			mergeAnthropicUsage(&usage, event["usage"])
		case "message_stop":
			stopped = true
		case "error":
			failed = true
		}
	}
	result.ProtocolOK = w.Code == 200 && started && stopped && !failed && scanner.Err() == nil
	result.SemanticOK = result.ProtocolOK && strings.TrimSpace(result.Text) == expected
	result.Input, result.Cached, result.CacheKnown = usage.PromptTokens, usage.CachedInputTokens, usage.CachedInputTokensKnown
	return result
}

func TestHistoryCandidatesKeepAppendOnlyPrefix(t *testing.T) {
	for _, mode := range []string{"system", "developer"} {
		t.Run(mode, func(t *testing.T) {
			var captured []byte
			h := experimentHandler(t, mode, codexResponsesExecutorStub{stream: func(_ context.Context, req codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
				captured = bytes.Clone(req.Body)
				if err := started(codexresponses.StreamStart{}); err != nil {
					return err
				}
				if err := emit([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"probe\",\"status\":\"in_progress\"}}\n\n")); err != nil {
					return err
				}
				return emit([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"probe\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}]}}\n\n"))
			}})
			messages := []any{cacheUserMessage("start")}
			var previous convertedPrompt
			for turn := 1; turn <= 3; turn++ {
				round := cacheToolRound(turn, 128)
				messages = append(messages, round[:2]...)
				text := fmt.Sprintf("<total_tokens>%d tokens left</total_tokens>", 900-turn)
				messages = append(messages, map[string]any{"role": "system", "content": text}, round[2])
				before, _ := json.Marshal(messages)
				w := experimentRequest(t, h, "candidate-session", "stable instructions", messages, cacheTools())
				if w.Code != 200 {
					t.Fatalf("local handler rejected candidate: %d", w.Code)
				}
				after, _ := json.Marshal(messages)
				if !bytes.Equal(before, after) {
					t.Fatal("source mutated")
				}
				current := parseConvertedPrompt(t, captured)
				if current.instructions != "stable instructions" {
					t.Fatal("dynamic instruction hoisted")
				}
				if turn > 1 && (current.cacheKey != previous.cacheKey || !reflect.DeepEqual(current.tools, previous.tools) || !reflect.DeepEqual(current.items[:len(previous.items)], previous.items)) {
					t.Fatal("prefix or identity changed")
				}
				var item map[string]any
				if err := json.Unmarshal(current.items[len(current.items)-2], &item); err != nil {
					t.Fatal(err)
				}
				if item["role"] != mode || item["content"].([]any)[0].(map[string]any)["text"] != text {
					t.Fatal("role/content/order lost")
				}
				previous = current
			}
		})
	}
}

func TestExperimentDoesNotEquateHTTP200WithAcceptance(t *testing.T) {
	w := httptest.NewRecorder()
	w.WriteHeader(200)
	w.WriteString("data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"AMBER\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	r := evaluateExperiment(w, "SAPPHIRE")
	if !r.ProtocolOK || r.SemanticOK || r.CacheKnown {
		t.Fatal("HTTP success or missing cache misclassified")
	}
	w.WriteString("data: {\"type\":\"error\"}\n\n")
	if evaluateExperiment(w, "AMBER").ProtocolOK {
		t.Fatal("stream error accepted")
	}
}

// Exercise the opt-in runner against a local fake transport, not an LLM.
// This verifies wiring and reporting only; it is not a semantic/cache result.
func TestCacheExperimentTransportHarness(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer synthetic-key" || !strings.HasPrefix(r.Header.Get("X-Request-Id"), "cache-experiment-") {
			t.Error("incorrect test transport request")
			w.WriteHeader(400)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if body["model"] != "gpt-6-luna" || body["stream"] != true || body["messages"] != nil || body["thinking"] != nil || body["prompt_cache_key"] == nil {
			t.Error("Anthropic conversion not applied")
		}
		text := "OK"
		if calls >= 3 && calls <= 8 {
			text = "SAPPHIRE"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"probe\",\"status\":\"in_progress\"}}\n\n")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":%q}\n\n", text)
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"probe\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}],\"usage\":{\"input_tokens\":4096,\"output_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":3072}}}}\n\n", text)
	}))
	defer server.Close()
	t.Setenv("AETHERRELAY_CACHE_LIVE", "1")
	t.Setenv("AETHERRELAY_CACHE_BASE_URL", server.URL)
	t.Setenv("AETHERRELAY_CACHE_API_KEY", "synthetic-key")
	t.Setenv("AETHERRELAY_CACHE_CANDIDATE", "developer")
	TestLiveAnthropicCacheExperiment(t)
	if calls != 12 {
		t.Fatalf("calls=%d want 12", calls)
	}
}

// Opt-in only. Client input ALWAYS enters the real /v1/messages handler here.
// The test executor forwards its converted payload to an existing relay's
// native port; that relay performs real Codex OAuth execution. This extra hop
// must be disclosed and its final upstream archive inspected before promotion.
func TestLiveAnthropicCacheExperiment(t *testing.T) {
	if os.Getenv("AETHERRELAY_CACHE_LIVE") != "1" {
		t.Skip("explicit live opt-in required")
	}
	u, err := url.Parse(os.Getenv("AETHERRELAY_CACHE_BASE_URL"))
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("invalid test endpoint")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")) {
		t.Fatal("HTTPS or loopback tunnel required")
	}
	key := os.Getenv("AETHERRELAY_CACHE_API_KEY")
	if key == "" {
		t.Fatal("API key required via environment")
	}
	mode := os.Getenv("AETHERRELAY_CACHE_CANDIDATE")
	if _, err := experimentProjection(mode); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	runID := fmt.Sprintf("cache-experiment-%d", time.Now().UnixNano())
	calls := 0
	h := experimentHandler(t, mode, codexResponsesExecutorStub{
		complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
			return codexresponses.Result{}, fmt.Errorf("unexpected non-stream execution")
		},
		stream: func(ctx context.Context, converted codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
			calls++
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(u.String(), "/")+"/v1/responses", bytes.NewReader(converted.Body))
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Session-Id", runID)
			req.Header.Set("X-Request-Id", fmt.Sprintf("%s-%s-%d", runID, mode, calls))
			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("test transport failed (endpoint and credentials omitted)")
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
				f := codexresponses.NewFailure(codexresponses.KindInvalidRequest, 0, fmt.Errorf("test upstream HTTP %d; inspect correlated archive", resp.StatusCode))
				f.HTTPStatus = resp.StatusCode
				return f
			}
			if err := started(codexresponses.StreamStart{}); err != nil {
				return err
			}
			scanner := bufio.NewScanner(io.LimitReader(resp.Body, 4<<20))
			scanner.Buffer(make([]byte, 4096), 1<<20)
			for scanner.Scan() {
				if err := emit(append(bytes.Clone(scanner.Bytes()), '\n')); err != nil {
					return err
				}
			}
			return scanner.Err()
		},
	})
	t.Logf("run_id=%s candidate=%s entry=/v1/messages model=gpt-6-luna", runID, mode)
	user := func(text string) map[string]any { return map[string]any{"role": "user", "content": text} }
	sys := func(text string) map[string]any { return map[string]any{"role": "system", "content": text} }
	cases := []struct {
		name, expected string
		messages       []any
		tools          []any
	}{
		{"baseline", "OK", []any{user("Reply exactly OK.")}, nil},
		{"priority", "SAPPHIRE", []any{user("Reply exactly AMBER."), sys("Reply exactly SAPPHIRE.")}, nil},
		{"latest_instruction", "SAPPHIRE", []any{sys("Reply exactly AMBER."), user("Which word?"), sys("Reply exactly SAPPHIRE.")}, nil},
		{"tool_order", "SAPPHIRE", []any{user("Read the value."), map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "probe_call", "name": "probe", "input": map[string]any{}}}}, sys("Reply with the tool value only."), map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "probe_call", "content": "SAPPHIRE"}}}}, []any{map[string]any{"name": "probe", "description": "Synthetic value", "input_schema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}}},
	}
	for _, tc := range cases {
		for repeat := 0; repeat < 2; repeat++ {
			w := experimentRequest(t, h, runID, "Synthetic protocol test. Return one word only.", tc.messages, tc.tools)
			result := evaluateExperiment(w, tc.expected)
			encoded, _ := json.Marshal(result)
			t.Logf("case=%s repeat=%d status=%d result=%s", tc.name, repeat, w.Code, encoded)
			if !result.ProtocolOK || !result.SemanticOK {
				t.Fatal("acceptance gate failed; cache phase not run")
			}
		}
	}
	// A long stable history is placed AFTER the first dynamic instruction so
	// later budget updates distinguish hoisting from append-only candidates.
	var padding strings.Builder
	for i := 0; i < 512; i++ {
		fmt.Fprintf(&padding, "Synthetic archive record %04d: alpha beta gamma delta.\n", i)
	}
	messages := []any{sys("<total_tokens>90000 tokens left</total_tokens>"), user(padding.String() + "\nReply exactly OK.")}
	for turn := 0; turn < 4; turn++ {
		w := experimentRequest(t, h, runID+"-long", "Read synthetic records. Reply exactly OK.", messages, nil)
		result := evaluateExperiment(w, "OK")
		encoded, _ := json.Marshal(result)
		t.Logf("cache_turn=%d result=%s", turn, encoded)
		if !result.ProtocolOK || !result.SemanticOK || !result.CacheKnown || result.Input < 2048 {
			t.Fatal("cache sample not eligible; no improvement claimed")
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": "OK"}, sys(fmt.Sprintf("<total_tokens>%d tokens left</total_tokens>", 89000-turn*1000)), user("Reply exactly OK."))
	}
	t.Log("cache observations only: compare matched runs and upstream archive; no automatic promotion or fixed hit-rate guarantee")
}
