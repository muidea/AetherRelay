package biz

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	acccommon "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	basebiz "aetherrelay/internal/modules/base/biz"
	upcommon "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	generation "aetherrelay/internal/pkg/aetherrelaygeneration"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestCompleteMarksOnlyClassifiedInvalidAccount(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquireTextToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireTextTokenResult{AccessToken: "test-token"}, nil)
	})
	accounts.Subscribe(accevents.TopicRefreshTextToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.RefreshTextTokenResult{PermanentFailure: true, ErrorClass: "invalid_grant"}, nil)
	})
	removed := make(chan accevents.RemoveInvalidCommand, 1)
	accounts.Subscribe(accevents.TopicRemoveInvalid, func(ev event.Event, result event.Result) {
		removed <- ev.Data().(accevents.RemoveInvalidCommand)
		result.Set(accevents.RemoveInvalidResult{Removed: true}, nil)
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicCompleteText, func(_ event.Event, result event.Result) {
		result.Set(upevents.CompleteTextResult{ErrorClass: upevents.ErrClassInvalidToken}, cd.NewError(cd.Unexpected, "invalid credential"))
	})

	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	if _, err := proxy.Complete(context.Background(), chatgpttext.Request{Model: "gpt", Messages: []chatgpttext.Message{{Role: "user", Content: "test"}}}); err == nil {
		t.Fatal("invalid upstream account unexpectedly completed")
	}
	select {
	case command := <-removed:
		if command.AccessToken != "test-token" || command.Event != "chat_completion" {
			t.Fatalf("remove command=%+v", command)
		}
	default:
		t.Fatal("invalid account was not marked")
	}
}

func TestCompleteRecordsFinalSuccessfulAccountResult(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquireTextToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireTextTokenResult{AccessToken: "test-token", Account: accevents.AccountView{ID: "account-1"}}, nil)
	})
	recorded := make(chan accevents.RecordTextResultCommand, 1)
	accounts.Subscribe(accevents.TopicRecordTextResult, func(ev event.Event, result event.Result) {
		recorded <- ev.Data().(accevents.RecordTextResultCommand)
		result.Set(accevents.RecordTextResultResult{}, nil)
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicCompleteText, func(_ event.Event, result event.Result) {
		result.Set(upevents.CompleteTextResult{ActualModel: "gpt-actual", Text: "ok"}, nil)
	})

	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	completed, err := proxy.Complete(context.Background(), chatgpttext.Request{Model: "gpt-request", Messages: []chatgpttext.Message{{Role: "user", Content: "test"}}})
	if err != nil || completed.Text != "ok" {
		t.Fatalf("complete result=%+v err=%v", completed, err)
	}
	select {
	case command := <-recorded:
		if command.AccountID != "account-1" || command.Model != "gpt-request" || !command.Success || command.ErrorClass != "" {
			t.Fatalf("record command=%+v", command)
		}
	default:
		t.Fatal("successful account result was not recorded")
	}
}

