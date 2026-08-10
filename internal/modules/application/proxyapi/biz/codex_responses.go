package biz

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func (s *Proxy) CompleteCodexResponses(ctx context.Context, request codexresponses.Request) (codexresponses.Result, error) {
	tried := make([]string, 0, 2)
	var lastFailure *codexresponses.Failure
	for {
		account, err := s.acquireCodexAccount(ctx, request.Model, tried)
		if err != nil {
			if lastFailure != nil {
				return codexresponses.Result{}, lastFailure
			}
			return codexresponses.Result{}, err
		}
		out, failure := s.completeCodexOnce(ctx, account, request)
		if failure == nil {
			s.recordCodexResult(ctx, account.AccountID, request.Model, true, "", 0, false, "")
			return out, nil
		}
		lastFailure = failure
		if failure.Kind == codexresponses.KindInvalidToken {
			refreshed, refreshErr := s.refreshCodexAccount(ctx, account.AccountID)
			if refreshErr == nil && refreshed.Refreshed {
				out, failure = s.completeCodexOnce(ctx, accevents.AcquireResult{AccountID: refreshed.AccountID, AccessToken: refreshed.AccessToken, AccountIDHeader: refreshed.AccountIDHeader, Proxy: refreshed.Proxy}, request)
				if failure == nil {
					s.recordCodexResult(ctx, account.AccountID, request.Model, true, "", 0, false, "")
					return out, nil
				}
				lastFailure = failure
			}
			if refreshErr != nil {
				class := refreshFailureClass(refreshed)
				if refreshed.PermanentFailure {
					class = accevents.ErrorInvalidToken
				}
				s.recordCodexResult(ctx, account.AccountID, request.Model, false, class, 0, false, "")
				tried = append(tried, account.AccountID)
				continue
			}
			if refreshed.PermanentFailure {
				s.recordCodexResult(ctx, account.AccountID, request.Model, false, accevents.ErrorInvalidToken, 0, false, "")
				tried = append(tried, account.AccountID)
				continue
			}
		}
		if failure.Kind == codexresponses.KindInvalidRequest {
			return codexresponses.Result{}, failure
		}
		s.recordCodexResult(ctx, account.AccountID, request.Model, false, string(failure.Kind), failure.RetryAfterSeconds, failure.QuotaExhausted, failure.QuotaResetAt)
		if failure.Kind == codexresponses.KindRateLimit || failure.Kind == codexresponses.KindInvalidToken {
			tried = append(tried, account.AccountID)
			continue
		}
		return codexresponses.Result{}, failure
	}
}

func (s *Proxy) StreamCodexResponses(ctx context.Context, request codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
	tried := make([]string, 0, 2)
	var lastFailure *codexresponses.Failure
	for {
		account, err := s.acquireCodexAccount(ctx, request.Model, tried)
		if err != nil {
			if lastFailure != nil {
				return lastFailure
			}
			return err
		}
		emitted := false
		err = s.streamCodexOnce(ctx, account, request, started, func(line []byte) error {
			emitted = emitted || len(line) > 0
			if emit == nil {
				return nil
			}
			return emit(line)
		})
		if err == nil {
			s.recordCodexResult(ctx, account.AccountID, request.Model, true, "", 0, false, "")
			return nil
		}
		failure, _ := codexresponses.AsFailure(err)
		if failure == nil {
			return err
		}
		lastFailure = failure
		if emitted {
			s.recordCodexResult(ctx, account.AccountID, request.Model, false, string(failure.Kind), failure.RetryAfterSeconds, failure.QuotaExhausted, failure.QuotaResetAt)
			return err
		}
		if failure.Kind == codexresponses.KindInvalidToken {
			refreshed, refreshErr := s.refreshCodexAccount(ctx, account.AccountID)
			if refreshErr == nil && refreshed.Refreshed {
				err = s.streamCodexOnce(ctx, accevents.AcquireResult{AccountID: refreshed.AccountID, AccessToken: refreshed.AccessToken, AccountIDHeader: refreshed.AccountIDHeader, Proxy: refreshed.Proxy}, request, started, emit)
				if err == nil {
					s.recordCodexResult(ctx, account.AccountID, request.Model, true, "", 0, false, "")
					return nil
				}
				failure, _ = codexresponses.AsFailure(err)
				if failure != nil {
					lastFailure = failure
				}
			}
			if refreshErr != nil {
				class := refreshFailureClass(refreshed)
				if refreshed.PermanentFailure {
					class = accevents.ErrorInvalidToken
				}
				s.recordCodexResult(ctx, account.AccountID, request.Model, false, class, 0, false, "")
				tried = append(tried, account.AccountID)
				continue
			}
			if refreshed.PermanentFailure {
				s.recordCodexResult(ctx, account.AccountID, request.Model, false, accevents.ErrorInvalidToken, 0, false, "")
				tried = append(tried, account.AccountID)
				continue
			}
		}
		if failure == nil {
			return err
		}
		if failure.Kind == codexresponses.KindInvalidRequest {
			return err
		}
		s.recordCodexResult(ctx, account.AccountID, request.Model, false, string(failure.Kind), failure.RetryAfterSeconds, failure.QuotaExhausted, failure.QuotaResetAt)
		if failure.Kind == codexresponses.KindRateLimit || failure.Kind == codexresponses.KindInvalidToken {
			tried = append(tried, account.AccountID)
			continue
		}
		return err
	}
}

