package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
)

type codexWebsocketBinding struct {
	leaseID        string
	accountID      string
	model          string
	resultRecorded bool
	turnEvidence   bool
}

func (s *Proxy) OpenCodexWebsocket(ctx context.Context, request codexresponses.WebsocketOpenRequest) (codexresponses.WebsocketOpenResult, error) {
	tried := make([]string, 0, 2)
	var lastFailure *codexresponses.Failure
	for {
		account, err := s.acquireCodexAccountForTransport(ctx, request.Model, tried, request.SessionHash, accevents.TransportWebsocket)
		if err != nil {
			if lastFailure != nil {
				return codexresponses.WebsocketOpenResult{}, lastFailure
			}
			return codexresponses.WebsocketOpenResult{}, err
		}
		value, sendErr := s.SendEvent(event.NewEventWithContext(upevents.TopicWSOpen, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.WSOpenCommand{
			AccessToken: account.AccessToken, AccountIDHeader: account.AccountIDHeader, Proxy: account.Proxy, MaxMessageBytes: s.config.MaxSSELineBytes, SessionHash: request.SessionHash,
			RemoteCompactionV2: request.RemoteCompactionV2, ResponsesLite: request.ResponsesLite,
		})).Get()
		if sendErr != nil {
			s.releaseCodexAccount(ctx, account.LeaseID)
			return codexresponses.WebsocketOpenResult{}, codexresponses.NewFailure(codexresponses.KindUpstream, 0, fmt.Errorf("Codex websocket unavailable"))
		}
		opened, ok := value.(upevents.WSOpenResult)
		if !ok {
			s.releaseCodexAccount(ctx, account.LeaseID)
			return codexresponses.WebsocketOpenResult{}, codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("invalid Codex websocket result"))
		}
		if opened.ErrorClass != "" {
			if transportExplicitlyUnsupported(accevents.TransportWebsocket, opened.HTTPStatus) {
				s.recordCodexTransportCapability(ctx, account.AccountID, accevents.TransportWebsocket, false)
				s.releaseCodexAccount(ctx, account.LeaseID)
				tried = append(tried, account.AccountID)
				continue
			}
			failure := failureFromUpstream(opened.ErrorClass, 0, upevents.RateLimitObservation{}, opened.HTTPStatus)
			lastFailure = failure
			s.releaseCodexAccount(ctx, account.LeaseID)
			s.recordCodexResult(ctx, account.AccountID, request.Model, false, string(failure.Kind), 0, false, "")
			if retryableCodexFailure(failure) {
				tried = append(tried, account.AccountID)
				continue
			}
			return codexresponses.WebsocketOpenResult{}, failure
		}
		if strings.TrimSpace(opened.SessionID) == "" {
			s.releaseCodexAccount(ctx, account.LeaseID)
			return codexresponses.WebsocketOpenResult{}, codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("Codex websocket session is missing"))
		}
		s.mu.Lock()
		if s.codexWebsockets == nil {
			s.codexWebsockets = map[string]codexWebsocketBinding{}
		}
		s.codexWebsockets[opened.SessionID] = codexWebsocketBinding{leaseID: account.LeaseID, accountID: account.AccountID, model: request.Model}
		s.mu.Unlock()
		s.recordCodexTransportCapability(ctx, account.AccountID, accevents.TransportWebsocket, true)
		return codexresponses.WebsocketOpenResult{SessionID: opened.SessionID}, nil
	}
}

func (s *Proxy) SendCodexWebsocket(ctx context.Context, sessionID string, payload []byte) error {
	s.mu.Lock()
	binding, found := s.codexWebsockets[sessionID]
	if found {
		binding.resultRecorded = false
		binding.turnEvidence = false
		s.codexWebsockets[sessionID] = binding
	}
	s.mu.Unlock()
	value, err := s.SendEvent(event.NewEventWithContext(upevents.TopicWSSend, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.WSSendCommand{SessionID: sessionID, Payload: payload})).Get()
	if err != nil {
		s.recordCodexWebsocketTurn(ctx, sessionID, false, accevents.ErrorUpstream)
		return codexresponses.NewFailure(codexresponses.KindUpstream, 0, fmt.Errorf("Codex websocket send failed"))
	}
	if sent, ok := value.(upevents.WSSendResult); !ok || !sent.Sent {
		s.recordCodexWebsocketTurn(ctx, sessionID, false, accevents.ErrorProtocol)
		return codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("invalid Codex websocket send result"))
	}
	return nil
}

