package biz

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	Data           []chatgptimage.Data
	Usage          *tokenusage.Usage
	ConversationID string
	AccountID      string
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
		if result.ConversationID == "" && output.ConversationID != "" {
			result.ConversationID = output.ConversationID
			result.AccountID = output.AccountID
		}
		if err != nil {
			// Preserve already-accumulated usage/data when a later of n calls fails.
			return result, err
		}
		result.Data = append(result.Data, output.Data...)
	}
	return result, nil
}

func (s *Proxy) runOneChatGPTImage(ctx context.Context, request chatgptimage.Request, edit bool) (oneImageResult, error) {
	accountValue, accountErr := s.SendEvent(event.NewEventWithContext(accevents.TopicAcquireImageToken, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.AcquireImageTokenCommand{Model: request.Model, Capability: accevents.ModelCapabilityImageGeneration})).Get()
	if accountErr != nil {
		slog.Warn("chatgpt image execution failed", "stage", "acquire_account", "event_error_code", accountErr.Code)
		return oneImageResult{}, chatgptfail.New(chatgptfail.KindProviderUnavailable, fmt.Errorf("chatgpt image account unavailable"))
	}
	account, ok := accountValue.(accevents.AcquireImageTokenResult)
	if !ok || strings.TrimSpace(account.AccessToken) == "" {
		return oneImageResult{}, chatgptfail.New(chatgptfail.KindProviderUnavailable, fmt.Errorf("invalid chatgpt image account result"))
	}
	defer s.releaseChatGPTImageSlot(ctx, account.AccessToken)

	activeToken, activeProxy := account.AccessToken, account.Account.Proxy
	for attempt := 0; attempt < 2; attempt++ {
		value, upstreamErr := s.executeChatGPTImageOnce(ctx, activeToken, activeProxy, request, edit)
		outputs, usage, conversationID, class, valid := imageUpstreamResult(value, edit)
		if upstreamErr == nil && valid && class == "" && len(outputs) > 0 {
			// A completed generation consumes account quota even when local image
			// storage or client delivery later fails. Those local errors must not
			// poison account health or leave quota feedback stale.
			s.markChatGPTImageResult(ctx, account.AccessToken, request.Model, true, "")
			data, err := s.presentChatGPTImages(ctx, request.APIKeyID, outputs, request.ResponseFormat, request.BaseURL)
			if err != nil {
				return oneImageResult{Usage: usage, ConversationID: conversationID, AccountID: account.Account.ID}, err
			}
			return oneImageResult{Data: data, Usage: usage, ConversationID: conversationID, AccountID: account.Account.ID}, nil
		}
		if !valid {
			return oneImageResult{Usage: usage}, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("invalid chatgpt image upstream result"))
		}
		if class == "" {
			class = upevents.ErrClassUpstream
		}
		if upstreamErr != nil {
			slog.Warn("chatgpt image execution failed", "stage", "upstream_execute", "event_error_code", upstreamErr.Code, "error_class", class, "has_conversation", conversationID != "")
		}

		// An invalid access token gets one refresh attempt. Only a failure before
		// ChatGPT created a conversation may be submitted again; retrying after a
		// conversation ID exists could generate a duplicate image.
		if class == upevents.ErrClassInvalidToken && attempt == 0 {
			refreshed, permanent, refreshErr := s.refreshChatGPTTextToken(ctx, activeToken)
			if refreshErr != nil {
				s.markChatGPTImageResult(ctx, account.AccessToken, request.Model, false, string(upevents.ErrClassUpstream))
				return oneImageResult{Usage: usage, ConversationID: conversationID, AccountID: account.Account.ID}, refreshErr
			}
			if permanent {
				s.removeInvalidChatGPTTextToken(ctx, activeToken)
				s.markChatGPTImageResult(ctx, account.AccessToken, request.Model, false, string(upevents.ErrClassInvalidToken))
				return oneImageResult{Usage: usage, ConversationID: conversationID, AccountID: account.Account.ID}, mapUpstreamImageFailure(class, fmt.Errorf("chatgpt image credential is invalid"))
			}
			if conversationID == "" {
				activeToken, activeProxy = refreshed.AccessToken, refreshed.Account.Proxy
				continue
			}
			// The refreshed credential is kept for subsequent resume/recovery, but
			// this synchronous request is not resubmitted.
			s.markChatGPTImageResult(ctx, account.AccessToken, request.Model, false, string(upevents.ErrClassUpstream))
			return oneImageResult{Usage: usage, ConversationID: conversationID, AccountID: account.Account.ID}, mapUpstreamImageFailure(class, fmt.Errorf("chatgpt image conversation needs resume"))
		}

		if class == upevents.ErrClassInvalidToken {
			s.removeInvalidChatGPTTextToken(ctx, activeToken)
		}
		s.markChatGPTImageResult(ctx, account.AccessToken, request.Model, false, string(class))
		return oneImageResult{Usage: usage, ConversationID: conversationID, AccountID: account.Account.ID}, mapUpstreamImageFailure(class, fmt.Errorf("chatgpt image upstream failed"))
	}
	return oneImageResult{}, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("chatgpt image retry exhausted"))
}

