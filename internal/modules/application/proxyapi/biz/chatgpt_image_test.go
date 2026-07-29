package biz

import (
	"context"
	"testing"

	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptimage"
	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	basebiz "ai-proxy/internal/modules/base/biz"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
	"ai-proxy/internal/pkg/chatgpttokenusage"
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
		result.Set(accevents.AcquireImageTokenResult{AccessToken: "token", Account: accevents.AccountView{Proxy: "http://image-proxy.invalid:8080"}}, nil)
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
				Images: []upevents.ImageOutput{{B64JSON: "first"}},
				Usage:  &tokenusage.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8},
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
}
