package biz

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"time"

	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptimage"
	imgcommon "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/common"
	imgevents "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/events"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

func (s *Proxy) GenerateImage(ctx context.Context, request chatgptimage.Request) (chatgptimage.Result, error) {
	return s.runChatGPTImages(ctx, request, false)
}

func (s *Proxy) EditImage(ctx context.Context, request chatgptimage.Request) (chatgptimage.Result, error) {
	if len(request.Images) == 0 {
		return chatgptimage.Result{}, fmt.Errorf("image is required")
	}
	return s.runChatGPTImages(ctx, request, true)
}

func (s *Proxy) runChatGPTImages(ctx context.Context, request chatgptimage.Request, edit bool) (chatgptimage.Result, error) {
	if strings.TrimSpace(request.Prompt) == "" {
		return chatgptimage.Result{}, fmt.Errorf("prompt is required")
	}
	if request.N < 1 || request.N > 4 {
		return chatgptimage.Result{}, fmt.Errorf("n must be between 1 and 4")
	}
	if request.ResponseFormat != "b64_json" && request.ResponseFormat != "url" {
		return chatgptimage.Result{}, fmt.Errorf("response_format must be b64_json or url")
	}
	result := chatgptimage.Result{Created: time.Now().Unix(), Data: make([]chatgptimage.Data, 0, request.N)}
	for range request.N {
		output, err := s.runOneChatGPTImage(ctx, request, edit)
		if err != nil {
			return chatgptimage.Result{}, err
		}
		result.Data = append(result.Data, output...)
	}
	return result, nil
}

func (s *Proxy) runOneChatGPTImage(ctx context.Context, request chatgptimage.Request, edit bool) ([]chatgptimage.Data, error) {
	accountValue, accountErr := s.SendEvent(event.NewEventWithContext(accevents.TopicAcquireImageToken, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.AcquireImageTokenCommand{})).Get()
	if accountErr != nil {
		slog.Warn("chatgpt image execution failed", "stage", "acquire_account", "event_error_code", accountErr.Code)
		return nil, fmt.Errorf("chatgpt image account unavailable")
	}
	account, ok := accountValue.(accevents.AcquireImageTokenResult)
	if !ok || strings.TrimSpace(account.AccessToken) == "" {
		return nil, fmt.Errorf("invalid chatgpt image account result")
	}
	defer s.releaseChatGPTImageSlot(ctx, account.AccessToken)

	var value any
	var upstreamErr *cd.Error
	if edit {
		value, upstreamErr = s.SendEvent(event.NewEventWithContext(upevents.TopicEditImage, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.EditImageCommand{AccessToken: account.AccessToken, Prompt: request.Prompt, Model: request.Model, Size: request.Size, Quality: request.Quality, Images: request.Images})).Get()
	} else {
		value, upstreamErr = s.SendEvent(event.NewEventWithContext(upevents.TopicGenerateImage, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.GenerateImageCommand{AccessToken: account.AccessToken, Prompt: request.Prompt, Model: request.Model, Size: request.Size, Quality: request.Quality})).Get()
	}
	if upstreamErr != nil {
		_, generationResult := value.(upevents.GenerateImageResult)
		_, editResult := value.(upevents.EditImageResult)
		slog.Warn("chatgpt image execution failed", "stage", "upstream_execute", "event_error_code", upstreamErr.Code, "generation_result_type_match", generationResult, "edit_result_type_match", editResult)
		s.markChatGPTImageResult(ctx, account.AccessToken, false)
		return nil, fmt.Errorf("chatgpt image upstream failed")
	}
	var outputs []upevents.ImageOutput
	if edit {
		result, ok := value.(upevents.EditImageResult)
		if !ok {
			return nil, fmt.Errorf("invalid chatgpt image edit result")
		}
		outputs = result.Images
	} else {
		result, ok := value.(upevents.GenerateImageResult)
		if !ok {
			return nil, fmt.Errorf("invalid chatgpt image generation result")
		}
		outputs = result.Images
	}
	data, err := s.presentChatGPTImages(ctx, outputs, request.ResponseFormat, request.BaseURL)
	if err != nil {
		s.markChatGPTImageResult(ctx, account.AccessToken, false)
		return nil, err
	}
	s.markChatGPTImageResult(ctx, account.AccessToken, true)
	return data, nil
}

func (s *Proxy) presentChatGPTImages(ctx context.Context, outputs []upevents.ImageOutput, responseFormat, baseURL string) ([]chatgptimage.Data, error) {
	if len(outputs) == 0 {
		return nil, fmt.Errorf("chatgpt image upstream returned no image")
	}
	data := make([]chatgptimage.Data, 0, len(outputs))
	for _, output := range outputs {
		item := chatgptimage.Data{RevisedPrompt: output.RevisedPrompt}
		if responseFormat == "b64_json" {
			item.B64JSON = output.B64JSON
			if item.B64JSON == "" && len(output.Bytes) > 0 {
				item.B64JSON = base64.StdEncoding.EncodeToString(output.Bytes)
			}
			if item.B64JSON == "" {
				return nil, fmt.Errorf("chatgpt image has no content")
			}
		} else if len(output.Bytes) > 0 {
			value, err := s.SendEvent(event.NewEventWithContext(imgevents.TopicSave, s.ID(), imgcommon.UnitID, event.NewHeader(), ctx, imgevents.SaveCommand{Bytes: output.Bytes, BaseURL: baseURL})).Get()
			if err != nil {
				return nil, fmt.Errorf("save chatgpt image failed")
			}
			saved, ok := value.(imgevents.SaveResult)
			if !ok || strings.TrimSpace(saved.PublicURL) == "" {
				return nil, fmt.Errorf("invalid saved image result")
			}
			item.URL = saved.PublicURL
		} else if strings.TrimSpace(output.URL) != "" {
			item.URL = output.URL
		} else {
			return nil, fmt.Errorf("chatgpt image has no URL")
		}
		data = append(data, item)
	}
	return data, nil
}

func (s *Proxy) releaseChatGPTImageSlot(ctx context.Context, token string) {
	_, _ = s.SendEvent(event.NewEventWithContext(accevents.TopicReleaseImageSlot, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.ReleaseImageSlotCommand{AccessToken: token})).Get()
}

func (s *Proxy) markChatGPTImageResult(ctx context.Context, token string, success bool) {
	_, _ = s.SendEvent(event.NewEventWithContext(accevents.TopicMarkImageResult, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.MarkImageResultCommand{AccessToken: token, Success: success})).Get()
}
