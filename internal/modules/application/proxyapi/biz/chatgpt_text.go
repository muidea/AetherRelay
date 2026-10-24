package biz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	acccommon "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
	"aetherrelay/internal/modules/application/proxyapi/pkg/usagetarget"
	upcommon "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	"aetherrelay/internal/pkg/chatgptattempt"
	"github.com/muidea/magicCommon/event"
)

func (s *Proxy) Complete(ctx context.Context, request chatgpttext.Request) (chatgpttext.Result, error) {
	return s.executeChatGPTText(ctx, request, func(account accevents.AcquireTextTokenResult) (chatgpttext.Result, bool, error) {
		out, err := s.completeChatGPTTextOnce(ctx, account.AccessToken, account.Account.Proxy, request)
		return out, false, err
	})
}

func (s *Proxy) Stream(ctx context.Context, request chatgpttext.Request, emit func(chatgpttext.Delta) error) (chatgpttext.Result, error) {
	return s.executeChatGPTText(ctx, request, func(account accevents.AcquireTextTokenResult) (chatgpttext.Result, bool, error) {
		return s.streamChatGPTTextOnce(ctx, account.AccessToken, account.Account.Proxy, request, emit)
	})
}

// CP-FAIL-021: authentication recovery and account failover share two executions.
func (s *Proxy) executeChatGPTText(ctx context.Context, request chatgpttext.Request, execute func(accevents.AcquireTextTokenResult) (chatgpttext.Result, bool, error)) (chatgpttext.Result, error) {
	if err := ctx.Err(); err != nil {
		return chatgpttext.Result{}, mapContextError(err)
	}
	account, err := s.acquireChatGPTTextToken(ctx, request.Model)
	if err != nil {
		if ctx.Err() != nil {
			return chatgpttext.Result{}, mapContextError(ctx.Err())
		}
		return chatgpttext.Result{}, err
	}
	refreshedToken := false
	var lastOut chatgpttext.Result
	var lastErr error
	for index := 1; index <= 2; index++ {
		if err := ctx.Err(); err != nil {
			return lastOut, mapContextError(err)
		}
		if index > 1 && !textRetryBudgetAvailable(ctx) {
			return lastOut, lastErr
		}
		usagetarget.Record(ctx, "chatgptweb", account.Account.ID, account.Account.Email)
		out, emitted, executionErr := execute(account)
		var transportFailure *chatgptfail.Failure
		errors.As(executionErr, &transportFailure)
		if executionErr != nil && ctx.Err() != nil {
			executionErr = mapContextError(ctx.Err())
		}
		lastOut, lastErr = out, executionErr
		observation := chatgptattempt.Observation{Index: index, AccountID: account.Account.ID, AccountLabel: account.Account.Email, Success: executionErr == nil, UpstreamStatus: out.UpstreamStatus}
		var failure *chatgptfail.Failure
		if errors.As(executionErr, &failure) {
			observation.FailureClass = string(failure.Kind)
		}
		if transportFailure != nil {
			observation.Operation, observation.GenerationNotSent = transportFailure.Operation, transportFailure.GenerationNotSent
			if transportFailure.UpstreamStatus != 0 {
				observation.UpstreamStatus = transportFailure.UpstreamStatus
			}
		}
		usagetarget.RecordTextAttempt(ctx, observation)
		hasOutput := emitted || out.Text != "" || out.ConversationID != "" || !out.Generation.FirstOutputAt.IsZero()
		if executionErr != nil && isInvalidChatGPTTextFailure(executionErr) && !hasOutput && index == 1 && textRetryBudgetAvailable(ctx) && (failure.Operation == "" || failure.GenerationNotSent) {
			refreshed, permanent, refreshErr := s.refreshChatGPTTextToken(ctx, account.AccessToken)
			if refreshErr == nil && !permanent {
				account.AccessToken, account.Account, refreshedToken = refreshed.AccessToken, refreshed.Account, true
				continue
			}
			if permanent {
				s.removeInvalidChatGPTTextToken(ctx, account.AccessToken)
			}
			if refreshErr != nil {
				executionErr = refreshErr
			}
			s.recordChatGPTTextResult(ctx, account.Account.ID, request.Model, executionErr)
			return out, executionErr
		}
		if refreshedToken && isInvalidChatGPTTextFailure(executionErr) && !hasOutput {
			s.removeInvalidChatGPTTextToken(ctx, account.AccessToken)
		}
		s.recordChatGPTTextResult(ctx, account.Account.ID, request.Model, executionErr)
		if index == 2 || hasOutput || !textRetryBudgetAvailable(ctx) || !retryableTextPrepareFailure(executionErr) {
			return out, executionErr
		}
		alternate, alternateErr := s.acquireAlternativeTextAccount(ctx, request.Model, account)
		if alternateErr != nil {
			if ctx.Err() != nil {
				return out, mapContextError(ctx.Err())
			}
			return out, executionErr
		}
		account = alternate
	}
	return chatgpttext.Result{}, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("text execution budget exhausted"))
}

func textRetryBudgetAvailable(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) > time.Second
}