func (s *Proxy) PullCodexWebsocket(ctx context.Context, sessionID string) ([]byte, bool, error) {
	value, err := s.SendEvent(event.NewEventWithContext(upevents.TopicWSPull, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.WSPullCommand{SessionID: sessionID, TimeoutMillis: 1000})).Get()
	if err != nil {
		s.recordCodexWebsocketTurn(ctx, sessionID, false, accevents.ErrorUpstream)
		return nil, false, codexresponses.NewFailure(codexresponses.KindUpstream, 0, fmt.Errorf("Codex websocket pull failed"))
	}
	update, ok := value.(upevents.WSPullResult)
	if !ok {
		s.recordCodexWebsocketTurn(ctx, sessionID, false, accevents.ErrorProtocol)
		return nil, false, codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("invalid Codex websocket update"))
	}
	if update.ErrorClass != "" && len(update.Payload) == 0 {
		failure := failureFromUpstream(update.ErrorClass, 0, upevents.RateLimitObservation{}, 0, upevents.SafeError{})
		s.recordCodexWebsocketTurn(ctx, sessionID, false, string(failure.Kind))
		return nil, update.Done, failure
	}
	if len(update.Payload) > 0 {
		evidence := codexWebsocketPayloadHasEvidence(update.Payload)
		s.mu.Lock()
		binding, found := s.codexWebsockets[sessionID]
		if found && evidence {
			binding.turnEvidence = true
			s.codexWebsockets[sessionID] = binding
		}
		turnEvidence := found && binding.turnEvidence
		s.mu.Unlock()
		if success, terminal, class := codexWebsocketTurnOutcomeWithEvidence(update.Payload, turnEvidence); terminal {
			if update.ErrorClass != "" {
				failure := failureFromUpstream(update.ErrorClass, update.RetryAfterSeconds, update.RateLimit, 0)
				class = string(failure.Kind)
				s.recordCodexWebsocketTurnResult(ctx, sessionID, false, class, failure.RetryAfterSeconds, failure.QuotaExhausted, failure.QuotaResetAt)
			} else {
				s.recordCodexWebsocketTurn(ctx, sessionID, success, class)
			}
			if !success && codexWebsocketEmptyCompleted(update.Payload) && !turnEvidence {
				return nil, true, codexresponses.NewFailure(codexresponses.KindUpstream, 0, fmt.Errorf("Codex upstream returned an empty response.completed"))
			}
		}
	}
	return update.Payload, update.Done, nil
}

func (s *Proxy) CloseCodexWebsocket(ctx context.Context, sessionID string) {
	s.mu.Lock()
	binding, found := s.codexWebsockets[sessionID]
	delete(s.codexWebsockets, sessionID)
	s.mu.Unlock()
	_, _ = s.SendEvent(event.NewEventWithContext(upevents.TopicWSClose, s.ID(), upcommon.UnitID, event.NewHeader(), context.WithoutCancel(ctx), upevents.WSCloseCommand{SessionID: sessionID})).Get()
	if found {
		s.releaseCodexAccount(ctx, binding.leaseID)
	}
}

func (s *Proxy) recordCodexWebsocketTurn(ctx context.Context, sessionID string, success bool, class string) {
	s.recordCodexWebsocketTurnResult(ctx, sessionID, success, class, 0, false, "")
}

func (s *Proxy) recordCodexWebsocketTurnResult(ctx context.Context, sessionID string, success bool, class string, retryAfter int, quotaExhausted bool, quotaResetAt string) {
	s.mu.Lock()
	binding, found := s.codexWebsockets[sessionID]
	shouldRecord := found && !binding.resultRecorded
	if shouldRecord {
		binding.resultRecorded = true
		s.codexWebsockets[sessionID] = binding
	}
	s.mu.Unlock()
	if shouldRecord {
		s.recordCodexResult(ctx, binding.accountID, binding.model, success, class, retryAfter, quotaExhausted, quotaResetAt)
	}
}

func codexWebsocketTurnOutcome(payload []byte) (success, terminal bool, class string) {
	return codexWebsocketTurnOutcomeWithEvidence(payload, false)
}