func (s *Proxy) acquireCodexAccount(ctx context.Context, model string, exclude []string) (accevents.AcquireResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicAcquire, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.AcquireCommand{Model: model, Exclude: exclude})).Get()
	if err != nil {
		return accevents.AcquireResult{}, codexresponses.NewFailure(codexresponses.KindProviderUnavailable, 0, fmt.Errorf("Codex OAuth account unavailable"))
	}
	account, ok := value.(accevents.AcquireResult)
	if !ok || strings.TrimSpace(account.AccountID) == "" || strings.TrimSpace(account.AccessToken) == "" {
		return accevents.AcquireResult{}, codexresponses.NewFailure(codexresponses.KindProviderUnavailable, 0, fmt.Errorf("Codex OAuth account unavailable"))
	}
	return account, nil
}

func (s *Proxy) completeCodexOnce(ctx context.Context, account accevents.AcquireResult, request codexresponses.Request) (codexresponses.Result, *codexresponses.Failure) {
	ctx, cancel := codexRequestContext(ctx, s.config.RequestTimeout)
	defer cancel()
	value, err := s.SendEvent(event.NewEventWithContext(upevents.TopicComplete, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.CompleteCommand{AccessToken: account.AccessToken, AccountIDHeader: account.AccountIDHeader, Proxy: account.Proxy, Body: request.Body, MaxResponseBytes: s.config.MaxUpstreamResponseBytes})).Get()
	if err != nil {
		return codexresponses.Result{}, codexresponses.NewFailure(codexresponses.KindUpstream, 0, fmt.Errorf("Codex upstream unavailable"))
	}
	completed, ok := value.(upevents.CompleteResult)
	if !ok {
		return codexresponses.Result{}, codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("invalid Codex upstream result"))
	}
	if completed.ErrorClass != "" {
		return codexresponses.Result{}, failureFromUpstream(completed.ErrorClass, completed.RetryAfterSeconds, completed.RateLimit, completed.HTTPStatus)
	}
	return codexresponses.Result{Body: completed.Body, Headers: toCodexHeaders(completed.Headers)}, nil
}

func (s *Proxy) streamCodexOnce(ctx context.Context, account accevents.AcquireResult, request codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
	ctx, cancel := codexRequestContext(ctx, s.config.RequestTimeout)
	defer cancel()
	value, err := s.SendEvent(event.NewEventWithContext(upevents.TopicStart, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.StartCommand{AccessToken: account.AccessToken, AccountIDHeader: account.AccountIDHeader, Proxy: account.Proxy, Body: request.Body, MaxLineBytes: s.config.MaxSSELineBytes})).Get()
	if err != nil {
		return codexresponses.NewFailure(codexresponses.KindUpstream, 0, fmt.Errorf("Codex stream unavailable"))
	}
	startedUpstream, ok := value.(upevents.StartResult)
	if !ok {
		return codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("invalid Codex stream result"))
	}
	if startedUpstream.ErrorClass != "" {
		return failureFromUpstream(startedUpstream.ErrorClass, startedUpstream.RetryAfterSeconds, startedUpstream.RateLimit, startedUpstream.HTTPStatus)
	}
	if strings.TrimSpace(startedUpstream.StreamID) == "" {
		return codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("Codex stream id is missing"))
	}
	defer s.SendEvent(event.NewEvent(upevents.TopicCancel, s.ID(), upcommon.UnitID, nil, upevents.CancelCommand{StreamID: startedUpstream.StreamID}))
	if started != nil {
		if err := started(codexresponses.StreamStart{Headers: toCodexHeaders(startedUpstream.Headers)}); err != nil {
			return clientFailure(err)
		}
	}
	for {
		value, pullErr := s.SendEvent(event.NewEventWithContext(upevents.TopicPull, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.PullCommand{StreamID: startedUpstream.StreamID, TimeoutMillis: 1000})).Get()
		if pullErr != nil {
			return codexresponses.NewFailure(codexresponses.KindUpstream, 0, fmt.Errorf("Codex stream pull failed"))
		}
		update, ok := value.(upevents.PullResult)
		if !ok {
			return codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("invalid Codex stream update"))
		}
		if len(update.Data) > 0 && emit != nil {
			if err := emit(update.Data); err != nil {
				return clientFailure(err)
			}
		}
		if update.Done {
			if update.ErrorClass != "" {
				return failureFromUpstream(update.ErrorClass, update.RetryAfterSeconds, update.RateLimit, 0)
			}
			return nil
		}
	}
}

