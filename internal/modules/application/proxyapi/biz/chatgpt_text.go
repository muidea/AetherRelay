package biz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	acccommon "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
	upcommon "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func (s *Proxy) Complete(ctx context.Context, request chatgpttext.Request) (out chatgpttext.Result, err error) {
	account, err := s.acquireChatGPTTextToken(ctx, request.Model)
	if err != nil {
		slog.Warn("chatgpt text execution failed", "stage", "acquire_account")
		return chatgpttext.Result{}, err
	}
	defer func() { s.recordChatGPTTextResult(ctx, account.Account.ID, request.Model, err) }()

	out, err = s.completeChatGPTTextOnce(ctx, account.AccessToken, account.Account.Proxy, request)
	if !isInvalidChatGPTTextFailure(err) {
		return out, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return out, mapContextError(contextErr)
	}
	refreshed, permanentFailure, refreshErr := s.refreshChatGPTTextToken(ctx, account.AccessToken)
	if refreshErr != nil {
		// The original 401 is not sufficient evidence to retire an account if
		// its OAuth refresh endpoint is temporarily unavailable. Return the
		// refresh failure instead so the final result records a transient model
		// cooldown rather than invalidating the credential.
		return out, refreshErr
	}
	if permanentFailure {
		s.removeInvalidChatGPTTextToken(ctx, account.AccessToken)
		return out, err
	}
	out, err = s.completeChatGPTTextOnce(ctx, refreshed.AccessToken, refreshed.Account.Proxy, request)
	if isInvalidChatGPTTextFailure(err) {
		s.removeInvalidChatGPTTextToken(ctx, refreshed.AccessToken)
	}
	return out, err
}

func (s *Proxy) Stream(ctx context.Context, request chatgpttext.Request, emit func(chatgpttext.Delta) error) (out chatgpttext.Result, err error) {
	account, err := s.acquireChatGPTTextToken(ctx, request.Model)
	if err != nil {
		return chatgpttext.Result{}, err
	}
	defer func() { s.recordChatGPTTextResult(ctx, account.Account.ID, request.Model, err) }()

	out, emitted, err := s.streamChatGPTTextOnce(ctx, account.AccessToken, account.Account.Proxy, request, emit)
	if !isInvalidChatGPTTextFailure(err) || emitted {
		return out, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return out, mapContextError(contextErr)
	}
	refreshed, permanentFailure, refreshErr := s.refreshChatGPTTextToken(ctx, account.AccessToken)
	if refreshErr != nil {
		return out, refreshErr
	}
	if permanentFailure {
		s.removeInvalidChatGPTTextToken(ctx, account.AccessToken)
		return out, err
	}
	out, _, err = s.streamChatGPTTextOnce(ctx, refreshed.AccessToken, refreshed.Account.Proxy, request, emit)
	if isInvalidChatGPTTextFailure(err) {
		s.removeInvalidChatGPTTextToken(ctx, refreshed.AccessToken)
	}
	return out, err
}

func (s *Proxy) completeChatGPTTextOnce(ctx context.Context, token, proxy string, request chatgpttext.Request) (chatgpttext.Result, error) {
	value, eventErr := s.SendEvent(event.NewEventWithContext(upevents.TopicCompleteText, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.CompleteTextCommand{
		AccessToken: token, Proxy: proxy, Model: request.Model, Messages: toUpstreamMessages(request.Messages), ThinkingEffort: request.ThinkingEffort,
	})).Get()
	if eventErr != nil {
		partial, isPartial := value.(upevents.CompleteTextResult)
		slog.Warn("chatgpt text execution failed", "stage", "upstream_complete", "event_error_code", eventErr.Code, "has_partial_result", value != nil, "partial_result_type_match", isPartial)
		out := chatgpttext.Result{}
		if isPartial {
			out = chatgpttext.Result{ConversationID: partial.ConversationID, ActualModel: partial.ActualModel, Text: partial.Text}
			if partial.ErrorClass != "" {
				return out, mapUpstreamTextFailure(partial.ErrorClass, eventErr)
			}
		}
		return out, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt text completion failed"))
	}
	completed, ok := value.(upevents.CompleteTextResult)
	if !ok {
		return chatgpttext.Result{}, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("invalid chatgpt text completion result"))
	}
	out := chatgpttext.Result{ConversationID: completed.ConversationID, ActualModel: completed.ActualModel, Text: completed.Text}
	if completed.ErrorClass != "" {
		return out, mapUpstreamTextFailure(completed.ErrorClass, fmt.Errorf("chatgpt text completion failed"))
	}
	return out, nil
}