func codexWebsocketTurnOutcomeWithEvidence(payload []byte, turnEvidence bool) (success, terminal bool, class string) {
	var event struct {
		Type     string          `json:"type"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return false, false, ""
	}
	switch event.Type {
	case "response.completed", "response.done":
		if codexResponseObjectEmpty(event.Response) && !turnEvidence {
			return false, true, accevents.ErrorUpstream
		}
		return true, true, ""
	case "response.incomplete":
		return true, true, ""
	case "response.failed", "response.cancelled", "response.canceled", "error":
		return false, true, accevents.ErrorUpstream
	default:
		return false, false, ""
	}
}

func codexWebsocketPayloadHasEvidence(payload []byte) bool {
	var event struct {
		Type      string          `json:"type"`
		Delta     json.RawMessage `json:"delta"`
		Item      json.RawMessage `json:"item"`
		Usage     json.RawMessage `json:"usage"`
		Error     json.RawMessage `json:"error"`
		Arguments json.RawMessage `json:"arguments"`
		Input     json.RawMessage `json:"input"`
		Response  json.RawMessage `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return false
	}
	if event.Type == "response.completed" || event.Type == "response.done" {
		return !codexResponseObjectEmpty(event.Response)
	}
	if event.Type == "response.created" || event.Type == "response.in_progress" || event.Type == "response.queued" {
		return false
	}
	return rawJSONPresent(event.Delta) || rawJSONPresent(event.Item) || rawJSONPresent(event.Usage) || rawJSONPresent(event.Error) ||
		rawJSONPresent(event.Arguments) || rawJSONPresent(event.Input)
}

func rawJSONPresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && string(trimmed) != "null" && string(trimmed) != `""`
}

func codexWebsocketEmptyCompleted(payload []byte) bool {
	var event struct {
		Type     string          `json:"type"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil || event.Type != "response.completed" && event.Type != "response.done" {
		return false
	}
	return codexResponseObjectEmpty(event.Response)
}

func codexResponseObjectEmpty(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return true
	}
	var response struct {
		Output []json.RawMessage `json:"output"`
		Usage  json.RawMessage   `json:"usage"`
		Error  json.RawMessage   `json:"error"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return false
	}
	return len(response.Output) == 0 && !rawJSONPresent(response.Usage) && !rawJSONPresent(response.Error)
}

func (s *Proxy) CompleteCodexResponses(ctx context.Context, request codexresponses.Request) (codexresponses.Result, error) {
	tried := make([]string, 0, 2)
	var lastFailure *codexresponses.Failure
	for {
		account, err := s.acquireCodexAccount(ctx, request.Model, tried, request.SessionHash)
		if err != nil {
			if lastFailure != nil {
				return codexresponses.Result{}, lastFailure
			}
			return codexresponses.Result{}, err
		}
		out, failure := s.completeCodexOnce(ctx, account, request)
		if failure == nil {
			s.releaseCodexAccount(ctx, account.LeaseID)
			s.recordCodexResult(ctx, account.AccountID, request.Model, true, "", 0, false, "")
			return out, nil
		}
		lastFailure = failure
		if failure.Kind == codexresponses.KindInvalidToken {
			refreshed, refreshErr := s.refreshCodexAccount(ctx, account.AccountID)
			if refreshErr == nil && refreshed.Refreshed {
				out, failure = s.completeCodexOnce(ctx, accevents.AcquireResult{AccountID: refreshed.AccountID, AccessToken: refreshed.AccessToken, AccountIDHeader: refreshed.AccountIDHeader, Proxy: refreshed.Proxy}, request)
				if failure == nil {
					s.releaseCodexAccount(ctx, account.LeaseID)
					s.recordCodexResult(ctx, account.AccountID, request.Model, true, "", 0, false, "")
					return out, nil
				}
				lastFailure = failure
			}
			if refreshErr != nil {
				s.releaseCodexAccount(ctx, account.LeaseID)
				class := refreshFailureClass(refreshed)
				if refreshed.PermanentFailure {
					class = accevents.ErrorInvalidToken
				}
				s.recordCodexResult(ctx, account.AccountID, request.Model, false, class, 0, false, "")
				tried = append(tried, account.AccountID)
				continue
			}
			if refreshed.PermanentFailure {
				s.releaseCodexAccount(ctx, account.LeaseID)
				s.recordCodexResult(ctx, account.AccountID, request.Model, false, accevents.ErrorInvalidToken, 0, false, "")
				tried = append(tried, account.AccountID)
				continue
			}
		}
		if failure.Kind == codexresponses.KindInvalidRequest {
			s.releaseCodexAccount(ctx, account.LeaseID)
			return codexresponses.Result{}, failure
		}
		s.releaseCodexAccount(ctx, account.LeaseID)
		s.recordCodexResult(ctx, account.AccountID, request.Model, false, string(failure.Kind), failure.RetryAfterSeconds, failure.QuotaExhausted, failure.QuotaResetAt)
		if retryableCodexFailure(failure) {
			tried = append(tried, account.AccountID)
			continue
		}
		return codexresponses.Result{}, failure
	}
}