func TestCompletePropagatesValidatedImagesToUpstream(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquireTextToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireTextTokenResult{AccessToken: "test-token", Account: accevents.AccountView{ID: "account-1"}}, nil)
	})
	accounts.Subscribe(accevents.TopicRecordTextResult, func(_ event.Event, result event.Result) {
		result.Set(accevents.RecordTextResultResult{}, nil)
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicCompleteText, func(ev event.Event, result event.Result) {
		command := ev.Data().(upevents.CompleteTextCommand)
		if len(command.Messages) != 1 || len(command.Messages[0].Images) != 1 || string(command.Messages[0].Images[0]) != "image-bytes" {
			t.Fatalf("command=%+v", command)
		}
		result.Set(upevents.CompleteTextResult{Text: "ok"}, nil)
	})

	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	if _, err := proxy.Complete(context.Background(), chatgpttext.Request{Model: "gpt", Messages: []chatgpttext.Message{{Role: "user", Content: "inspect", Images: [][]byte{[]byte("image-bytes")}}}}); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteRefreshesInvalidOAuthTokenAndRetriesOnce(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquireTextToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireTextTokenResult{AccessToken: "old-token", Account: accevents.AccountView{ID: "account-1", Proxy: "http://old-proxy.invalid:8080"}}, nil)
	})
	var refreshes atomic.Int32
	accounts.Subscribe(accevents.TopicRefreshTextToken, func(ev event.Event, result event.Result) {
		refreshes.Add(1)
		if command := ev.Data().(accevents.RefreshTextTokenCommand); command.AccessToken != "old-token" {
			t.Fatalf("refresh command=%+v", command)
		}
		result.Set(accevents.RefreshTextTokenResult{AccessToken: "new-token", Account: accevents.AccountView{ID: "account-1", Proxy: "http://new-proxy.invalid:8080"}, Refreshed: true}, nil)
	})
	recorded := make(chan accevents.RecordTextResultCommand, 1)
	accounts.Subscribe(accevents.TopicRecordTextResult, func(ev event.Event, result event.Result) {
		recorded <- ev.Data().(accevents.RecordTextResultCommand)
		result.Set(accevents.RecordTextResultResult{}, nil)
	})
	removed := make(chan accevents.RemoveInvalidCommand, 1)
	accounts.Subscribe(accevents.TopicRemoveInvalid, func(ev event.Event, result event.Result) {
		removed <- ev.Data().(accevents.RemoveInvalidCommand)
		result.Set(accevents.RemoveInvalidResult{Removed: true}, nil)
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	var attempts atomic.Int32
	upstream.Subscribe(upevents.TopicCompleteText, func(ev event.Event, result event.Result) {
		attempt := attempts.Add(1)
		command := ev.Data().(upevents.CompleteTextCommand)
		if attempt == 1 {
			if command.AccessToken != "old-token" || command.Proxy != "http://old-proxy.invalid:8080" {
				t.Fatalf("first command=%+v", command)
			}
			result.Set(upevents.CompleteTextResult{ErrorClass: upevents.ErrClassInvalidToken}, cd.NewError(cd.Unexpected, "expired credential"))
			return
		}
		if command.AccessToken != "new-token" || command.Proxy != "http://new-proxy.invalid:8080" {
			t.Fatalf("retry command=%+v", command)
		}
		result.Set(upevents.CompleteTextResult{Text: "recovered"}, nil)
	})

	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	completed, err := proxy.Complete(context.Background(), chatgpttext.Request{Model: "gpt", Messages: []chatgpttext.Message{{Role: "user", Content: "test"}}})
	if err != nil || completed.Text != "recovered" || attempts.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("complete=%+v err=%v attempts=%d refreshes=%d", completed, err, attempts.Load(), refreshes.Load())
	}
	select {
	case command := <-recorded:
		if !command.Success || command.ErrorClass != "" {
			t.Fatalf("final result must be success, got %+v", command)
		}
	default:
		t.Fatal("final retry result was not recorded")
	}
	select {
	case command := <-removed:
		t.Fatalf("recovered token must not be removed: %+v", command)
	default:
	}
}

func TestCompleteKeepsAccountOnTransientOAuthRefreshFailure(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquireTextToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireTextTokenResult{AccessToken: "old-token", Account: accevents.AccountView{ID: "account-1"}}, nil)
	})
	accounts.Subscribe(accevents.TopicRefreshTextToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.RefreshTextTokenResult{ErrorClass: "transport"}, nil)
	})
	removed := make(chan accevents.RemoveInvalidCommand, 1)
	accounts.Subscribe(accevents.TopicRemoveInvalid, func(ev event.Event, result event.Result) {
		removed <- ev.Data().(accevents.RemoveInvalidCommand)
		result.Set(accevents.RemoveInvalidResult{Removed: true}, nil)
	})
	recorded := make(chan accevents.RecordTextResultCommand, 1)
	accounts.Subscribe(accevents.TopicRecordTextResult, func(ev event.Event, result event.Result) {
		recorded <- ev.Data().(accevents.RecordTextResultCommand)
		result.Set(accevents.RecordTextResultResult{}, nil)
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicCompleteText, func(_ event.Event, result event.Result) {
		result.Set(upevents.CompleteTextResult{ErrorClass: upevents.ErrClassInvalidToken}, cd.NewError(cd.Unexpected, "expired credential"))
	})

	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	if _, err := proxy.Complete(context.Background(), chatgpttext.Request{Model: "gpt", Messages: []chatgpttext.Message{{Role: "user", Content: "test"}}}); err == nil {
		t.Fatal("transient refresh failure unexpectedly completed")
	}
	select {
	case command := <-recorded:
		if command.Success || command.ErrorClass != "upstream" {
			t.Fatalf("record command=%+v", command)
		}
	default:
		t.Fatal("transient refresh outcome was not recorded")
	}
	select {
	case command := <-removed:
		t.Fatalf("transient refresh failure must not remove account: %+v", command)
	default:
	}
}

func TestStreamDoesNotRefreshAfterOutput(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquireTextToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireTextTokenResult{AccessToken: "old-token", Account: accevents.AccountView{ID: "account-1"}}, nil)
	})
	var refreshes atomic.Int32
	accounts.Subscribe(accevents.TopicRefreshTextToken, func(_ event.Event, result event.Result) {
		refreshes.Add(1)
		result.Set(accevents.RefreshTextTokenResult{AccessToken: "new-token", Refreshed: true}, nil)
	})
	accounts.Subscribe(accevents.TopicRecordTextResult, func(_ event.Event, result event.Result) {
		result.Set(accevents.RecordTextResultResult{}, nil)
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicStartText, func(_ event.Event, result event.Result) {
		result.Set(upevents.StartTextResult{StreamID: "stream-1"}, nil)
	})
	var pulls atomic.Int32
	upstream.Subscribe(upevents.TopicPullText, func(_ event.Event, result event.Result) {
		if pulls.Add(1) == 1 {
			result.Set(upevents.PullTextResult{Delta: "partial"}, nil)
			return
		}
		result.Set(upevents.PullTextResult{Done: true, ErrorClass: upevents.ErrClassInvalidToken}, nil)
	})
	upstream.Subscribe(upevents.TopicCancelText, func(_ event.Event, result event.Result) {
		result.Set(upevents.CancelTextResult{Cancelled: true}, nil)
	})

	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	var emitted []string
	result, err := proxy.Stream(context.Background(), chatgpttext.Request{Model: "gpt", Messages: []chatgpttext.Message{{Role: "user", Content: "test"}}}, func(delta chatgpttext.Delta) error {
		emitted = append(emitted, delta.Text)
		return nil
	})
	if err == nil || result.Text != "partial" || len(emitted) != 1 || refreshes.Load() != 0 {
		t.Fatalf("result=%+v err=%v emitted=%v refreshes=%d", result, err, emitted, refreshes.Load())
	}
}