func (s *Proxy) streamChatGPTTextOnce(ctx context.Context, token, proxy string, request chatgpttext.Request, emit func(chatgpttext.Delta) error) (chatgpttext.Result, bool, error) {
	value, startErr := s.SendEvent(event.NewEventWithContext(upevents.TopicStartText, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.StartTextCommand{
		AccessToken: token, Proxy: proxy, Model: request.Model, Messages: toUpstreamMessages(request.Messages), ThinkingEffort: request.ThinkingEffort,
	})).Get()
	if startErr != nil {
		slog.Warn("chatgpt text execution failed", "stage", "upstream_start_stream", "event_error_code", startErr.Code)
		return chatgpttext.Result{}, false, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt text stream failed"))
	}
	started, ok := value.(upevents.StartTextResult)
	if !ok || started.StreamID == "" {
		return chatgpttext.Result{}, false, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("invalid chatgpt text stream result"))
	}
	defer s.SendEvent(event.NewEvent(upevents.TopicCancelText, s.ID(), upcommon.UnitID, nil, upevents.CancelTextCommand{StreamID: started.StreamID}))
	var result chatgpttext.Result
	var builder strings.Builder
	emitted := false
	for {
		if err := ctx.Err(); err != nil {
			return result, emitted, mapContextError(err)
		}
		value, pullErr := s.SendEvent(event.NewEventWithContext(upevents.TopicPullText, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.PullTextCommand{StreamID: started.StreamID, TimeoutMillis: 1000})).Get()
		if pullErr != nil {
			slog.Warn("chatgpt text execution failed", "stage", "upstream_pull_stream", "event_error_code", pullErr.Code)
			return result, emitted, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt text stream failed"))
		}
		update, ok := value.(upevents.PullTextResult)
		if !ok {
			return result, emitted, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("invalid chatgpt text stream update"))
		}
		if update.ConversationID != "" {
			result.ConversationID = update.ConversationID
		}
		if update.ActualModel != "" {
			result.ActualModel = update.ActualModel
		}
		if update.Delta != "" {
			emitted = true
			builder.WriteString(update.Delta)
			result.Text = builder.String()
			if emit != nil {
				if err := emit(chatgpttext.Delta{Text: update.Delta, ActualModel: update.ActualModel}); err != nil {
					return result, emitted, mapEmitError(err)
				}
			}
		}
		if update.Done {
			if update.ErrorClass != "" {
				return result, emitted, mapUpstreamTextFailure(update.ErrorClass, fmt.Errorf("chatgpt text stream failed"))
			}
			result.Text = builder.String()
			return result, emitted, nil
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
	_, _ = s.SendEvent(event.NewEventWithContext(accevents.TopicRemoveInvalid, s.ID(), acccommon.UnitID, event.NewHeader(), context.WithoutCancel(ctx), accevents.RemoveInvalidCommand{AccessToken: token, Event: "chat_completion"})).Get()
}

func (s *Proxy) refreshChatGPTTextToken(ctx context.Context, token string) (accevents.RefreshTextTokenResult, bool, error) {
	if strings.TrimSpace(token) == "" {
		return accevents.RefreshTextTokenResult{}, false, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt access token is unavailable"))
	}
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicRefreshTextToken, s.ID(), acccommon.UnitID, event.NewHeader(), context.WithoutCancel(ctx), accevents.RefreshTextTokenCommand{AccessToken: token})).Get()
	if err != nil {
		return accevents.RefreshTextTokenResult{}, false, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("refresh chatgpt text token: %w", err))
	}
	refreshed, ok := value.(accevents.RefreshTextTokenResult)
	if !ok {
		return accevents.RefreshTextTokenResult{}, false, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("invalid refreshed chatgpt text token result"))
	}
	if refreshed.Refreshed && strings.TrimSpace(refreshed.AccessToken) != "" {
		return refreshed, false, nil
	}
	if refreshed.PermanentFailure {
		return refreshed, true, nil
	}
	return refreshed, false, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt oauth refresh temporarily unavailable"))
}

