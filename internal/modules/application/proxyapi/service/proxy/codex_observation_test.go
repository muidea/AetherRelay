package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	archive "aetherrelay/internal/pkg/aetherrelayarchive"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
	"github.com/gorilla/websocket"
)

const websocketTimingFixture = `{"type":"responsesapi.websocket_timing","timing_scope":"logical_turn","response_id":"resp_test","timing_metrics":{"total_turn_time_s":2.285444984,"num_engine_calls":1,"engine_total_prompt_tokens_total":99031,"num_sampled_tokens_total":74,"critical_path":{"coverage":"complete","boundary_type":"actionable_output_item_done","future":[null,{"nested":true}]}}}`

func TestNativeWebsocketObservationsPreserveFramesAndSeparateTurnUsage(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint(fallback), func(t *testing.T) {
			store := usage.NewMemoryStore()
			defer store.Close()
			turn, index := 0, 0
			executor := codexResponsesExecutorStub{
				wsOpen: func(context.Context, codexresponses.WebsocketOpenRequest) (codexresponses.WebsocketOpenResult, error) {
					return codexresponses.WebsocketOpenResult{SessionID: "observations", HTTPFallback: fallback}, nil
				},
				wsSend: func(context.Context, string, []byte) error { turn++; index = 0; return nil },
				wsPull: func(context.Context, string) ([]byte, bool, error) {
					index++
					if index == 1 {
						return []byte(websocketTimingFixture), false, nil
					}
					if turn == 3 {
						return nil, true, nil
					}
					if index == 2 {
						return []byte(`{"type":"response.future_business","payload":{"value":true}}`), false, nil
					}
					reported := `,"usage":{"input_tokens":3,"output_tokens":0}`
					if turn == 2 {
						reported = ""
					}
					return []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]%s}}`, turn, reported)), false, nil
				},
				wsClose: func(context.Context, string) {},
			}
			done := make(chan struct{})
			h := newCodexResponsesHandler(t, store, executor)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); h.ServeHTTP(w, r) }))
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer test-client-key"}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			for n := 1; n <= 3; n++ {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.2-codex","input":"hello"}`)); err != nil {
					t.Fatal(err)
				}
				if n == 3 {
					// Deferred telemetry must be delivered before the failure frame.
					for range 2 {
						_, frame, err := conn.ReadMessage()
						if err != nil {
							t.Fatal(err)
						}
						if strings.Contains(string(frame), "response.completed") {
							t.Fatal("telemetry-only turn succeeded")
						}
					}
					continue
				}
				for i := 0; i < 3; i++ {
					_, frame, err := conn.ReadMessage()
					if err != nil {
						t.Fatal(err)
					}
					if i == 0 && !bytes.Equal(frame, []byte(websocketTimingFixture)) || i == 1 && !strings.Contains(string(frame), "response.future_business") || i == 2 && !strings.Contains(string(frame), fmt.Sprintf(`"id":"resp_%d"`, n)) {
						t.Fatalf("native frame changed: %s", frame)
					}
				}
			}
			_ = conn.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("WS handler did not finish")
			}
			events := usageEvents(t, store)
			if len(events) != 3 {
				t.Fatalf("turns not settled separately: %+v", events)
			}
			known, success, failures := 0, 0, 0
			for _, e := range events {
				if e.OutputTokensKnown {
					known++
					if e.InputTokens != 3 || e.OutputTokens != 0 {
						t.Fatal(e)
					}
				}
				if e.Outcome == "success" {
					success++
				} else {
					failures++
				}
				if e.Estimated || e.TPS != nil && !e.OutputTokensKnown {
					t.Fatalf("usage fabricated from telemetry: %+v", e)
				}
			}
			if known != 1 || success != 2 || failures != 1 {
				t.Fatalf("usage leaked between turns: %+v", events)
			}
		})
	}
}

