package biz

import (
	"context"
	"testing"

	acccommon "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptimage"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	basebiz "aetherrelay/internal/modules/base/biz"
	upcommon "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	"aetherrelay/internal/pkg/chatgpttokenusage"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestGenerateImageKeepsUsageFromFailedNthUpstreamCall(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(4)
	t.Cleanup(func() {
		background.Shutdown(nil)
		hub.Terminate(context.Background())
	})

	accountObs := event.NewSimpleObserver(acccommon.UnitID, hub)
	accountObs.Subscribe(accevents.TopicAcquireImageToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireImageTokenResult{AccessToken: "token", Account: accevents.AccountView{ID: "account-1", Proxy: "http://image-proxy.invalid:8080"}}, nil)
	})
	accountObs.Subscribe(accevents.TopicReleaseImageSlot, func(_ event.Event, result event.Result) {
		result.Set(accevents.ReleaseImageSlotResult{OK: true}, nil)
	})
	accountObs.Subscribe(accevents.TopicMarkImageResult, func(_ event.Event, result event.Result) {
		result.Set(accevents.MarkImageResultResult{}, nil)
	})

	calls := 0
	upstreamObs := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstreamObs.Subscribe(upevents.TopicGenerateImage, func(ev event.Event, result event.Result) {
		if command := ev.Data().(upevents.GenerateImageCommand); command.Proxy != "http://image-proxy.invalid:8080" {
			t.Fatalf("image proxy=%q", command.Proxy)
		}
		calls++
		if calls == 1 {
			result.Set(upevents.GenerateImageResult{
				Images:         []upevents.ImageOutput{{B64JSON: "first"}},
				Usage:          &tokenusage.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8},
				ConversationID: "conversation-1",
			}, nil)
			return
		}
		result.Set(upevents.GenerateImageResult{
			Usage: &tokenusage.Usage{PromptTokens: 7, CompletionTokens: 11, TotalTokens: 18},
		}, cd.NewError(cd.Unexpected, "upstream failed"))
	})

	p := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	result, err := p.GenerateImage(context.Background(), chatgptimage.Request{
		Prompt: "cat", Model: "gpt-image-2", N: 2, ResponseFormat: "b64_json",
	})
	if err == nil {
		t.Fatal("expected second upstream call to fail")
	}
	if calls != 2 || len(result.Data) != 1 {
		t.Fatalf("calls=%d result=%+v", calls, result)
	}
	if result.Usage == nil || result.Usage.PromptTokens != 10 || result.Usage.CompletionTokens != 16 || result.Usage.TotalTokens != 26 {
		t.Fatalf("failed-call usage was lost: %+v", result.Usage)
	}
	if result.ConversationID != "conversation-1" || result.AccountID != "account-1" {
		t.Fatalf("recovery metadata was lost: %+v", result)
	}
}

func TestGenerateImageRefreshesInvalidOAuthThenRetriesBeforeConversation(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(4)
	t.Cleanup(func() {
		background.Shutdown(nil)
		hub.Terminate(context.Background())
	})
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquireImageToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireImageTokenResult{AccessToken: "old-token", Account: accevents.AccountView{ID: "account-1", Proxy: "http://old.invalid:8080"}}, nil)
	})
	accounts.Subscribe(accevents.TopicReleaseImageSlot, func(_ event.Event, result event.Result) { result.Set(accevents.ReleaseImageSlotResult{OK: true}, nil) })
	refreshes, marks := 0, 0
	accounts.Subscribe(accevents.TopicRefreshTextToken, func(ev event.Event, result event.Result) {
		refreshes++
		if command := ev.Data().(accevents.RefreshTextTokenCommand); command.AccessToken != "old-token" {
			t.Fatalf("refresh token=%q", command.AccessToken)
		}
		result.Set(accevents.RefreshTextTokenResult{AccessToken: "new-token", Account: accevents.AccountView{ID: "account-1", Proxy: "http://new.invalid:8080"}, Refreshed: true}, nil)
	})
	accounts.Subscribe(accevents.TopicMarkImageResult, func(ev event.Event, result event.Result) {
		marks++
		command := ev.Data().(accevents.MarkImageResultCommand)
		if !command.Success || command.Model != "gpt-image-2" || command.ErrorClass != "" {
			t.Fatalf("mark=%+v", command)
		}
		result.Set(accevents.MarkImageResultResult{}, nil)
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	calls := 0
	upstream.Subscribe(upevents.TopicGenerateImage, func(ev event.Event, result event.Result) {
		calls++
		command := ev.Data().(upevents.GenerateImageCommand)
		if calls == 1 {
			if command.AccessToken != "old-token" || command.Proxy != "http://old.invalid:8080" {
				t.Fatalf("first command=%+v", command)
			}
			result.Set(upevents.GenerateImageResult{ErrorClass: upevents.ErrClassInvalidToken}, cd.NewError(cd.Unexpected, "unauthorized"))
			return
		}
		if command.AccessToken != "new-token" || command.Proxy != "http://new.invalid:8080" {
			t.Fatalf("retry command=%+v", command)
		}
		result.Set(upevents.GenerateImageResult{Images: []upevents.ImageOutput{{B64JSON: "aW1hZ2U="}}, ConversationID: "conversation-1"}, nil)
	})
	p := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	result, err := p.GenerateImage(context.Background(), chatgptimage.Request{Prompt: "cat", Model: "gpt-image-2", N: 1, ResponseFormat: "b64_json"})
	if err != nil || calls != 2 || refreshes != 1 || marks != 1 || len(result.Data) != 1 {
		t.Fatalf("result=%+v err=%v calls=%d refreshes=%d marks=%d", result, err, calls, refreshes, marks)
	}
	if result.ConversationID != "conversation-1" || result.AccountID != "account-1" {
		t.Fatalf("result metadata=%+v", result)
	}
}
