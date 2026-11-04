package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acccommon "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	proxybiz "aetherrelay/internal/modules/application/proxyapi/biz"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	proxyhttp "aetherrelay/internal/modules/application/proxyapi/service/proxy"
	basebiz "aetherrelay/internal/modules/base/biz"
	upclient "aetherrelay/internal/modules/blocks/chatgptwebupstream/internal/client"
	upcommon "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func integrationSnapshot(text string) string {
	return fmt.Sprintf("data: {\"conversation_id\":\"c\",\"message\":{\"id\":\"a\",\"author\":{\"role\":\"assistant\"},\"content\":{\"parts\":[%q]}}}\n\n", text)
}

// Real local HTTP adapter -> production Executor -> EventHub owner -> real
// client/parser. Only the upstream HTTP server and account selection are fake.
func TestTextCompletionRealOwnerExecutorAndHTTP(t *testing.T) {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, "")
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, state := range []string{"complete", "invalid_business_json", "eof", "malformed", "rewrite", "budget", "malformed_first", "budget_first", "eof_empty", "model_length", "invalid_request"} {
				t.Run(fmt.Sprintf("%s/%v/%s", path, stream, state), func(t *testing.T) {
					text := "AB"
					if state == "invalid_business_json" {
						text = `{"a":`
					}
					wire := integrationSnapshot(text)
					switch state {
					case "complete", "invalid_business_json":
						wire += "data: [DONE]\n\n"
					case "malformed":
						wire += "data: {\"message\":\n\ndata: [DONE]\n\n"
					case "rewrite":
						wire += integrationSnapshot("AC") + "data: [DONE]\n\n"
					case "model_length":
						wire = strings.Replace(wire, `"content":`, `"metadata":{"finish_details":{"type":"max_tokens"}},"content":`, 1) + "data: [DONE]\n\n"
					case "malformed_first":
						wire = "data: {\"message\":\n\ndata: [DONE]\n\n"
					case "budget_first":
						wire = integrationSnapshot(strings.Repeat("x", (256<<10)+1)) + "data: [DONE]\n\n"
					case "eof_empty":
						wire = "data: {\"conversation_id\":\"c\"}\n\n"
					case "budget":
						wire += integrationSnapshot(strings.Repeat("x", (256<<10)+1)) + "data: [DONE]\n\n"
					}
					var generations, acquires, refreshes, feedback, failedFeedback atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/":
							fmt.Fprint(w, "<html></html>")
						case "/backend-api/sentinel/chat-requirements/prepare":
							fmt.Fprint(w, `{"prepare_token":"prepare"}`)
						case "/backend-api/sentinel/chat-requirements/finalize":
							fmt.Fprint(w, `{"token":"requirements","so_token":"so"}`)
						case "/backend-api/conversation":
							generations.Add(1)
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, wire)
						default:
							http.NotFound(w, r)
						}
					}))
					defer server.Close()
					hub := event.NewHub(16)
					background := task.NewBackgroundRoutine(8)
					defer hub.Terminate(context.Background())
					defer background.Shutdown(nil)
					upstream, err := New(context.Background(), hub, background)
					if err != nil {
						t.Fatal(err)
					}
					defer upstream.Teardown(context.Background())
					upstream.newTextClient = func(cfg upclient.Config) (*upclient.Client, error) {
						cfg.BaseURL = server.URL
						return upclient.New(cfg)
					}
					accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
					accounts.Subscribe(accevents.TopicAcquireTextToken, func(_ event.Event, r event.Result) {
						acquires.Add(1)
						r.Set(accevents.AcquireTextTokenResult{AccessToken: "local-test", Account: accevents.AccountView{ID: "account", DeviceID: "12345678-1234-4234-8234-123456789abc"}}, nil)
					})
					accounts.Subscribe(accevents.TopicRefreshTextToken, func(_ event.Event, r event.Result) { refreshes.Add(1); r.Set(accevents.RefreshTextTokenResult{}, nil) })
					accounts.Subscribe(accevents.TopicRecordTextResult, func(ev event.Event, r event.Result) {
						if ev.Data().(accevents.RecordTextResultCommand).Success {
							feedback.Add(1)
						} else {
							failedFeedback.Add(1)
						}
						r.Set(accevents.RecordTextResultResult{}, nil)
					})
					executor := &proxybiz.Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
					cfg := config.Config{Providers: map[string]config.Provider{}, ModelMetadata: map[string]config.ModelMetadata{}}
					store := usage.NewMemoryStore()
					handler := proxyhttp.NewHandler(cfg, store, nil, nil).WithChatGPTTextExecutor(executor)
					handler.ReplaceEffectiveCatalog(effectivecatalog.Build(cfg, 1, 1, []effectivecatalog.PoolModel{{ID: "gpt-5"}}, "2026-11-04T00:00:00Z"))
					body := fmt.Sprintf(`{"model":"gpt-5","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)
					if path == "/v1/responses" {
						body = fmt.Sprintf(`{"model":"gpt-5","stream":%v,"input":"hi"}`, stream)
					}
					if state == "invalid_request" {
						messages := strings.TrimSuffix(strings.Repeat(`{"role":"user","content":"hi"},`, 65), ",")
						if path == "/v1/responses" {
							body = fmt.Sprintf(`{"model":"gpt-5","stream":%v,"input":[%s]}`, stream, messages)
						} else {
							body = fmt.Sprintf(`{"model":"gpt-5","stream":%v,"messages":[%s]}`, stream, messages)
						}
					}
					req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
					req.Header.Set("Authorization", "Bearer test-client-key")
					req.Header.Set("Content-Type", "application/json")
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, req)
					success := state == "complete" || state == "invalid_business_json"
					payload := response.Body.String()
					if success {
						if response.Code != 200 || feedback.Load() != 1 {
							t.Fatal(response.Code, payload, feedback.Load())
						}
						if stream && path == "/v1/chat/completions" && !strings.Contains(payload, `"finish_reason":"stop"`) {
							t.Fatal(payload)
						}
					} else {
						expectedStatus := 502
						if state == "invalid_request" {
							expectedStatus = 400
						}
						if !stream && response.Code != expectedStatus {
							t.Fatal(response.Code, payload)
						}
						if strings.Contains(payload, "[DONE]") || strings.Contains(payload, "response.completed") || strings.Contains(payload, `"finish_reason":"stop"`) || feedback.Load() != 0 {
							t.Fatal("false success", payload, feedback.Load())
						}
						if stream && !strings.Contains(payload, `"error"`) {
							t.Fatal(payload)
						}
					}
					expectedGenerations := int32(1)
					if state == "invalid_request" {
						expectedGenerations = 0
					}
					if generations.Load() != expectedGenerations || acquires.Load() != 1 || refreshes.Load() != 0 || failedFeedback.Load() != 0 {
						t.Fatal("replayed", generations.Load(), acquires.Load(), refreshes.Load())
					}
					page, e := store.Events(context.Background(), usage.EventFilter{PageSize: 10})
					if e != nil || len(page.Events) != 1 || (page.Events[0].Outcome == "success") != success {
						t.Fatal(page, e)
					}
					upstream.streamMu.Lock()
					leaked := len(upstream.streams)
					upstream.streamMu.Unlock()
					if leaked != 0 {
						t.Fatal("stream leaked", leaked)
					}
				})
			}
		}
	}
}

func TestTextOwnerConcurrentTerminalCancellationAndBackpressure(t *testing.T) {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, "")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, "<html></html>")
		case "/backend-api/sentinel/chat-requirements/prepare":
			fmt.Fprint(w, `{"prepare_token":"prepare"}`)
		case "/backend-api/sentinel/chat-requirements/finalize":
			fmt.Fprint(w, `{"token":"requirements"}`)
		case "/backend-api/conversation":
			var payload struct {
				ConversationID string `json:"conversation_id"`
			}
			if json.NewDecoder(r.Body).Decode(&payload) != nil {
				w.WriteHeader(400)
				return
			}
			id := payload.ConversationID
			w.Header().Set("Content-Type", "text/event-stream")
			count := 80
			if id == "cancel" {
				count = 1
			}
			for n := 1; n <= count; n++ {
				snapshot := strings.ReplaceAll(integrationSnapshot(strings.Repeat(id, n)), `"conversation_id":"c"`, `"conversation_id":"`+id+`"`)
				fmt.Fprint(w, snapshot)
			}
			w.(http.Flusher).Flush()
			if id == "cancel" {
				<-r.Context().Done()
				return
			}
			if id == "complete" {
				fmt.Fprint(w, "data: [DONE]\n\n")
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)
	upstream, err := New(context.Background(), hub, background)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Teardown(context.Background())
	upstream.newTextClient = func(cfg upclient.Config) (*upclient.Client, error) {
		cfg.BaseURL = server.URL
		return upclient.New(cfg)
	}
	var wg sync.WaitGroup
	for _, id := range []string{"complete", "eof", "cancel"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			value, e := upstream.SendEvent(event.NewEventWithContext(upevents.TopicStartText, "test", upcommon.UnitID, nil, ctx, upevents.StartTextCommand{AccessToken: "local", DeviceID: "12345678-1234-4234-8234-123456789abc", ConversationID: id, Messages: []upevents.TextMessage{{Role: "user", Content: "hi"}}})).Get()
			if e != nil {
				t.Error(e)
				return
			}
			streamID := value.(upevents.StartTextResult).StreamID
			// Let successful/EOF streams fill their bounded queue before consumption.
			time.Sleep(30 * time.Millisecond)
			var text strings.Builder
			for n := 0; n < 100; n++ {
				value, e = upstream.SendEvent(event.NewEvent(upevents.TopicPullText, "test", upcommon.UnitID, nil, upevents.PullTextCommand{StreamID: streamID, TimeoutMillis: 500})).Get()
				if e != nil {
					t.Error(id, e)
					return
				}
				update := value.(upevents.PullTextResult)
				text.WriteString(update.Delta)
				if id == "cancel" && update.Delta != "" {
					cancel()
				}
				if update.Done {
					if update.ConversationID != id {
						t.Error("crossed stream identity", id, update)
						return
					}
					if id == "complete" {
						if update.ErrorClass != "" || !update.Completion.Valid(text.String()) || text.String() != strings.Repeat(id, 80) {
							t.Error(id, update, text.String())
						}
					} else if update.ErrorClass == "" || update.Completion.Completed {
						t.Error("false success", id, update)
					}
					if id == "cancel" && update.ErrorClass != "cancelled" {
						t.Error("lost cancellation", update)
					}
					return
				}
			}
			t.Error("stream did not terminate", id)
		}(id)
	}
	wg.Wait()
	upstream.streamMu.Lock()
	leaked := len(upstream.streams)
	upstream.streamMu.Unlock()
	if leaked != 0 {
		t.Fatal("streams leaked", leaked)
	}
}
