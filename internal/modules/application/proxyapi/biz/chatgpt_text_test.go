package biz

import (
	"context"
	"testing"

	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgpttext"
	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	basebiz "ai-proxy/internal/modules/base/biz"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
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
