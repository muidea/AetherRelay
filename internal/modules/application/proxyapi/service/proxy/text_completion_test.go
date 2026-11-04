package proxy

import (
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	archive "aetherrelay/internal/pkg/aetherrelayarchive"
	completion "aetherrelay/internal/pkg/aetherrelaycompletion"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	metrics "aetherrelay/internal/pkg/aetherrelaymetrics"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func textEvidence(text string) completion.Text {
	return completion.Text{Completed: true, Reason: completion.Stop, Terminal: "done", TextBytes: len(text), OutputBytes: len(text), TextSHA256: completion.Hash(text)}
}

func TestTextCompletionResponsesRejectsPartialAndUnknown(t *testing.T) {
	for _, path := range []string{"/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, state := range []string{"complete", "unknown", "eof", "protocol", "budget", "mismatched_text"} {
				for _, partial := range []bool{false, true} {
					if state == "complete" && !partial {
						continue
					}
					t.Run(fmt.Sprintf("%s/%v/%s/partial=%v", path, stream, state, partial), func(t *testing.T) {
						store := usage.NewMemoryStore()
						execute := func() (chatgpttext.Result, error) {
							text := ""
							if partial {
								text = "AB"
							}
							out := chatgpttext.Result{Text: text, ActualModel: "gpt-5"}
							switch state {
							case "complete":
								out.Completion = textEvidence(text)
								return out, nil
							case "unknown":
								return out, nil
							case "mismatched_text":
								out.Text = "AC"
								out.Completion = textEvidence("AC")
								return out, nil
							default:
								kind := chatgptfail.KindTextIncomplete
								if state == "protocol" {
									kind = chatgptfail.KindTextProtocol
								}
								if state == "budget" {
									kind = chatgptfail.KindTextBudget
								}
								return out, chatgptfail.New(kind, errors.New("text completion failed"))
							}
						}
						exec := chatGPTTextExecutorStub{complete: func(context.Context, chatgpttext.Request) (chatgpttext.Result, error) { return execute() }, stream: func(_ context.Context, _ chatgpttext.Request, emit func(chatgpttext.Delta) error) (chatgpttext.Result, error) {
							if partial {
								if err := emit(chatgpttext.Delta{Text: "AB"}); err != nil {
									return chatgpttext.Result{}, err
								}
							}
							return execute()
						}}
						cfg := mustHandlerConfig(config.Config{InteractionDir: t.TempDir(), ArchiveInteractions: true, ArchiveFullContent: true})
						recorder, err := archive.NewRecorderOptions(cfg.InteractionDir, archive.RecorderOptions{MaxRounds: 10, ScopeByAPIKey: true})
						if err != nil {
							t.Fatal(err)
						}
						registry := metrics.NewRegistry()
						h := NewHandler(cfg, store, recorder, registry).WithChatGPTTextExecutor(exec)
						h.ReplaceEffectiveCatalog(effectivecatalog.Build(cfg, 1, 1, []effectivecatalog.PoolModel{{ID: "gpt-5"}}, "2026-11-04T00:00:00Z"))
						body := fmt.Sprintf(`{"model":"gpt-5","stream":%v,"input":"hi"}`, stream)
						req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
						req.Header.Set("Authorization", "Bearer test-client-key")
						req.Header.Set("Content-Type", "application/json")
						response := httptest.NewRecorder()
						h.ServeHTTP(response, req)
						success := state == "complete" || !stream && state == "mismatched_text"
						payload := response.Body.String()
						if success {
							if response.Code != 200 {
								t.Fatal(response.Code, payload)
							}
						} else {
							if strings.Contains(payload, "[DONE]") || strings.Contains(payload, "response.completed") || strings.Contains(payload, `"finish_reason":"stop"`) || strings.Contains(payload, `"status":"completed"`) {
								t.Fatal("false success", payload)
							}
							if stream && partial {
								if response.Code != 200 || !strings.Contains(payload, `"error"`) {
									t.Fatal(response.Code, payload)
								}
								if !strings.Contains(payload, "response.failed") {
									t.Fatal(payload)
								}
							} else if response.Code < 400 {
								t.Fatal(response.Code, payload)
							}
						}
						events := usageEvents(t, store)
						if len(events) != 1 || (events[0].Outcome == "success") != success {
							t.Fatalf("success=%v events=%+v payload=%s", success, events, payload)
						}
						paths, err := filepath.Glob(filepath.Join(cfg.InteractionDir, "*", "*", "metadata.json"))
						if err != nil || len(paths) != 1 {
							t.Fatalf("archive paths=%v err=%v", paths, err)
						}
						raw, err := os.ReadFile(paths[0])
						if err != nil {
							t.Fatal(err)
						}
						var metadata archive.Metadata
						if err := json.Unmarshal(raw, &metadata); err != nil {
							t.Fatal(err)
						}
						if metadata.Outcome != events[0].Outcome || metadata.ErrorCode != events[0].ErrorCode {
							t.Fatalf("archive=%s usage=%+v", raw, events[0])
						}
						stats := registry.Snapshot()
						if stats.Requests.Total != 1 || stats.Requests.ByOutcome[events[0].Outcome] != 1 {
							t.Fatalf("metrics=%+v usage=%+v", stats.Requests, events[0])
						}
					})
				}
			}
		}
	}
}

func TestTextFailureDiagnosticsExcludePrivateBodies(t *testing.T) {
	const private = "private upstream body or credential"
	original := chatgptfail.New(chatgptfail.KindUpstream, errors.New(private))
	failure := streamFailFromChatGPTErr(original)
	var output bytes.Buffer
	slog.New(slog.NewTextHandler(&output, nil)).Error("request failed", "error", failure)
	if strings.Contains(output.String(), private) || !strings.Contains(output.String(), "upstream") {
		t.Fatal(output.String())
	}
	var typed *chatgptfail.Failure
	if !errors.As(failure, &typed) || typed != original {
		t.Fatal("lost typed cause")
	}
}
