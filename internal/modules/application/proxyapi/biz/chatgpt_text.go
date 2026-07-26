package biz

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgpttext"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func (s *Proxy) Complete(ctx context.Context, request chatgpttext.Request) (chatgpttext.Result, error) {
	token, err := s.acquireChatGPTTextToken(ctx, request.Model)
	if err != nil {
		slog.Warn("chatgpt text execution failed", "stage", "acquire_account")
		return chatgpttext.Result{}, err
	}
	value, eventErr := s.SendEvent(event.NewEventWithContext(upevents.TopicCompleteText, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.CompleteTextCommand{
		AccessToken: token, Model: request.Model, Messages: toUpstreamMessages(request.Messages), ThinkingEffort: request.ThinkingEffort,
	})).Get()
	if eventErr != nil {
		_, isPartial := value.(upevents.CompleteTextResult)
		slog.Warn("chatgpt text execution failed", "stage", "upstream_complete", "event_error_code", eventErr.Code, "has_partial_result", value != nil, "partial_result_type_match", isPartial)
		if partial, ok := value.(upevents.CompleteTextResult); ok && partial.ErrorClass != "" {
			if partial.ErrorClass == upevents.ErrClassInvalidToken {
				s.removeInvalidChatGPTTextToken(ctx, token)
			}
			return chatgpttext.Result{}, fmt.Errorf("chatgpt text completion %s", partial.ErrorClass)
		}
		return chatgpttext.Result{}, fmt.Errorf("chatgpt text completion failed")
	}
	completed, ok := value.(upevents.CompleteTextResult)
	if !ok {
		return chatgpttext.Result{}, fmt.Errorf("invalid chatgpt text completion result")
	}
	return chatgpttext.Result{ConversationID: completed.ConversationID, Text: completed.Text}, nil
}

func (s *Proxy) Stream(ctx context.Context, request chatgpttext.Request, emit func(chatgpttext.Delta) error) (chatgpttext.Result, error) {
	token, err := s.acquireChatGPTTextToken(ctx, request.Model)
	if err != nil {
		return chatgpttext.Result{}, err
	}
	value, startErr := s.SendEvent(event.NewEventWithContext(upevents.TopicStartText, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.StartTextCommand{
		AccessToken: token, Model: request.Model, Messages: toUpstreamMessages(request.Messages), ThinkingEffort: request.ThinkingEffort,
	})).Get()
	if startErr != nil {
		slog.Warn("chatgpt text execution failed", "stage", "upstream_start_stream", "event_error_code", startErr.Code)
		return chatgpttext.Result{}, fmt.Errorf("chatgpt text stream failed")
	}
	started, ok := value.(upevents.StartTextResult)
	if !ok || started.StreamID == "" {
		return chatgpttext.Result{}, fmt.Errorf("invalid chatgpt text stream result")
	}
	defer s.SendEvent(event.NewEvent(upevents.TopicCancelText, s.ID(), upcommon.UnitID, nil, upevents.CancelTextCommand{StreamID: started.StreamID}))
	var result chatgpttext.Result
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		value, pullErr := s.SendEvent(event.NewEventWithContext(upevents.TopicPullText, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.PullTextCommand{StreamID: started.StreamID, TimeoutMillis: 1000})).Get()
		if pullErr != nil {
			slog.Warn("chatgpt text execution failed", "stage", "upstream_pull_stream", "event_error_code", pullErr.Code)
			return result, fmt.Errorf("chatgpt text stream failed")
		}
		update, ok := value.(upevents.PullTextResult)
		if !ok {
			return result, fmt.Errorf("invalid chatgpt text stream update")
		}
		if update.Delta != "" && emit != nil {
			if err := emit(chatgpttext.Delta{Text: update.Delta}); err != nil {
				return result, err
			}
		}
		if update.Done {
			if update.ErrorClass != "" {
				if update.ErrorClass == upevents.ErrClassInvalidToken {
					s.removeInvalidChatGPTTextToken(ctx, token)
				}
				return result, fmt.Errorf("chatgpt text stream failed")
			}
			return result, nil
		}
	}
}

// removeInvalidChatGPTTextToken mirrors the account-pool state transition used
// by the source gateway. Only a classified invalid credential is evicted;
// transient TLS, timeout and upstream errors keep the account available.
func (s *Proxy) removeInvalidChatGPTTextToken(ctx context.Context, token string) {
	if strings.TrimSpace(token) == "" {
		return
	}
	_, _ = s.SendEvent(event.NewEventWithContext(accevents.TopicRemoveInvalid, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.RemoveInvalidCommand{AccessToken: token, Event: "chat_completion"})).Get()
}

func (s *Proxy) acquireChatGPTTextToken(ctx context.Context, model string) (string, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicAcquireTextToken, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.AcquireTextTokenCommand{
		Model: model, Operation: accevents.ModelOperationChatCompletions,
	})).Get()
	if err != nil {
		return "", fmt.Errorf("chatgpt account unavailable")
	}
	account, ok := value.(accevents.AcquireTextTokenResult)
	if !ok || strings.TrimSpace(account.AccessToken) == "" {
		return "", fmt.Errorf("chatgpt account unavailable")
	}
	return account.AccessToken, nil
}

func toUpstreamMessages(messages []chatgpttext.Message) []upevents.TextMessage {
	result := make([]upevents.TextMessage, 0, len(messages))
	for _, message := range messages {
		result = append(result, upevents.TextMessage{Role: message.Role, Content: message.Content})
	}
	return result
}