func (s *Proxy) acquireChatGPTTextToken(ctx context.Context, model string) (accevents.AcquireTextTokenResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicAcquireTextToken, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.AcquireTextTokenCommand{
		Model: model, Capability: accevents.ModelCapabilityTextGeneration,
	})).Get()
	if err != nil {
		return accevents.AcquireTextTokenResult{}, chatgptfail.New(chatgptfail.KindProviderUnavailable, fmt.Errorf("chatgpt account unavailable"))
	}
	account, ok := value.(accevents.AcquireTextTokenResult)
	if !ok || strings.TrimSpace(account.AccessToken) == "" {
		return accevents.AcquireTextTokenResult{}, chatgptfail.New(chatgptfail.KindProviderUnavailable, fmt.Errorf("chatgpt account unavailable"))
	}
	return account, nil
}

// recordChatGPTTextResult records the single final account outcome. Local
// client disconnects, response-writer errors and request-content rejections do
// not describe account health and therefore must not trigger account cooling.
func (s *Proxy) recordChatGPTTextResult(ctx context.Context, accountID, model string, executionErr error) {
	if strings.TrimSpace(accountID) == "" {
		return
	}
	success := executionErr == nil
	errorClass := ""
	if !success {
		var failure *chatgptfail.Failure
		if !errors.As(executionErr, &failure) {
			return
		}
		switch failure.Kind {
		case chatgptfail.KindInvalidToken, chatgptfail.KindRateLimit, chatgptfail.KindTLS, chatgptfail.KindTimeout, chatgptfail.KindUpstream:
			errorClass = string(failure.Kind)
		default:
			return
		}
	}
	_, _ = s.SendEvent(event.NewEventWithContext(accevents.TopicRecordTextResult, s.ID(), acccommon.UnitID, event.NewHeader(), context.WithoutCancel(ctx), accevents.RecordTextResultCommand{
		AccountID:  accountID,
		Model:      model,
		Success:    success,
		ErrorClass: errorClass,
	})).Get()
}

func isInvalidChatGPTTextFailure(err error) bool {
	var failure *chatgptfail.Failure
	return errors.As(err, &failure) && failure.Kind == chatgptfail.KindInvalidToken
}

func toUpstreamMessages(messages []chatgpttext.Message) []upevents.TextMessage {
	result := make([]upevents.TextMessage, 0, len(messages))
	for _, message := range messages {
		result = append(result, upevents.TextMessage{Role: message.Role, Content: message.Content, Images: message.Images, Files: message.Files})
	}
	return result
}

func mapUpstreamTextFailure(class upevents.ErrorClass, cause error) error {
	return chatgptfail.New(chatgptfail.FromUpstreamClass(string(class)), cause)
}

func mapContextError(err error) error {
	if err == nil {
		return nil
	}
	if err == context.Canceled {
		return chatgptfail.New(chatgptfail.KindClientCanceled, err)
	}
	if err == context.DeadlineExceeded {
		return chatgptfail.New(chatgptfail.KindTimeout, err)
	}
	return chatgptfail.New(chatgptfail.KindClientCanceled, err)
}

func mapEmitError(err error) error {
	if err == nil {
		return nil
	}
	if f, ok := chatgpttext.AsFailure(err); ok {
		return f
	}
	if err == context.Canceled {
		return chatgptfail.New(chatgptfail.KindClientCanceled, err)
	}
	// Response writer failures surface here from the HTTP adapter emit callback.
	return chatgptfail.New(chatgptfail.KindClientWrite, err)
}
