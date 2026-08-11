package biz

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"testing"

	acccommon "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptimage"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	basebiz "aetherrelay/internal/modules/base/biz"
	imgcommon "aetherrelay/internal/modules/blocks/chatgptimagestore/pkg/common"
	imgevents "aetherrelay/internal/modules/blocks/chatgptimagestore/pkg/events"
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

func TestPresentChatGPTImagesUsesRasterBytesForBase64Response(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(4)
	t.Cleanup(func() {
		background.Shutdown(nil)
		hub.Terminate(context.Background())
	})

	var saved []byte
	imageStore := event.NewSimpleObserver(imgcommon.UnitID, hub)
	imageStore.Subscribe(imgevents.TopicSave, func(ev event.Event, result event.Result) {
		command, ok := ev.Data().(imgevents.SaveCommand)
		if !ok {
			t.Fatalf("save command type = %T", ev.Data())
		}
		if command.APIKeyID != "builtin-local" {
			t.Fatalf("save api_key_id = %q", command.APIKeyID)
		}
		saved = append([]byte(nil), command.Bytes...)
		result.Set(imgevents.SaveResult{PublicURL: "https://relay.example/images/result.png"}, nil)
	})

	normalized := testPNG(t, 5, 3)
	staleUpstreamB64 := base64.StdEncoding.EncodeToString(testPNG(t, 1, 1))
	p := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	data, err := p.presentChatGPTImages(context.Background(), "builtin-local", []upevents.ImageOutput{{
		Bytes:   normalized,
		B64JSON: staleUpstreamB64,
	}}, "b64_json", "https://relay.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 1 {
		t.Fatalf("data=%+v", data)
	}
	if !bytes.Equal(saved, normalized) {
		t.Fatal("image store did not receive normalized raster bytes")
	}
	payload, err := base64.StdEncoding.DecodeString(data[0].B64JSON)
	if err != nil {
		t.Fatalf("decode returned b64_json: %v", err)
	}
	if !bytes.Equal(payload, normalized) {
		t.Fatal("returned b64_json did not use normalized raster bytes")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(payload))
	if err != nil || format != "png" || config.Width != 5 || config.Height != 3 {
		t.Fatalf("returned raster = %dx%d %q, err=%v", config.Width, config.Height, format, err)
	}
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	image := image.NewRGBA(image.Rect(0, 0, width, height))
	image.Set(0, 0, color.RGBA{R: 255, A: 255})
	var payload bytes.Buffer
	if err := png.Encode(&payload, image); err != nil {
		t.Fatal(err)
	}
	return payload.Bytes()
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