func TestChatGPTGenerationSurvivesProjectionAndAccountSettlement(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/failed=%v", stream, failed), func(t *testing.T) {
				hub := event.NewHub(8)
				background := task.NewBackgroundRoutine(8)
				defer hub.Terminate(context.Background())
				defer background.Shutdown(nil)
				sample := generation.Sample{FirstOutputAt: time.Now().Add(-time.Second), Duration: 20 * time.Millisecond, Partial: failed}
				accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
				accounts.Subscribe(accevents.TopicAcquireTextToken, func(_ event.Event, result event.Result) {
					result.Set(accevents.AcquireTextTokenResult{AccessToken: "token", Account: accevents.AccountView{ID: "account"}}, nil)
				})
				accounts.Subscribe(accevents.TopicRecordTextResult, func(_ event.Event, result event.Result) {
					time.Sleep(30 * time.Millisecond)
					result.Set(accevents.RecordTextResultResult{}, nil)
				})
				upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
				upstream.Subscribe(upevents.TopicCompleteText, func(_ event.Event, result event.Result) {
					out := upevents.CompleteTextResult{Text: "hello", ActualModel: "actual", Generation: sample}
					if failed {
						out.ErrorClass = upevents.ErrClassUpstream
						result.Set(out, cd.NewError(cd.Unexpected, "read failure"))
					} else {
						result.Set(out, nil)
					}
				})
				upstream.Subscribe(upevents.TopicStartText, func(_ event.Event, result event.Result) {
					result.Set(upevents.StartTextResult{StreamID: "stream"}, nil)
				})
				var pulls atomic.Int32
				upstream.Subscribe(upevents.TopicPullText, func(_ event.Event, result event.Result) {
					if pulls.Add(1) == 1 {
						result.Set(upevents.PullTextResult{Delta: "hello", Generation: generation.Sample{FirstOutputAt: sample.FirstOutputAt, Partial: true}}, nil)
						return
					}
					out := upevents.PullTextResult{Done: true, Generation: sample}
					if failed {
						out.ErrorClass = upevents.ErrClassUpstream
					}
					result.Set(out, nil)
				})
				upstream.Subscribe(upevents.TopicCancelText, func(_ event.Event, result event.Result) { result.Set(upevents.CancelTextResult{}, nil) })
				proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
				request := chatgpttext.Request{Model: "model", Messages: []chatgpttext.Message{{Role: "user", Content: "hello"}}}
				var out chatgpttext.Result
				var err error
				if stream {
					out, err = proxy.Stream(context.Background(), request, func(chatgpttext.Delta) error { return nil })
				} else {
					out, err = proxy.Complete(context.Background(), request)
				}
				if (err != nil) != failed || out.Text != "hello" || out.Generation != sample {
					t.Fatalf("result=%+v err=%v want=%+v", out, err, sample)
				}
			})
		}
	}
}

func TestChatGPTEmitFailureUsesCancelTimingBeforeAccountSettlement(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)
	sample := generation.Sample{FirstOutputAt: time.Now().Add(-time.Second), Duration: 25 * time.Millisecond, Partial: true}
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicStartText, func(_ event.Event, result event.Result) {
		result.Set(upevents.StartTextResult{StreamID: "stream"}, nil)
	})
	upstream.Subscribe(upevents.TopicPullText, func(_ event.Event, result event.Result) {
		result.Set(upevents.PullTextResult{Delta: "hello", Generation: generation.Sample{FirstOutputAt: sample.FirstOutputAt, Partial: true}}, nil)
	})
	upstream.Subscribe(upevents.TopicCancelText, func(_ event.Event, result event.Result) {
		result.Set(upevents.CancelTextResult{Cancelled: true, Generation: sample}, nil)
	})
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	out, emitted, err := proxy.streamChatGPTTextOnce(context.Background(), "token", "", chatgpttext.Request{Model: "model"}, func(chatgpttext.Delta) error { return errors.New("client write failed") })
	if err == nil || !emitted || out.Text != "hello" || out.Generation != sample {
		t.Fatal(out, emitted, err)
	}
}