func TestCodexAncillaryPayloadCannotChangeBusinessState(t *testing.T) {
	for _, typ := range []string{"response.metadata", "codex.response.metadata", "codex.rate_limits", "responsesapi.websocket_timing"} {
		payload := []byte(fmt.Sprintf(`{"type":%q,"delta":"not output","response":{"status":"completed","usage":{"input_tokens":99,"output_tokens":74}},"usage":{"output_tokens":74},"error":{"code":"rate_limit"},"future":{"nested":[null,true]}}`, typ))
		anthropic := &textConversionStreamState{}
		chat := &codexChatStreamState{}
		for range 2 { // Repeated observations produce one fixed ignored feature.
			out, err := codexResponsesEventToAnthropic(projectionCapability)(payload, anthropic)
			chunks, chatErr := codexResponsesEventToChat(payload, chat)
			if err != nil || chatErr != nil || len(out) != 0 || len(chunks) != 0 || anthropic.Completed || chat.Done || chat.OutputEvidence || anthropic.tokenUsage().Known || chat.Usage.Known {
				t.Fatalf("%s mutated response: anthropic=%+v chat=%+v errors=%v/%v", typ, anthropic, chat, err, chatErr)
			}
		}
		if len(anthropic.IgnoredFeatures) != 1 || len(chat.IgnoredFeatures) != 1 {
			t.Fatal("ignored features were not bounded/deduplicated")
		}
		if usage := codexWebsocketUsage(payload, tokenUsage{}); usage.Known || usage.OutputTokensKnown {
			t.Fatalf("native WS used observation tokens: %+v", usage)
		}
		if terminal, _ := codexWebsocketTerminalOutcome(payload); terminal || !codexWebsocketNeutralPrelude(payload) {
			t.Fatal("ancillary event completed WS or counted as business output")
		}
	}
}

func TestCodexAncillaryEventsAcrossHTTPInterfaces(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, tool := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/tool=%t", path, stream, tool), func(t *testing.T) {
					item := projectionMessage("", "hello")
					delta := `{"type":"response.output_text.delta","output_index":0,"delta":"hello"}`
					if tool {
						item = map[string]any{"type": "function_call", "call_id": "call_test", "name": "Edit", "arguments": `{}`}
						delta = `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`
					}
					response := projectionResponse(item)
					executor := codexResponsesExecutorStub{
						complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
							return codexresponses.Result{Body: projectionJSON(response)}, nil
						},
						stream: func(_ context.Context, _ codexresponses.Request, start func(codexresponses.StreamStart) error, emit func([]byte) error) error {
							if err := start(codexresponses.StreamStart{}); err != nil {
								return err
							}
							events := [][]byte{
								[]byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":25}}}`),
								[]byte(`{"type":"response.created","response":{"id":"resp_test","model":"gpt-5.2-codex"}}`),
								projectionJSON(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item}),
								[]byte(delta),
							}
							if tool {
								events = append(events, []byte(`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{}"}`))
							}
							events = append(events, projectionJSON(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}), []byte(`{"type":"response.metadata","metadata":{"future":[null,true]}}`), []byte(websocketTimingFixture), projectionJSON(map[string]any{"type": "response.completed", "response": response}))
							for _, event := range events {
								if err := emit(append(append([]byte("data: "), event...), '\n', '\n')); err != nil {
									return err
								}
							}
							return nil
						},
					}
					h, root := newArchivedCodexResponsesHandler(t, executor)
					recorder, err := archive.NewRecorderOptions(root, archive.RecorderOptions{MaxRounds: 10, ScopeByAPIKey: true, FullContent: true})
					if err != nil {
						t.Fatal(err)
					}
					h.interactionRecorder = recorder
					body := fmt.Sprintf(`{"model":"gpt-5.2-codex","stream":%t,"max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`, stream)
					if path == "/v1/responses" {
						body = fmt.Sprintf(`{"model":"gpt-5.2-codex","stream":%t,"input":"hello"}`, stream)
					}
					r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
					r.Header.Set("Authorization", "Bearer test-client-key")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					events := usageEvents(t, h.usageStore)
					if w.Code != 200 || len(events) != 1 || events[0].Outcome != "success" || events[0].InputTokens != 100 || events[0].OutputTokens != 12 || !events[0].OutputTokensKnown {
						t.Fatalf("response=%d %s usage=%+v", w.Code, w.Body.String(), events)
					}
					if stream && path != "/v1/responses" {
						if !slices.Contains(events[0].IgnoredFeatures, "websocket_timing") || strings.Contains(w.Body.String(), "timing_metrics") {
							t.Fatal("projection leaked/lost timing observation")
						}
						if path == "/v1/messages" && !strings.Contains(w.Body.String(), "event: message_stop") || path == "/v1/chat/completions" && !strings.Contains(w.Body.String(), "data: [DONE]") {
							t.Fatal("missing client completion")
						}
						raw, err := os.ReadFile(filepath.Join(root, "test-client", "000001", "upstream_response_001.projection.sse"))
						if err != nil || !bytes.Contains(raw, []byte(websocketTimingFixture)) || !bytes.Contains(raw, []byte(`"response.completed"`)) {
							t.Fatalf("projection archive lost timing or terminal: %v", err)
						}
					}
					if stream && path == "/v1/responses" && !strings.Contains(w.Body.String(), websocketTimingFixture) {
						t.Fatal("native Responses lost timing")
					}
				})
			}
		}
	}
}

