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
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptfail"
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptimage"
	imgcommon "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/common"
	imgevents "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/events"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
	"ai-proxy/internal/pkg/chatgpttokenusage"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

type oneImageResult struct {
	Data  []chatgptimage.Data
	Usage *tokenusage.Usage
}

func (s *Proxy) GenerateImage(ctx context.Context, request chatgptimage.Request) (chatgptimage.Result, error) {
	return s.runChatGPTImages(ctx, request, false)
}

func (s *Proxy) EditImage(ctx context.Context, request chatgptimage.Request) (chatgptimage.Result, error) {
	if len(request.Images) == 0 {
		return chatgptimage.Result{}, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("image is required"))
	}
	return s.runChatGPTImages(ctx, request, true)
}

func (s *Proxy) runChatGPTImages(ctx context.Context, request chatgptimage.Request, edit bool) (chatgptimage.Result, error) {
	if strings.TrimSpace(request.Prompt) == "" {
		return chatgptimage.Result{}, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("prompt is required"))
	}
	if request.N < 1 || request.N > 4 {
		return chatgptimage.Result{}, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("n must be between 1 and 4"))
	}
	if request.ResponseFormat != "b64_json" && request.ResponseFormat != "url" {
		return chatgptimage.Result{}, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("response_format must be b64_json or url"))
	}
	result := chatgptimage.Result{Created: time.Now().Unix(), Data: make([]chatgptimage.Data, 0, request.N)}
	for i := 0; i < request.N; i++ {
		output, err := s.runOneChatGPTImage(ctx, request, edit)
		// An upstream image call can have consumed tokens even if it returns an
		// error. Preserve that bounded partial projection for the single proxy
		// usage event before returning the request failure.
		result.Usage = addTokenUsage(result.Usage, output.Usage)
		if err != nil {
			// Preserve already-accumulated usage/data when a later of n calls fails.
			return result, err
		}
		result.Data = append(result.Data, output.Data...)
	}
	return result, nil
}

func (s *Proxy) runOneChatGPTImage(ctx context.Context, request chatgptimage.Request, edit bool) (oneImageResult, error) {
	accountValue, accountErr := s.SendEvent(event.NewEventWithContext(accevents.TopicAcquireImageToken, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.AcquireImageTokenCommand{Model: request.Model, Operation: accevents.ModelOperationImageGenerations})).Get()
	if accountErr != nil {
		slog.Warn("chatgpt image execution failed", "stage", "acquire_account", "event_error_code", accountErr.Code)
		return oneImageResult{}, chatgptfail.New(chatgptfail.KindProviderUnavailable, fmt.Errorf("chatgpt image account unavailable"))
	}
	account, ok := accountValue.(accevents.AcquireImageTokenResult)
	if !ok || strings.TrimSpace(account.AccessToken) == "" {
		return oneImageResult{}, chatgptfail.New(chatgptfail.KindProviderUnavailable, fmt.Errorf("invalid chatgpt image account result"))
	}
	defer s.releaseChatGPTImageSlot(ctx, account.AccessToken)

	var value any
	var upstreamErr *cd.Error
	if edit {
		value, upstreamErr = s.SendEvent(event.NewEventWithContext(upevents.TopicEditImage, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.EditImageCommand{AccessToken: account.AccessToken, Proxy: account.Account.Proxy, Prompt: request.Prompt, Model: request.Model, Size: request.Size, Quality: request.Quality, Images: request.Images})).Get()
	} else {
		value, upstreamErr = s.SendEvent(event.NewEventWithContext(upevents.TopicGenerateImage, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.GenerateImageCommand{AccessToken: account.AccessToken, Proxy: account.Account.Proxy, Prompt: request.Prompt, Model: request.Model, Size: request.Size, Quality: request.Quality})).Get()
	}
	if upstreamErr != nil {
		partialUsage := imageUsageFromUpstream(value, edit)
		_, generationResult := value.(upevents.GenerateImageResult)
		_, editResult := value.(upevents.EditImageResult)
		slog.Warn("chatgpt image execution failed", "stage", "upstream_execute", "event_error_code", upstreamErr.Code, "generation_result_type_match", generationResult, "edit_result_type_match", editResult)
		s.markChatGPTImageResult(ctx, account.AccessToken, false)
		return oneImageResult{Usage: partialUsage}, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt image upstream failed"))
	}
	var outputs []upevents.ImageOutput
	var usage *tokenusage.Usage
	if edit {
		result, ok := value.(upevents.EditImageResult)
		if !ok {
			return oneImageResult{}, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("invalid chatgpt image edit result"))
		}
		outputs = result.Images
		usage = result.Usage
	} else {
		result, ok := value.(upevents.GenerateImageResult)
		if !ok {
			return oneImageResult{}, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("invalid chatgpt image generation result"))
		}
		outputs = result.Images
		usage = result.Usage
	}
	data, err := s.presentChatGPTImages(ctx, outputs, request.ResponseFormat, request.BaseURL)
	if err != nil {
		s.markChatGPTImageResult(ctx, account.AccessToken, false)
		return oneImageResult{Usage: usage}, err
	}
	s.markChatGPTImageResult(ctx, account.AccessToken, true)
	return oneImageResult{Data: data, Usage: usage}, nil
}

func imageUsageFromUpstream(value any, edit bool) *tokenusage.Usage {
	if edit {
		if result, ok := value.(upevents.EditImageResult); ok {
			return result.Usage
		}
		return nil
	}
	if result, ok := value.(upevents.GenerateImageResult); ok {
		return result.Usage
	}
	return nil
}

func addTokenUsage(base, extra *tokenusage.Usage) *tokenusage.Usage {
	if extra == nil {
		return base
	}
	if base == nil {
		copied := *extra
		return &copied
	}
	base.PromptTokens += extra.PromptTokens
	base.CompletionTokens += extra.CompletionTokens
	base.InputTokens += extra.InputTokens
	base.OutputTokens += extra.OutputTokens
	base.TotalTokens += extra.TotalTokens
	return base
}

func (s *Proxy) presentChatGPTImages(ctx context.Context, outputs []upevents.ImageOutput, responseFormat, baseURL string) ([]chatgptimage.Data, error) {
	if len(outputs) == 0 {
		return nil, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt image upstream returned no image"))
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
				return nil, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt image has no content"))
			}
		} else if len(output.Bytes) > 0 {
			value, err := s.SendEvent(event.NewEventWithContext(imgevents.TopicSave, s.ID(), imgcommon.UnitID, event.NewHeader(), ctx, imgevents.SaveCommand{Bytes: output.Bytes, BaseURL: baseURL})).Get()
			if err != nil {
				return nil, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("save chatgpt image failed"))
			}
			saved, ok := value.(imgevents.SaveResult)
			if !ok || strings.TrimSpace(saved.PublicURL) == "" {
				return nil, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("invalid saved image result"))
			}
			item.URL = saved.PublicURL
		} else if strings.TrimSpace(output.URL) != "" {
			item.URL = output.URL
		} else {
			return nil, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt image has no URL"))
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
