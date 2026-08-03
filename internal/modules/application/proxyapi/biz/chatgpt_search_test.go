package biz

import (
	"context"
	"sync/atomic"
	"testing"

	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptsearch"
	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	basebiz "ai-proxy/internal/modules/base/biz"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
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