func (s *Proxy) CompleteCodexCompact(ctx context.Context, request codexresponses.Request) (codexresponses.Result, error) {
	tried := make([]string, 0, 2)
	var lastFailure *codexresponses.Failure
	for {
		account, err := s.acquireCodexAccountForTransport(ctx, request.Model, tried, request.SessionHash, accevents.TransportCompact)
		if err != nil {
			if lastFailure != nil {
				return codexresponses.Result{}, lastFailure
			}
			return codexresponses.Result{}, err
		}
		value, sendErr := s.SendEvent(event.NewEventWithContext(upevents.TopicCompact, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.CompactCommand{
			AccessToken: account.AccessToken, AccountIDHeader: account.AccountIDHeader, Proxy: account.Proxy,
			Body: request.Body, MaxResponseBytes: s.config.MaxUpstreamResponseBytes, SessionHash: request.SessionHash, RemoteCompactionV2: request.RemoteCompactionV2, ResponsesLite: request.ResponsesLite,
		})).Get()
		if sendErr != nil {
			s.releaseCodexAccount(ctx, account.LeaseID)
			return codexresponses.Result{}, codexresponses.NewFailure(codexresponses.KindUpstream, 0, fmt.Errorf("Codex compact upstream unavailable"))
		}
		completed, ok := value.(upevents.CompactResult)
		if !ok {
			s.releaseCodexAccount(ctx, account.LeaseID)
			return codexresponses.Result{}, codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("invalid Codex compact result"))
		}
		if completed.ErrorClass == "" {
			s.mergeCodexUsageHeaders(ctx, account.AccountID, completed.Headers)
			s.recordCodexTransportCapability(ctx, account.AccountID, accevents.TransportCompact, true)
			s.releaseCodexAccount(ctx, account.LeaseID)
			s.recordCodexResult(ctx, account.AccountID, request.Model, true, "", 0, false, "")
			return codexresponses.Result{Body: completed.Body, Headers: toCodexHeaders(completed.Headers)}, nil
		}
		failure := failureFromUpstream(completed.ErrorClass, completed.RetryAfterSeconds, completed.RateLimit, completed.HTTPStatus, completed.SafeError)
		lastFailure = failure
		if transportExplicitlyUnsupported(accevents.TransportCompact, completed.HTTPStatus) {
			s.recordCodexTransportCapability(ctx, account.AccountID, accevents.TransportCompact, false)
			s.releaseCodexAccount(ctx, account.LeaseID)
			tried = append(tried, account.AccountID)
			continue
		}
		s.releaseCodexAccount(ctx, account.LeaseID)
		s.recordCodexResult(ctx, account.AccountID, request.Model, false, string(failure.Kind), failure.RetryAfterSeconds, failure.QuotaExhausted, failure.QuotaResetAt)
		if retryableCodexFailure(failure) {
			tried = append(tried, account.AccountID)
			continue
		}
		return codexresponses.Result{}, failure
	}
}

func (s *Proxy) StartCodexCompact(ctx context.Context, request codexresponses.Request) (<-chan codexresponses.Completion, error) {
	done := make(chan codexresponses.Completion, 1)
	if err := s.BackgroundRoutine().AsyncFunction(func() {
		result, err := s.CompleteCodexCompact(ctx, request)
		done <- codexresponses.Completion{Result: result, Err: err}
		close(done)
	}); err != nil {
		return nil, codexresponses.NewFailure(codexresponses.KindProviderUnavailable, 0, fmt.Errorf("Codex compact background routine unavailable"))
	}
	return done, nil
}