func (s *Proxy) refreshCodexAccount(ctx context.Context, id string) (accevents.RefreshTokenResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicRefreshToken, s.ID(), acccommon.UnitID, event.NewHeader(), context.WithoutCancel(ctx), accevents.RefreshTokenCommand{AccountID: id})).Get()
	result, ok := value.(accevents.RefreshTokenResult)
	if !ok {
		if err != nil {
			return accevents.RefreshTokenResult{ErrorClass: accevents.ErrorUpstream}, err
		}
		return accevents.RefreshTokenResult{}, fmt.Errorf("invalid Codex refresh result")
	}
	// The account owner deliberately returns its redacted failure classification
	// together with a synchronous error. Keep that data: a timeout/network/429
	// refresh failure needs a bounded cooldown, not an invalid-token quarantine.
	if err != nil {
		return result, err
	}
	return result, nil
}

func refreshFailureClass(result accevents.RefreshTokenResult) string {
	switch result.ErrorClass {
	case accevents.ErrorRateLimit, accevents.ErrorTimeout, accevents.ErrorNetwork, accevents.ErrorUpstream:
		return result.ErrorClass
	default:
		return accevents.ErrorUpstream
	}
}

func (s *Proxy) recordCodexResult(ctx context.Context, id, model string, success bool, class string, retryAfter int, quotaExhausted bool, quotaResetAt string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	_, _ = s.SendEvent(event.NewEventWithContext(accevents.TopicRecordResult, s.ID(), acccommon.UnitID, event.NewHeader(), context.WithoutCancel(ctx), accevents.RecordResultCommand{
		AccountID: id, Model: model, Success: success, ErrorClass: class, RetryAfterSeconds: retryAfter,
		QuotaExhausted: quotaExhausted, QuotaResetAt: quotaResetAt,
	})).Get()
}

func toCodexHeaders(headers []upevents.Header) []codexresponses.Header {
	result := make([]codexresponses.Header, 0, len(headers))
	for _, header := range headers {
		result = append(result, codexresponses.Header{Name: header.Name, Value: header.Value})
	}
	return result
}
func failureFromUpstream(class upevents.ErrorClass, retryAfter int, rateLimit upevents.RateLimitObservation, httpStatus int) *codexresponses.Failure {
	var failure *codexresponses.Failure
	switch class {
	case upevents.ErrorInvalidRequest:
		failure = codexresponses.NewFailure(codexresponses.KindInvalidRequest, retryAfter, fmt.Errorf("Codex upstream rejected the request"))
	case upevents.ErrorInvalidToken:
		failure = codexresponses.NewFailure(codexresponses.KindInvalidToken, retryAfter, fmt.Errorf("Codex OAuth access token rejected"))
	case upevents.ErrorRateLimit:
		if rateLimit.UsageLimited {
			failure = codexresponses.NewQuotaFailure(codexresponses.KindRateLimit, retryAfter, true, rateLimit.ResetAt, fmt.Errorf("Codex upstream usage limit reached"))
		} else {
			failure = codexresponses.NewFailure(codexresponses.KindRateLimit, retryAfter, fmt.Errorf("Codex upstream rate limited"))
		}
	case upevents.ErrorTimeout:
		failure = codexresponses.NewFailure(codexresponses.KindTimeout, retryAfter, fmt.Errorf("Codex upstream timed out"))
	case upevents.ErrorNetwork:
		failure = codexresponses.NewFailure(codexresponses.KindNetwork, retryAfter, fmt.Errorf("Codex upstream network failed"))
	case upevents.ErrorProtocol:
		failure = codexresponses.NewFailure(codexresponses.KindProtocol, retryAfter, fmt.Errorf("Codex upstream protocol failed"))
	default:
		failure = codexresponses.NewFailure(codexresponses.KindUpstream, retryAfter, fmt.Errorf("Codex upstream failed"))
	}
	failure.HTTPStatus = httpStatus
	return failure
}
func clientFailure(err error) error {
	if errors.Is(err, context.Canceled) {
		return codexresponses.NewFailure(codexresponses.KindClientCanceled, 0, err)
	}
	return codexresponses.NewFailure(codexresponses.KindClientWrite, 0, err)
}

func codexRequestContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}
