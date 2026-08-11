package biz

import (
	"context"
	"sync/atomic"
	"testing"

	acccommon "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptsearch"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	basebiz "aetherrelay/internal/modules/base/biz"
	upcommon "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestSearchRefreshesInvalidCredentialOnceAndRecordsFinalResult(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquireTextToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireTextTokenResult{AccessToken: "old", Account: accevents.AccountView{ID: "account-1", Proxy: "http://old.invalid"}}, nil)
	})
	accounts.Subscribe(accevents.TopicRefreshTextToken, func(ev event.Event, result event.Result) {
		if ev.Data().(accevents.RefreshTextTokenCommand).AccessToken != "old" {
			t.Fatal("unexpected refresh token")
		}
		result.Set(accevents.RefreshTextTokenResult{AccessToken: "new", Refreshed: true, Account: accevents.AccountView{ID: "account-1", Proxy: "http://new.invalid"}}, nil)
	})
	recorded := make(chan accevents.RecordTextResultCommand, 1)
	accounts.Subscribe(accevents.TopicRecordTextResult, func(ev event.Event, result event.Result) {
		recorded <- ev.Data().(accevents.RecordTextResultCommand)
		result.Set(accevents.RecordTextResultResult{}, nil)
	})

	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	var attempts atomic.Int32
	upstream.Subscribe(upevents.TopicSearch, func(ev event.Event, result event.Result) {
		cmd := ev.Data().(upevents.SearchCommand)
		if attempts.Add(1) == 1 {
			if cmd.AccessToken != "old" || cmd.Proxy != "http://old.invalid" {
				t.Fatalf("first command=%+v", cmd)
			}
			result.Set(upevents.SearchResult{ErrorClass: upevents.ErrClassInvalidToken}, cd.NewError(cd.Unexpected, "expired"))
			return
		}
		if cmd.AccessToken != "new" || cmd.Proxy != "http://new.invalid" || cmd.Query != "today news" {
			t.Fatalf("retry command=%+v", cmd)
		}
		result.Set(upevents.SearchResult{Text: "answer", Sources: []upevents.SearchSource{{URL: "https://example.test"}}}, nil)
	})

	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	result, err := proxy.Search(context.Background(), chatgptsearch.Request{Model: "gpt-search", Query: "today news"})
	if err != nil || result.Text != "answer" || attempts.Load() != 2 || len(result.Sources) != 1 {
		t.Fatalf("result=%+v err=%v attempts=%d", result, err, attempts.Load())
	}
	select {
	case command := <-recorded:
		if command.AccountID != "account-1" || command.Model != "gpt-search" || !command.Success {
			t.Fatalf("record=%+v", command)
		}
	default:
		t.Fatal("final account result was not recorded")
	}
}

func TestSearchFailsOverFromPrepareTLSWithoutReusingProxy(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	acquires := 0
	accounts.Subscribe(accevents.TopicAcquireTextToken, func(ev event.Event, result event.Result) {
		acquires++
		command := ev.Data().(accevents.AcquireTextTokenCommand)
		switch acquires {
		case 1:
			if len(command.Exclude) != 0 {
				t.Fatalf("first acquire exclusion=%v", command.Exclude)
			}
			result.Set(accevents.AcquireTextTokenResult{AccessToken: "token-a", Account: accevents.AccountView{ID: "account-a", Proxy: "http://first:secret@proxy-one.invalid:8080"}}, nil)
		case 2:
			if !containsSearchToken(command.Exclude, "token-a") {
				t.Fatalf("second acquire exclusion=%v", command.Exclude)
			}
			// Same endpoint with a different credential must not be used for
			// recovery from a connection-level failure.
			result.Set(accevents.AcquireTextTokenResult{AccessToken: "token-b", Account: accevents.AccountView{ID: "account-b", Proxy: "http://second:secret@proxy-one.invalid:8080"}}, nil)
		case 3:
			if !containsSearchToken(command.Exclude, "token-a") || !containsSearchToken(command.Exclude, "token-b") {
				t.Fatalf("third acquire exclusion=%v", command.Exclude)
			}
			result.Set(accevents.AcquireTextTokenResult{AccessToken: "token-c", Account: accevents.AccountView{ID: "account-c", Proxy: "http://proxy-two.invalid:8080"}}, nil)
		default:
			t.Fatalf("unexpected acquire %d", acquires)
		}
	})
	recorded := make(chan accevents.RecordTextResultCommand, 2)
	accounts.Subscribe(accevents.TopicRecordTextResult, func(ev event.Event, result event.Result) {
		recorded <- ev.Data().(accevents.RecordTextResultCommand)
		result.Set(accevents.RecordTextResultResult{}, nil)
	})

	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicSearch, func(ev event.Event, result event.Result) {
		command := ev.Data().(upevents.SearchCommand)
		switch command.AccessToken {
		case "token-a":
			result.Set(upevents.SearchResult{ErrorClass: upevents.ErrClassTLS, ErrorOperation: "search_prepare"}, cd.NewError(cd.Unexpected, "EOF"))
		case "token-c":
			result.Set(upevents.SearchResult{Text: "recovered"}, nil)
		default:
			t.Fatalf("unexpected upstream command=%+v", command)
		}
	})

	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	searched, err := proxy.Search(context.Background(), chatgptsearch.Request{Model: "gpt-search", Query: "recover search"})
	if err != nil || searched.Text != "recovered" || acquires != 3 {
		t.Fatalf("result=%+v err=%v acquires=%d", searched, err, acquires)
	}
	first, second := <-recorded, <-recorded
	if first.AccountID != "account-a" || first.Success || first.ErrorClass != "tls" {
		t.Fatalf("first record=%+v", first)
	}
	if second.AccountID != "account-c" || !second.Success || second.ErrorClass != "" {
		t.Fatalf("second record=%+v", second)
	}
}

func TestSearchDoesNotFailOverAfterConversationStageFailure(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	acquires := 0
	accounts.Subscribe(accevents.TopicAcquireTextToken, func(_ event.Event, result event.Result) {
		acquires++
		result.Set(accevents.AcquireTextTokenResult{AccessToken: "token-a", Account: accevents.AccountView{ID: "account-a", Proxy: "http://proxy-one.invalid:8080"}}, nil)
	})
	accounts.Subscribe(accevents.TopicRecordTextResult, func(_ event.Event, result event.Result) {
		result.Set(accevents.RecordTextResultResult{}, nil)
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicSearch, func(_ event.Event, result event.Result) {
		result.Set(upevents.SearchResult{ErrorClass: upevents.ErrClassTLS, ErrorOperation: "search_conversation"}, cd.NewError(cd.Unexpected, "EOF"))
	})

	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	_, err := proxy.Search(context.Background(), chatgptsearch.Request{Model: "gpt-search", Query: "do not duplicate"})
	if err == nil || acquires != 1 {
		t.Fatalf("err=%v acquires=%d", err, acquires)
	}
}

func containsSearchToken(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