func (s *Proxy) StreamCodexResponses(ctx context.Context, request codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
	tried := make([]string, 0, 2)
	var lastFailure *codexresponses.Failure
	for {
		account, err := s.acquireCodexAccount(ctx, request.Model, tried, request.SessionHash)
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
			s.releaseCodexAccount(ctx, account.LeaseID)
			s.recordCodexResult(ctx, account.AccountID, request.Model, true, "", 0, false, "")
			return nil
		}
		failure, _ := codexresponses.AsFailure(err)
		if failure == nil {
			s.releaseCodexAccount(ctx, account.LeaseID)
			return err
		}
		lastFailure = failure
		if emitted {
			s.releaseCodexAccount(ctx, account.LeaseID)
			s.recordCodexResult(ctx, account.AccountID, request.Model, false, string(failure.Kind), failure.RetryAfterSeconds, failure.QuotaExhausted, failure.QuotaResetAt)
			return err
		}
		if failure.Kind == codexresponses.KindInvalidToken {
			refreshed, refreshErr := s.refreshCodexAccount(ctx, account.AccountID)
			if refreshErr == nil && refreshed.Refreshed {
				err = s.streamCodexOnce(ctx, accevents.AcquireResult{AccountID: refreshed.AccountID, AccessToken: refreshed.AccessToken, AccountIDHeader: refreshed.AccountIDHeader, Proxy: refreshed.Proxy}, request, started, func(line []byte) error {
					emitted = emitted || len(line) > 0
					if emit == nil {
						return nil
					}
					return emit(line)
				})
				if err == nil {
					s.releaseCodexAccount(ctx, account.LeaseID)
					s.recordCodexResult(ctx, account.AccountID, request.Model, true, "", 0, false, "")
					return nil
				}
				failure, _ = codexresponses.AsFailure(err)
				if failure != nil {
					lastFailure = failure
				}
				if emitted {
					s.releaseCodexAccount(ctx, account.LeaseID)
					s.recordCodexResult(ctx, account.AccountID, request.Model, false, string(failure.Kind), failure.RetryAfterSeconds, failure.QuotaExhausted, failure.QuotaResetAt)
					return err
				}
			}
			if refreshErr != nil {
				s.releaseCodexAccount(ctx, account.LeaseID)
				class := refreshFailureClass(refreshed)
				if refreshed.PermanentFailure {
					class = accevents.ErrorInvalidToken
				}
				s.recordCodexResult(ctx, account.AccountID, request.Model, false, class, 0, false, "")
				tried = append(tried, account.AccountID)
				continue
			}
			if refreshed.PermanentFailure {
				s.releaseCodexAccount(ctx, account.LeaseID)
				s.recordCodexResult(ctx, account.AccountID, request.Model, false, accevents.ErrorInvalidToken, 0, false, "")
				tried = append(tried, account.AccountID)
				continue
			}
		}
		if failure == nil {
			s.releaseCodexAccount(ctx, account.LeaseID)
			return err
		}
		if failure.Kind == codexresponses.KindInvalidRequest {
			s.releaseCodexAccount(ctx, account.LeaseID)
			return err
		}
		s.releaseCodexAccount(ctx, account.LeaseID)
		s.recordCodexResult(ctx, account.AccountID, request.Model, false, string(failure.Kind), failure.RetryAfterSeconds, failure.QuotaExhausted, failure.QuotaResetAt)
		if retryableCodexFailure(failure) {
			tried = append(tried, account.AccountID)
			continue
		}
		return err
	}
}

func retryableCodexFailure(failure *codexresponses.Failure) bool {
	if failure == nil {
		return false
	}
	switch failure.Kind {
	case codexresponses.KindInvalidToken, codexresponses.KindRateLimit, codexresponses.KindTimeout, codexresponses.KindNetwork, codexresponses.KindUpstream:
		return true
	default:
		return false
	}
}

func (s *Proxy) acquireCodexAccount(ctx context.Context, model string, exclude []string, sessionHash ...string) (accevents.AcquireResult, error) {
	return s.acquireCodexAccountForTransport(ctx, model, exclude, firstString(sessionHash), accevents.TransportResponses)
}