func (s *Proxy) executeChatGPTImageOnce(ctx context.Context, token, proxy string, request chatgptimage.Request, edit bool) (any, *cd.Error) {
	if edit {
		return s.SendEvent(event.NewEventWithContext(upevents.TopicEditImage, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.EditImageCommand{AccessToken: token, Proxy: proxy, Prompt: request.Prompt, Model: request.Model, Size: request.Size, Quality: request.Quality, Images: request.Images})).Get()
	}
	return s.SendEvent(event.NewEventWithContext(upevents.TopicGenerateImage, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.GenerateImageCommand{AccessToken: token, Proxy: proxy, Prompt: request.Prompt, Model: request.Model, Size: request.Size, Quality: request.Quality})).Get()
}

func imageUpstreamResult(value any, edit bool) ([]upevents.ImageOutput, *tokenusage.Usage, string, upevents.ErrorClass, bool) {
	if edit {
		result, ok := value.(upevents.EditImageResult)
		if !ok {
			return nil, nil, "", "", false
		}
		return result.Images, result.Usage, result.ConversationID, result.ErrorClass, true
	}
	result, ok := value.(upevents.GenerateImageResult)
	if !ok {
		return nil, nil, "", "", false
	}
	return result.Images, result.Usage, result.ConversationID, result.ErrorClass, true
}

func mapUpstreamImageFailure(class upevents.ErrorClass, cause error) error {
	return chatgptfail.New(chatgptfail.FromUpstreamClass(string(class)), cause)
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

func (s *Proxy) presentChatGPTImages(ctx context.Context, apiKeyID string, outputs []upevents.ImageOutput, responseFormat, baseURL string) ([]chatgptimage.Data, error) {
	if len(outputs) == 0 {
		return nil, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt image upstream returned no image"))
	}
	data := make([]chatgptimage.Data, 0, len(outputs))
	for _, output := range outputs {
		item := chatgptimage.Data{RevisedPrompt: output.RevisedPrompt}
		var saved imgevents.SaveResult
		if len(output.Bytes) > 0 {
			value, err := s.SendEvent(event.NewEventWithContext(imgevents.TopicSave, s.ID(), imgcommon.UnitID, event.NewHeader(), ctx, imgevents.SaveCommand{APIKeyID: apiKeyID, Bytes: output.Bytes, BaseURL: baseURL})).Get()
			if err != nil {
				return nil, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("save chatgpt image failed"))
			}
			var ok bool
			saved, ok = value.(imgevents.SaveResult)
			if !ok {
				return nil, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("invalid saved image result"))
			}
		}
		if responseFormat == "b64_json" {
			item.B64JSON = output.B64JSON
			if item.B64JSON == "" && len(output.Bytes) > 0 {
				item.B64JSON = base64.StdEncoding.EncodeToString(output.Bytes)
			}
			if item.B64JSON == "" {
				return nil, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt image has no content"))
			}
		} else if len(output.Bytes) > 0 {
			if strings.TrimSpace(saved.PublicURL) == "" {
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

func (s *Proxy) ArchiveResponseImages(ctx context.Context, apiKeyID string, responseBody []byte, baseURL string) error {
	var response struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return fmt.Errorf("decode native image response: %w", err)
	}
	for _, item := range response.Data {
		encoded := strings.TrimSpace(item.B64JSON)
		if encoded == "" {
			continue
		}
		payload, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("decode native image payload: %w", err)
		}
		if _, err := s.SendEvent(event.NewEventWithContext(imgevents.TopicSave, s.ID(), imgcommon.UnitID, event.NewHeader(), ctx, imgevents.SaveCommand{APIKeyID: apiKeyID, Bytes: payload, BaseURL: baseURL})).Get(); err != nil {
			return fmt.Errorf("save native image: %w", err)
		}
	}
	return nil
}

func (s *Proxy) releaseChatGPTImageSlot(ctx context.Context, token string) {
	_, _ = s.SendEvent(event.NewEventWithContext(accevents.TopicReleaseImageSlot, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.ReleaseImageSlotCommand{AccessToken: token})).Get()
}

func (s *Proxy) markChatGPTImageResult(ctx context.Context, token, model string, success bool, errorClass string) {
	_, _ = s.SendEvent(event.NewEventWithContext(accevents.TopicMarkImageResult, s.ID(), acccommon.UnitID, event.NewHeader(), context.WithoutCancel(ctx), accevents.MarkImageResultCommand{AccessToken: token, Model: model, Success: success, ErrorClass: errorClass})).Get()
}
