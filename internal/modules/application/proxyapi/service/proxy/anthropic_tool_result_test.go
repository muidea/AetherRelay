package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
)

const toolResultPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jM1kAAAAASUVORK5CYII="

func toolResultImageBlock() map[string]any {
	return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": toolResultPNG}}
}

func imageToolContinuation(content []any) map[string]any {
	return map[string]any{"model": "gpt-5.2-codex", "stream": true, "messages": []any{
		map[string]any{"role": "user", "content": "inspect"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call_image", "name": "Read", "input": map[string]any{}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_image", "content": content, "cache_control": map[string]any{"type": "ephemeral"}}}},
	}}
}

func TestCodexImageToolResultReachesExecutor(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[stream], func(t *testing.T) {
			body := imageToolContinuation([]any{toolResultImageBlock()})
			body["stream"] = stream
			// Reproduce the production message index with synthetic text only.
			messages := make([]any, 197, 200)
			for i := range messages {
				messages[i] = map[string]any{"role": "user", "content": "history"}
			}
			body["messages"] = append(messages, body["messages"].([]any)[1:]...)
			body["messages"] = append(body["messages"].([]any), map[string]any{"role": "user", "content": "describe"})
			raw, _ := json.Marshal(body)
			calls := 0
			check := func(req codexresponses.Request) {
				calls++
				var upstream map[string]any
				if err := json.Unmarshal(req.Body, &upstream); err != nil {
					t.Fatal(err)
				}
				items := upstream["input"].([]any)
				output := items[198].(map[string]any)
				if output["type"] != "function_call_output" || output["call_id"] != "call_image" {
					t.Fatalf("wrong tool result: %v", output)
				}
				parts, ok := output["output"].([]any)
				if !ok || len(parts) != 1 {
					t.Fatalf("image was flattened or omitted: %v", output)
				}
				image := parts[0].(map[string]any)
				if image["type"] != "input_image" || image["image_url"] != "data:image/png;base64,"+toolResultPNG {
					t.Fatalf("wrong image mapping: %v", image)
				}
			}
			h, _ := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{
				complete: func(_ context.Context, req codexresponses.Request) (codexresponses.Result, error) {
					check(req)
					return codexresponses.Result{Body: []byte(`{"id":"resp_image","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)}, nil
				},
				stream: func(_ context.Context, req codexresponses.Request, start func(codexresponses.StreamStart) error, emit func([]byte) error) error {
					check(req)
					if err := start(codexresponses.StreamStart{}); err != nil {
						return err
					}
					if err := emit([]byte(`data: {"type":"response.created","response":{"id":"resp_image","model":"gpt-5.2-codex"}}`)); err != nil {
						return err
					}
					return emit([]byte(`data: {"type":"response.completed","response":{"id":"resp_image","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`))
				},
			})
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(raw)))
			r.Header.Set("Authorization", "Bearer test-client-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
		})
	}
}

func TestCodexMixedToolResultPreservesOrderAndFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		body := imageToolContinuation([]any{map[string]any{"type": "text", "text": "before"}, toolResultImageBlock(), map[string]any{"type": "text", "text": "after"}})
		result := body["messages"].([]any)[2].(map[string]any)["content"].([]any)[0].(map[string]any)
		result["is_error"] = failed
		before, _ := json.Marshal(body)
		encoded, _, err := buildCodexResponsesFromAnthropicWithCapability(body, "model", false, config.ConversionCapability{Tools: true})
		if err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(body)
		if string(before) != string(after) {
			t.Fatal("source request mutated")
		}
		var upstream map[string]any
		_ = json.Unmarshal(encoded, &upstream)
		parts := upstream["input"].([]any)[2].(map[string]any)["output"].([]any)
		if failed {
			if parts[0].(map[string]any)["text"] != `{"error":true}` {
				t.Fatal("tool failure marker missing")
			}
			parts = parts[1:]
		}
		if len(parts) != 3 || parts[0].(map[string]any)["text"] != "before" || parts[1].(map[string]any)["type"] != "input_image" || parts[2].(map[string]any)["text"] != "after" {
			t.Fatalf("content order changed: %v", parts)
		}
	}
	// Generic conversion profiles still reject image tool results precisely.
	body := imageToolContinuation([]any{toolResultImageBlock()})
	_, _, err := buildResponsesFromAnthropicWithCapability(body, "model", false, config.ConversionCapability{Tools: true})
	if err == nil {
		t.Fatal("generic conversion unexpectedly accepted images")
	}
	api := conversionAPIError(TransportPlan{}, err)
	if api.Feature != "tool_result.image" || api.Param != "messages[2].content[0].content[0]" {
		t.Fatalf("generic rejection=%+v", api)
	}
}

func TestInvalidImageToolResultHasExactPathAndNoUpstreamAttempt(t *testing.T) {
	for _, tc := range []struct {
		name, path, feature string
		block               map[string]any
	}{
		{"bad base64", ".source.data", "tool_result.image", map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "private-invalid-base64"}}},
		{"wrong media type", ".source.media_type", "tool_result.image", map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": toolResultPNG}}},
		{"URL", ".source.type", "tool_result.image", map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://private.invalid/image.png"}}},
		{"document", ".type", "tool_result.content.type", map[string]any{"type": "document", "source": map[string]any{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := imageToolContinuation([]any{tc.block})
			raw, _ := json.Marshal(body)
			calls := 0
			h, root := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{stream: func(context.Context, codexresponses.Request, func(codexresponses.StreamStart) error, func([]byte) error) error {
				calls++
				return nil
			}})
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(raw)))
			r.Header.Set("Authorization", "Bearer test-client-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			path := "messages[2].content[0].content[0]" + tc.path
			if w.Code != 400 || calls != 0 || !strings.Contains(w.Body.String(), path) || !strings.Contains(w.Body.String(), tc.feature) || strings.Contains(w.Body.String(), "private") {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
			meta, err := os.ReadFile(filepath.Join(root, "test-client", "000001", "metadata.json"))
			if err != nil {
				t.Fatal(err)
			}
			var archived map[string]any
			_ = json.Unmarshal(meta, &archived)
			if archived["conversion_error_path"] != path || !reflect.DeepEqual(archived["unsupported_features"], []any{tc.feature}) {
				t.Fatalf("metadata=%s", meta)
			}
			if events := usageEvents(t, h.usageStore); len(events) != 1 || events[0].HTTPStatus != 400 || events[0].ErrorCode != "conversion_unsupported" || events[0].UpstreamStatus != 0 {
				t.Fatalf("usage=%v", events)
			}
		})
	}
}

func TestImageToolResultKeepsAggregateByteBudget(t *testing.T) {
	body := imageToolContinuation([]any{map[string]any{"type": "text", "text": strings.Repeat("x", maxConversionToolArgumentBytes)}, toolResultImageBlock()})
	if _, _, err := buildCodexResponsesFromAnthropicWithCapability(body, "model", false, config.ConversionCapability{Tools: true}); err == nil {
		t.Fatal("mixed output exceeded tool-result byte budget")
	}
}