func (s *Proxy) acquireCodexAccountForTransport(ctx context.Context, model string, exclude []string, sessionHash, transport string) (accevents.AcquireResult, error) {
	command := accevents.AcquireCommand{Model: model, Exclude: exclude, SessionHash: sessionHash, Transport: transport}
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicAcquire, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, command)).Get()
	if err != nil {
		return accevents.AcquireResult{}, codexresponses.NewFailure(codexresponses.KindProviderUnavailable, 0, fmt.Errorf("Codex OAuth account unavailable"))
	}
	account, ok := value.(accevents.AcquireResult)
	if !ok || strings.TrimSpace(account.AccountID) == "" || strings.TrimSpace(account.AccessToken) == "" {
		return accevents.AcquireResult{}, codexresponses.NewFailure(codexresponses.KindProviderUnavailable, 0, fmt.Errorf("Codex OAuth account unavailable"))
	}
	return account, nil
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func transportExplicitlyUnsupported(transport string, status int) bool {
	switch transport {
	case accevents.TransportCompact:
		return status == 404 || status == 405
	case accevents.TransportWebsocket:
		return status == 404 || status == 405 || status == 426
	default:
		return false
	}
}

func (s *Proxy) recordCodexTransportCapability(ctx context.Context, id, transport string, supported bool) {
	_, _ = s.SendEvent(event.NewEventWithContext(accevents.TopicRecordTransportCapability, s.ID(), acccommon.UnitID, event.NewHeader(), context.WithoutCancel(ctx), accevents.RecordTransportCapabilityCommand{
		AccountID: id, Transport: transport, Supported: supported,
	})).Get()
}

func (s *Proxy) releaseCodexAccount(ctx context.Context, leaseID string) {
	if strings.TrimSpace(leaseID) == "" {
		return
	}
	_, _ = s.SendEvent(event.NewEventWithContext(accevents.TopicRelease, s.ID(), acccommon.UnitID, event.NewHeader(), context.WithoutCancel(ctx), accevents.ReleaseCommand{LeaseID: leaseID})).Get()
}

func (s *Proxy) completeCodexOnce(ctx context.Context, account accevents.AcquireResult, request codexresponses.Request) (codexresponses.Result, *codexresponses.Failure) {
	ctx, cancel := codexRequestContext(ctx, s.config.RequestTimeout)
	defer cancel()
	value, err := s.SendEvent(event.NewEventWithContext(upevents.TopicComplete, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.CompleteCommand{AccessToken: account.AccessToken, AccountIDHeader: account.AccountIDHeader, Proxy: account.Proxy, Body: request.Body, MaxResponseBytes: s.config.MaxUpstreamResponseBytes, SessionHash: request.SessionHash, RemoteCompactionV2: request.RemoteCompactionV2, ResponsesLite: request.ResponsesLite})).Get()
	if err != nil {
		return codexresponses.Result{}, codexresponses.NewFailure(codexresponses.KindUpstream, 0, fmt.Errorf("Codex upstream unavailable"))
	}
	completed, ok := value.(upevents.CompleteResult)
	if !ok {
		return codexresponses.Result{}, codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("invalid Codex upstream result"))
	}
	if completed.ErrorClass != "" {
		return codexresponses.Result{}, failureFromUpstream(completed.ErrorClass, completed.RetryAfterSeconds, completed.RateLimit, completed.HTTPStatus, completed.SafeError)
	}
	s.mergeCodexUsageHeaders(ctx, account.AccountID, completed.Headers)
	return codexresponses.Result{Body: completed.Body, Headers: toCodexHeaders(completed.Headers)}, nil
}

