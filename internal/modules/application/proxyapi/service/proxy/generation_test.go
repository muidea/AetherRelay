package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	generation "aetherrelay/internal/pkg/aetherrelaygeneration"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

func TestCodexGenerationSettlementWithoutArchive(t *testing.T) {
	for _, raw := range []string{`{"completion_tokens":100}`, `{"output_tokens":100}`, `{"input_tokens":100}`, `{"output_tokens":0}`} {
		t.Run(raw, func(t *testing.T) {
			store := usage.NewMemoryStore()
			defer store.Close()
			h := &Handler{usageStore: store}
			at := time.Now().Add(-time.Minute)
			if err := store.Start(context.Background(), usage.StartRecord{EventID: "sample", APIKeyID: "key", StartedAt: at}); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			req = req.WithContext(withUsageCompletion(req.Context(), &usageCompletion{}))
			observer := h.codexAttemptObserver(nil, req, "codexoauth")
			if observer == nil {
				t.Fatal("archive-off dropped observer")
			}
			observer(codexresponses.HTTPAttempt{Response: codexresponses.HTTPResponseObservation{FirstOutputAt: at.Add(20 * time.Second), GenerationDuration: 10 * time.Second, GenerationBuffered: true}}, nil)
			tok, ok := usageFromRaw([]byte(raw))
			if !ok {
				t.Fatal("parse usage")
			}
			if !h.completeUsage(req, "sample", "codexoauth", "model", false, 200, 40*time.Second, tok, "success", "", "", nil, 0, nil) {
				t.Fatal("settlement")
			}
			page, err := store.Events(context.Background(), usage.EventFilter{UsageFilter: usage.UsageFilter{AllTime: true}})
			if err != nil || len(page.Events) != 1 {
				t.Fatal(page, err)
			}
			e := page.Events[0]
			if tok.OutputTokensKnown {
				if e.ObservedTPS == nil || *e.ObservedTPS != float64(tok.CompletionTokens)/10 || e.GenerationDurationMS != 10000 || !e.GenerationBuffered {
					t.Fatal(e)
				}
			} else if e.ObservedTPS != nil {
				t.Fatal("missing output usage polluted ObservedTPS", e)
			}
		})
	}
}
func TestGenerationRetryResetAndReadFailure(t *testing.T) {
	completion := &usageCompletion{}
	ctx := withUsageCompletion(context.Background(), completion)
	at := time.Now().Add(-time.Second)
	completion.generation.Output(at)
	completion.generationObserved.FirstOutputAt = at
	completion.generationObserved.Buffered = true
	resetGeneration(ctx)
	if s := completion.generation.Snapshot(time.Now()); !s.FirstOutputAt.IsZero() || !completion.generationObserved.FirstOutputAt.IsZero() || completion.generationObserved.Buffered {
		t.Fatal("previous attempt timing leaked")
	}
	observeGenerationSSE(ctx, []byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`), nil)
	observeGenerationSSE(ctx, nil, context.Canceled)
	s := completion.generation.Snapshot(time.Now().Add(time.Hour))
	if !s.Partial || s.Duration <= 0 || s.Duration > time.Second {
		t.Fatal(s)
	}
}

func TestAnthropicFinalUsagePreservesKnownZeroOutput(t *testing.T) {
	accumulator := &anthropicRawStreamAccumulator{InputTokens: 100, OutputTokensKnown: true}
	accumulator.Content.WriteString("observed text")
	tok := accumulator.FinalizeUsage(nil)
	if tok.CompletionTokens != 0 || !tok.OutputTokensKnown || tok.Estimated {
		t.Fatal("reported zero replaced with estimate", tok)
	}
	missing := &anthropicRawStreamAccumulator{InputTokens: 100}
	missing.Content.WriteString("observed text")
	tok = missing.FinalizeUsage(nil)
	if tok.CompletionTokens <= 0 || !tok.Estimated {
		t.Fatal("missing usage estimate provenance lost", tok)
	}
}

func TestConversionFailuresFreezeGeneration(t *testing.T) {
	for _, mode := range []string{"eof", "read_error", "cancel", "timeout", "mapper", "client_write", "buffered_eof"} {
		t.Run(mode, func(t *testing.T) {
			completion := &usageCompletion{}
			completion.generation.Output(time.Now().Add(-time.Second))
			ctx, cancel := context.WithCancel(withUsageCompletion(context.Background(), completion))
			defer cancel()
			var input io.Reader = strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
			writer := http.ResponseWriter(httptest.NewRecorder())
			mapper := func(_ []byte, _ *textConversionStreamState) ([]map[string]any, error) { return nil, nil }
			switch mode {
			case "read_error":
				input = reviewGenerationErrorReader{}
			case "cancel":
				reader, pipeWriter := io.Pipe()
				defer pipeWriter.Close()
				input = reader
				cancel()
			case "timeout":
				reader, pipeWriter := io.Pipe()
				defer pipeWriter.Close()
				input = reader
			case "mapper":
				mapper = func(_ []byte, _ *textConversionStreamState) ([]map[string]any, error) {
					return nil, errors.New("bad conversion")
				}
			case "client_write":
				writer = &generationErrorWriter{}
				mapper = func(_ []byte, _ *textConversionStreamState) ([]map[string]any, error) {
					return []map[string]any{{"type": "response.output_text.delta", "delta": "hello"}}, nil
				}
			}
			var err error
			if mode == "buffered_eof" {
				_, err = convertSSEReaderContext(ctx, input, mapper, &textConversionStreamState{}, true)
			} else {
				err = serveConvertedSSEWithTimeouts(ctx, writer, input, mapper, &textConversionStreamState{}, true, 10*time.Millisecond, 10*time.Millisecond)
			}
			if err == nil {
				t.Fatal("expected conversion failure")
			}
			at := time.Now()
			a := completion.generation.Snapshot(at)
			b := completion.generation.Snapshot(at.Add(time.Hour))
			if a.FirstOutputAt.IsZero() || !a.Partial || a.Duration != b.Duration {
				t.Fatalf("failure did not freeze timing: %+v / %+v", a, b)
			}
		})
	}
}

type reviewGenerationErrorReader struct{}

func (reviewGenerationErrorReader) Read([]byte) (int, error) {
	return 0, errors.New("upstream read failed")
}

type generationErrorWriter struct{ httptest.ResponseRecorder }

func (*generationErrorWriter) Header() http.Header { return make(http.Header) }
func (*generationErrorWriter) Write([]byte) (int, error) {
	return 0, errors.New("downstream write failed")
}
func (*generationErrorWriter) WriteHeader(int) {}

type generationTextExecutor struct {
	sample      generation.Sample
	fail        bool
	returnDelay time.Duration
}

func (e generationTextExecutor) Complete(context.Context, chatgpttext.Request) (chatgpttext.Result, error) {
	time.Sleep(e.returnDelay)
	result := chatgpttext.Result{Text: "hello", ActualModel: "actual-model", Generation: e.sample}
	if e.fail {
		return result, chatgptfail.New(chatgptfail.KindUpstream, errors.New("read failure"))
	}
	return result, nil
}
func (e generationTextExecutor) Stream(ctx context.Context, r chatgpttext.Request, emit func(chatgpttext.Delta) error) (chatgpttext.Result, error) {
	if err := emit(chatgpttext.Delta{Text: "hello"}); err != nil {
		return chatgpttext.Result{Text: "hello", Generation: e.sample}, err
	}
	return e.Complete(ctx, r)
}
func TestChatGPTBothEndpointsAndModesUseFrozenUpstreamGeneration(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, fail := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/fail=%v", path, stream, fail), func(t *testing.T) {
					store := usage.NewMemoryStore()
					defer store.Close()
					at := time.Now().Add(-time.Second)
					sample := generation.Sample{FirstOutputAt: at, Duration: 20 * time.Millisecond, Partial: fail}
					h := &Handler{usageStore: store, chatGPTText: generationTextExecutor{sample: sample, fail: fail, returnDelay: 25 * time.Millisecond}}
					req := httptest.NewRequest(http.MethodPost, path, nil)
					req = req.WithContext(withUsageCompletion(withUsageEventID(req.Context(), "event"), &usageCompletion{}))
					if err := store.Start(context.Background(), usage.StartRecord{EventID: "event", APIKeyID: "key", StartedAt: at}); err != nil {
						t.Fatal(err)
					}
					body := map[string]any{"model": "test-model", "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "input": "hello"}
					writer := httptest.NewRecorder()
					if path == "/v1/responses" {
						h.handleChatGPTWebResponses(writer, req, at, "chatgptweb", "test-model", stream, body)
					} else {
						h.handleChatGPTWebChatCompletions(writer, req, at, "chatgptweb", "test-model", stream, body)
					}
					page, err := store.Events(context.Background(), usage.EventFilter{UsageFilter: usage.UsageFilter{AllTime: true}})
					if err != nil || len(page.Events) != 1 {
						t.Fatal(page, err)
					}
					e := page.Events[0]
					if e.ObservedTPS == nil || e.GenerationDurationMS != 20 || *e.ObservedTPS != float64(e.OutputTokens)/.02 || !e.Estimated || e.GenerationPartial != fail || e.OutputTokens <= 0 {
						t.Fatalf("frozen sample lost: %+v; response=%s", e, writer.Body.String())
					}
					if fail && e.Outcome == "success" {
						t.Fatal("partial failure recorded as success")
					}
					d, err := store.Dashboard(context.Background(), usage.UsageFilter{AllTime: true})
					if err != nil || d.Summary.ObservedTPSSamples != 1 || len(d.ByAPIKey) != 1 || d.Summary.ObservedTPS == nil || *d.Summary.ObservedTPS != *e.ObservedTPS || d.ByAPIKey[0].ObservedTPS == nil || *d.ByAPIKey[0].ObservedTPS != *e.ObservedTPS {
						t.Fatal(d, err)
					}
				})
			}
		}
	}
}