func retryableTextPrepareFailure(err error) bool {
	var failure *chatgptfail.Failure
	if !errors.As(err, &failure) || !failure.GenerationNotSent {
		return false
	}
	switch failure.Operation {
	case "bootstrap", "requirements_prepare", "requirements_finalize":
	default:
		return false
	}
	switch failure.Kind {
	case chatgptfail.KindTLS, chatgptfail.KindTimeout:
		return true
	case chatgptfail.KindUpstream:
		return failure.UpstreamStatus >= 500 && failure.UpstreamStatus <= 599 || failure.Operation == "bootstrap" && failure.UpstreamStatus == 403
	default:
		return false
	}
}

// Only owner-selected accounts are considered; never mutate account proxy bindings.
func (s *Proxy) acquireAlternativeTextAccount(ctx context.Context, model string, failed accevents.AcquireTextTokenResult) (accevents.AcquireTextTokenResult, error) {
	excluded := []string{strings.TrimSpace(failed.AccessToken)}
	var sameExit *accevents.AcquireTextTokenResult
	for scan := 0; scan < 8 && textRetryBudgetAvailable(ctx); scan++ {
		candidate, err := s.acquireChatGPTTextTokenExcluding(ctx, model, excluded)
		if err != nil {
			break
		}
		token := strings.TrimSpace(candidate.AccessToken)
		if slices.Contains(excluded, token) {
			break
		}
		excluded = append(excluded, token)
		if candidate.Account.ID == "" || candidate.Account.ID == failed.Account.ID {
			continue
		}
		if !sameChatGPTProxyEndpoint(failed.Account.Proxy, candidate.Account.Proxy) && !(strings.TrimSpace(failed.Account.Proxy) == "" && strings.TrimSpace(candidate.Account.Proxy) == "") {
			return candidate, nil
		}
		if sameExit == nil {
			copy := candidate
			sameExit = &copy
		}
	}
	if sameExit != nil && textRetryBudgetAvailable(ctx) {
		return *sameExit, nil
	}
	return accevents.AcquireTextTokenResult{}, chatgptfail.New(chatgptfail.KindProviderUnavailable, fmt.Errorf("no alternative text account"))
}

func textAttemptFailure(class upevents.ErrorClass, cause error, operation string, status int, notSent bool) error {
	failure := mapUpstreamTextFailure(class, cause)
	var typed *chatgptfail.Failure
	if errors.As(failure, &typed) {
		typed.Operation, typed.UpstreamStatus, typed.GenerationNotSent = operation, status, notSent
	}
	return failure
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
			out = chatgpttext.Result{ConversationID: partial.ConversationID, ActualModel: partial.ActualModel, Text: partial.Text, Generation: partial.Generation, UpstreamStatus: partial.UpstreamStatus}
			if partial.ErrorClass != "" {
				return out, textAttemptFailure(partial.ErrorClass, eventErr, partial.ErrorOperation, partial.UpstreamStatus, partial.GenerationNotSent)
			}
		}
		return out, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt text completion failed"))
	}
	completed, ok := value.(upevents.CompleteTextResult)
	if !ok {
		return chatgpttext.Result{}, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("invalid chatgpt text completion result"))
	}
	out := chatgpttext.Result{ConversationID: completed.ConversationID, ActualModel: completed.ActualModel, Text: completed.Text, Generation: completed.Generation, UpstreamStatus: completed.UpstreamStatus}
	if completed.ErrorClass != "" {
		return out, textAttemptFailure(completed.ErrorClass, fmt.Errorf("chatgpt text completion failed"), completed.ErrorOperation, completed.UpstreamStatus, completed.GenerationNotSent)
	}
	return out, nil
}

func (s *Proxy) streamChatGPTTextOnce(ctx context.Context, token, proxy string, request chatgpttext.Request, emit func(chatgpttext.Delta) error) (result chatgpttext.Result, emitted bool, resultErr error) {
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
	defer func() {
		value, _ := s.SendEvent(event.NewEventWithContext(upevents.TopicCancelText, s.ID(), upcommon.UnitID, event.NewHeader(), context.WithoutCancel(ctx), upevents.CancelTextCommand{StreamID: started.StreamID})).Get()
		if canceled, ok := value.(upevents.CancelTextResult); ok && !canceled.Generation.FirstOutputAt.IsZero() && resultErr != nil {
			result.Generation = canceled.Generation
		}
	}()
	var builder strings.Builder
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
		if update.UpstreamStatus != 0 {
			result.UpstreamStatus = update.UpstreamStatus
		}
		if !update.Generation.FirstOutputAt.IsZero() {
			result.Generation = update.Generation
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
				return result, emitted, textAttemptFailure(update.ErrorClass, fmt.Errorf("chatgpt text stream failed"), update.ErrorOperation, update.UpstreamStatus, update.GenerationNotSent)
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
	return s.acquireChatGPTTextTokenExcluding(ctx, model, nil)
}

// acquireChatGPTTextTokenExcluding is used by bounded recovery paths that
// have already observed a failure on one account. The account-pool owner keeps
// the actual selection and model capability checks authoritative.
func (s *Proxy) acquireChatGPTTextTokenExcluding(ctx context.Context, model string, exclude []string) (accevents.AcquireTextTokenResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicAcquireTextToken, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.AcquireTextTokenCommand{
		Exclude: append([]string(nil), exclude...), Model: model, Capability: accevents.ModelCapabilityTextGeneration,
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

// recordChatGPTTextResult records the final outcome for one attempted account.
// Search recovery calls it once per account attempt. Local client disconnects,
// response-writer errors and request-content rejections do not describe account
// health and therefore must not trigger account cooling.
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