func (s *Proxy) streamCodexOnce(ctx context.Context, account accevents.AcquireResult, request codexresponses.Request, started func(codexresponses.StreamStart) error, emit func([]byte) error) error {
	ctx, cancel := codexRequestContext(ctx, s.config.RequestTimeout)
	defer cancel()
	value, err := s.SendEvent(event.NewEventWithContext(upevents.TopicStart, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.StartCommand{AccessToken: account.AccessToken, AccountIDHeader: account.AccountIDHeader, Proxy: account.Proxy, Body: request.Body, MaxLineBytes: s.config.MaxSSELineBytes, SessionHash: request.SessionHash, RemoteCompactionV2: request.RemoteCompactionV2, ResponsesLite: request.ResponsesLite})).Get()
	if err != nil {
		return codexresponses.NewFailure(codexresponses.KindUpstream, 0, fmt.Errorf("Codex stream unavailable"))
	}
	startedUpstream, ok := value.(upevents.StartResult)
	if !ok {
		return codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("invalid Codex stream result"))
	}
	if startedUpstream.ErrorClass != "" {
		return failureFromUpstream(startedUpstream.ErrorClass, startedUpstream.RetryAfterSeconds, startedUpstream.RateLimit, startedUpstream.HTTPStatus, startedUpstream.SafeError)
	}
	s.mergeCodexUsageHeaders(ctx, account.AccountID, startedUpstream.Headers)
	if strings.TrimSpace(startedUpstream.StreamID) == "" {
		return codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("Codex stream id is missing"))
	}
	defer s.SendEvent(event.NewEvent(upevents.TopicCancel, s.ID(), upcommon.UnitID, nil, upevents.CancelCommand{StreamID: startedUpstream.StreamID}))
	clientStarted := false
	for {
		value, pullErr := s.SendEvent(event.NewEventWithContext(upevents.TopicPull, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.PullCommand{StreamID: startedUpstream.StreamID, TimeoutMillis: 1000})).Get()
		if pullErr != nil {
			return codexresponses.NewFailure(codexresponses.KindUpstream, 0, fmt.Errorf("Codex stream pull failed"))
		}
		update, ok := value.(upevents.PullResult)
		if !ok {
			return codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("invalid Codex stream update"))
		}
		if len(update.Data) > 0 {
			if !clientStarted && started != nil {
				if err := started(codexresponses.StreamStart{Headers: toCodexHeaders(startedUpstream.Headers)}); err != nil {
					return clientFailure(err)
				}
				clientStarted = true
			}
			if emit != nil {
				if err := emit(update.Data); err != nil {
					return clientFailure(err)
				}
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

func (s *Proxy) mergeCodexUsageHeaders(ctx context.Context, accountID string, headers []upevents.Header) {
	values := make(map[string]string, len(headers))
	for _, header := range headers {
		values[strings.ToLower(strings.TrimSpace(header.Name))] = strings.TrimSpace(header.Value)
	}
	now := time.Now().UTC()
	snapshot := accevents.AccountUsageSnapshot{ObservedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(codexUsageSnapshotTTL).Format(time.RFC3339)}
	for _, definition := range []struct {
		id, label, prefix string
	}{
		{id: "header-primary", label: "Primary", prefix: "x-codex-primary-"},
		{id: "header-secondary", label: "Secondary", prefix: "x-codex-secondary-"},
	} {
		window := accevents.UsageWindow{ID: definition.id, Label: definition.label}
		used, usedErr := strconv.ParseFloat(values[definition.prefix+"used-percent"], 64)
		minutes, minutesErr := strconv.Atoi(values[definition.prefix+"window-minutes"])
		resetSeconds, resetErr := strconv.Atoi(values[definition.prefix+"reset-after-seconds"])
		if usedErr == nil {
			window.UsedPercent, window.UsedPercentKnown = used, true
			window.LimitReached = used >= 100
		}
		if minutesErr == nil && minutes > 0 {
			window.WindowSeconds = minutes * 60
		}
		if resetErr == nil && resetSeconds > 0 {
			window.ResetAt = now.Add(time.Duration(resetSeconds) * time.Second).Format(time.RFC3339)
		}
		if window.UsedPercentKnown || window.WindowSeconds > 0 || window.ResetAt != "" {
			snapshot.Windows = append(snapshot.Windows, window)
		}
	}
	if len(snapshot.Windows) == 0 {
		return
	}
	_, _ = s.SendEvent(event.NewEventWithContext(accevents.TopicMergeUsageSnapshot, s.ID(), acccommon.UnitID, event.NewHeader(), context.WithoutCancel(ctx), accevents.MergeUsageSnapshotCommand{AccountID: accountID, Snapshot: snapshot})).Get()
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
func failureFromUpstream(class upevents.ErrorClass, retryAfter int, rateLimit upevents.RateLimitObservation, httpStatus int, safeErrors ...upevents.SafeError) *codexresponses.Failure {
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
	if len(safeErrors) > 0 {
		failure.UpstreamType = safeErrors[0].Type
		failure.UpstreamCode = safeErrors[0].Code
		failure.UpstreamParam = safeErrors[0].Param
		failure.UpstreamMessage = safeErrors[0].Message
	}
	return failure
}
func clientFailure(err error) error {
	if _, ok := codexresponses.AsFailure(err); ok {
		return err
	}
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