func TestCodexConvertedStreamsDoNotCompleteOnTelemetryOrUnknownBusiness(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
		for _, mode := range []string{"only_timing", "output_then_eof", "unknown_business", "failed"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				h, _ := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{stream: func(_ context.Context, _ codexresponses.Request, _ func(codexresponses.StreamStart) error, emit func([]byte) error) error {
					if mode != "only_timing" {
						for _, event := range []string{`{"type":"response.created","response":{"id":"resp_test"}}`, `{"type":"response.output_text.delta","delta":"hello"}`} {
							if err := emit([]byte("data: " + event)); err != nil {
								return err
							}
						}
					}
					if err := emit([]byte("data: " + websocketTimingFixture)); err != nil {
						return err
					}
					if mode == "unknown_business" || mode == "failed" {
						typ := "response.future_business"
						if mode == "failed" {
							typ = "response.failed"
						}
						return emit([]byte(fmt.Sprintf(`data: {"type":%q}`, typ)))
					}
					return nil
				}})
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-5.2-codex","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`))
				r.Header.Set("Authorization", "Bearer test-client-key")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				e := usageEvents(t, h.usageStore)[0]
				if e.Outcome == "success" || e.OutputTokensKnown || strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), "message_stop") {
					t.Fatalf("false success: %+v body=%s", e, w.Body.String())
				}
				if mode == "only_timing" && w.Code != 502 || mode != "only_timing" && (w.Code != 200 || !strings.Contains(w.Body.String(), `"error"`)) {
					t.Fatalf("missing failure terminal: %d %s", w.Code, w.Body.String())
				}
				if mode == "unknown_business" && e.Outcome != "conversion" {
					t.Fatalf("business conversion error misclassified: %+v", e)
				}
			})
		}
	}
}

func TestCodexConvertedFailureKeepsTypedClassificationAndUsage(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			h, _ := newArchivedCodexResponsesHandler(t, codexResponsesExecutorStub{stream: func(_ context.Context, _ codexresponses.Request, _ func(codexresponses.StreamStart) error, emit func([]byte) error) error {
				for _, payload := range []string{
					`{"type":"response.created","response":{"id":"resp_failed"}}`,
					`{"type":"response.output_text.delta","delta":"partial"}`,
					websocketTimingFixture,
					`{"type":"response.failed","response":{"usage":{"input_tokens":3,"output_tokens":2},"error":{"code":"rate_limit_exceeded"}}}`,
				} {
					if err := emit([]byte("data: " + payload)); err != nil {
						return err
					}
				}
				return codexresponses.NewFailure(codexresponses.KindRateLimit, 30, fmt.Errorf("upstream rate limit"))
			}})
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-5.2-codex","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`))
			r.Header.Set("Authorization", "Bearer test-client-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			e := usageEvents(t, h.usageStore)[0]
			if e.ErrorCode != "rate_limit" || e.Outcome == "conversion" || e.Outcome == "success" || e.InputTokens != 3 || e.OutputTokens != 2 || !e.OutputTokensKnown || !strings.Contains(w.Body.String(), `"error"`) {
				t.Fatalf("typed failure or actual usage lost: %+v body=%s", e, w.Body.String())
			}
		})
	}
}
