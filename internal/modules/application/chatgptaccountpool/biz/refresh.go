// Package biz implements account-pool use cases.
package biz

import (
	"context"
	"fmt"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/chatgptaccountpool/internal/oauth"
	"aetherrelay/internal/modules/application/chatgptaccountpool/internal/store"
	events "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
)

const (
	refreshProgressTTL        = time.Hour
	accountInfoRefreshTimeout = 45 * time.Second
)

func (s *Account) putProgress(progress events.RefreshProgress) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	s.pruneProgressLocked(time.Now())
	s.progress[progress.ProgressID] = progress
	s.progressAt[progress.ProgressID] = time.Now()
}

func (s *Account) replaceProgress(progressID string, progress events.RefreshProgress) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	if _, found := s.progress[progressID]; !found {
		return
	}
	s.progress[progressID] = progress
	s.progressAt[progressID] = time.Now()
}

func (s *Account) getProgress(progressID string) (events.RefreshProgress, bool) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	s.pruneProgressLocked(time.Now())
	progress, found := s.progress[progressID]
	return progress, found
}

func (s *Account) finishProgress(progressID, message string) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	progress, found := s.progress[progressID]
	if !found {
		return
	}
	progress.Done = true
	progress.Error = strings.TrimSpace(message)
	s.progress[progressID] = progress
	s.progressAt[progressID] = time.Now()
}

func (s *Account) updateProgress(progressID string, account events.AccountView, message string, refreshed bool) {
	current := account
	for _, item := range s.store.List() {
		if item.ID == account.ID {
			current = item
			break
		}
	}
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	progress, found := s.progress[progressID]
	if !found {
		return
	}
	progress.Processed++
	progress.TotalQuota += max(0, current.Quota)
	switch current.Status {
	case store.StatusLimited:
		progress.StatusCounts.Limited++
	case store.StatusAbnormal:
		progress.StatusCounts.Abnormal++
	case store.StatusDisabled:
		progress.StatusCounts.Disabled++
	default:
		progress.StatusCounts.Normal++
	}
	if refreshed {
		progress.Refreshed++
	}
	if message = strings.TrimSpace(message); message != "" {
		progress.Errors = append(progress.Errors, events.RefreshError{AccountID: current.ID, Error: boundedRefreshError(message)})
	}
	s.progress[progressID] = progress
	s.progressAt[progressID] = time.Now()
}

func (s *Account) pruneProgressLocked(now time.Time) {
	for progressID, updated := range s.progressAt {
		if now.Sub(updated) <= refreshProgressTTL {
			continue
		}
		delete(s.progressAt, progressID)
		delete(s.progress, progressID)
	}
}

func boundedRefreshError(message string) string {
	const limit = 512
	if len(message) <= limit {
		return message
	}
	return message[:limit]
}

// refreshAccounts is deliberately single-flight. A slow upstream must not
// queue overlapping account-wide scans on the framework BackgroundRoutine.
func (s *Account) refreshAccounts() {
	if s.stopping.Load() {
		return
	}
	if !s.refreshing.CompareAndSwap(false, true) {
		return
	}
	defer s.refreshing.Store(false)

	_ = s.refreshOAuthTokens(nil)
	if s.stopping.Load() {
		return
	}
	for _, account := range s.store.RefreshCandidates() {
		if s.stopping.Load() {
			return
		}
		s.refreshAccount(account)
	}
}

func (s *Account) runManualRefresh(progressID string, tokens []string) {
	if s.stopping.Load() {
		s.finishProgress(progressID, "account pool is shutting down")
		return
	}
	if !s.refreshing.CompareAndSwap(false, true) {
		s.finishProgress(progressID, "account refresh already running")
		return
	}
	defer s.refreshing.Store(false)

	// Match the scheduled refresh path: renew eligible OAuth access tokens
	// before requesting ChatGPT Web account information. A manual refresh is
	// expected to recover an expired OAuth access token, not merely report it.
	selectedIDs := s.store.AccountIDsForAccessTokens(tokens)
	oauthFailures := s.refreshOAuthTokens(selectedIDs)
	if s.stopping.Load() {
		s.finishProgress(progressID, "account pool is shutting down")
		return
	}
	candidates := s.store.RefreshCandidatesFor(tokens)
	s.replaceProgress(progressID, events.RefreshProgress{ProgressID: progressID, Total: len(candidates), Errors: []events.RefreshError{}})
	if len(candidates) == 0 {
		s.finishProgress(progressID, "no refreshable accounts found")
		return
	}
	for _, account := range candidates {
		if s.stopping.Load() {
			s.finishProgress(progressID, "account pool is shutting down")
			return
		}
		err := s.refreshAccount(account)
		if oauthFailure, found := oauthFailures[account.ID]; found {
			if err != nil {
				err = fmt.Errorf("account information refresh failed; OAuth credential renewal failed (%s): %w", oauthFailure, err)
			} else {
				err = fmt.Errorf("account information refreshed, but OAuth credential renewal failed (%s)", oauthFailure)
			}
		}
		if s.stopping.Load() {
			s.finishProgress(progressID, "account pool is shutting down")
			return
		}
		if err != nil {
			s.updateProgress(progressID, account, err.Error(), false)
			continue
		}
		s.updateProgress(progressID, account, "", true)
	}
	s.finishProgress(progressID, "")
}

func (s *Account) runManualRefreshByID(progressID string, ids []string) {
	if s.stopping.Load() {
		s.finishProgress(progressID, "account pool is shutting down")
		return
	}
	if !s.refreshing.CompareAndSwap(false, true) {
		s.finishProgress(progressID, "account refresh already running")
		return
	}
	defer s.refreshing.Store(false)
	selectedIDs := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			selectedIDs[id] = struct{}{}
		}
	}
	oauthFailures := s.refreshOAuthTokens(selectedIDs)
	if s.stopping.Load() {
		s.finishProgress(progressID, "account pool is shutting down")
		return
	}
	candidates := s.store.RefreshCandidatesForIDs(ids)
	s.replaceProgress(progressID, events.RefreshProgress{ProgressID: progressID, Total: len(candidates), Errors: []events.RefreshError{}})
	if len(candidates) == 0 {
		s.finishProgress(progressID, "no refreshable accounts found")
		return
	}
	for _, account := range candidates {
		if s.stopping.Load() {
			s.finishProgress(progressID, "account pool is shutting down")
			return
		}
		err := s.refreshAccount(account)
		if oauthFailure, found := oauthFailures[account.ID]; found {
			if err != nil {
				err = fmt.Errorf("account information refresh failed; OAuth credential renewal failed (%s): %w", oauthFailure, err)
			} else {
				err = fmt.Errorf("account information refreshed, but OAuth credential renewal failed (%s)", oauthFailure)
			}
		}
		if s.stopping.Load() {
			s.finishProgress(progressID, "account pool is shutting down")
			return
		}
		if err != nil {
			s.updateProgress(progressID, account, err.Error(), false)
			continue
		}
		s.updateProgress(progressID, account, "", true)
	}
	s.finishProgress(progressID, "")
}

func (s *Account) refreshAccount(account events.AccountView) error {
	if s.stopping.Load() {
		return context.Canceled
	}
	ctx, cancel := context.WithTimeout(s.shutdownCtx, accountInfoRefreshTimeout)
	defer cancel()
	result := s.SendEvent(event.NewEventWithContext(upevents.TopicGetUserInfo, s.ID(), upcommon.UnitID, nil, ctx, upevents.GetUserInfoCommand{
		AccessToken: account.AccessToken,
		Proxy:       account.Proxy,
	}))
	value, err := result.Get()
	if err != nil {
		if s.stopping.Load() {
			return err
		}
		_ = s.store.RecordRefreshError(account.AccessToken, err.Error())
		return err
	}
	info, ok := value.(upevents.GetUserInfoResult)
	if !ok {
		err := "invalid upstream account status result"
		if s.stopping.Load() {
			return context.Canceled
		}
		_ = s.store.RecordRefreshError(account.AccessToken, err)
		return fmt.Errorf("%s", err)
	}
	if s.stopping.Load() {
		return context.Canceled
	}
	if _, err := s.store.ApplyUpstreamInfo(account.AccessToken, info.Email, info.PlanType, info.Quota, info.RestoreAt); err != nil {
		if s.stopping.Load() {
			return err
		}
		_ = s.store.RecordRefreshError(account.AccessToken, err.Error())
		return err
	}
	return nil
}

// refreshOAuthTokens renews due OAuth credentials and returns failures keyed
// by stable account ID. Account information may still be readable with the
// current access token, but a manual refresh must expose a credential failure
// instead of reporting the whole operation as successful.
func (s *Account) refreshOAuthTokens(selectedIDs map[string]struct{}) map[string]string {
	failures := make(map[string]string)
	if s.oauth == nil || s.stopping.Load() {
		return failures
	}
	now := time.Now().UTC()
	for _, candidate := range s.store.TokenRefreshCandidates(now, 24*time.Hour, 72*time.Hour, 6*time.Hour, 3) {
		if s.stopping.Load() {
			return failures
		}
		if len(selectedIDs) > 0 {
			account, found := s.store.ViewForAccessToken(candidate.AccessToken)
			if !found {
				continue
			}
			if _, selected := selectedIDs[account.ID]; !selected {
				continue
			}
		}
		// Scheduled and request-driven renewal must share the same per-account
		// flight. Otherwise an expired token under traffic can consume a
		// rotating refresh token twice concurrently.
		_, err := s.refreshTextToken(candidate.AccessToken)
		if err != nil {
			if account, found := s.store.ViewForAccessToken(candidate.AccessToken); found {
				failures[account.ID] = oauth.FailureClass(err)
			}
		}
	}
	return failures
}

// refreshTextToken is the request-driven counterpart of the scheduled OAuth
// renewal loop. A single invalid-token result may be an expired access token,
// so the caller gets one safe refresh-and-retry opportunity before the account
// is marked abnormal. Refresh credentials remain in Store and never cross the
// account-pool EventHub boundary.
func (s *Account) refreshTextToken(accessToken string) (events.RefreshTextTokenResult, error) {
	credential, found := s.store.OAuthRefreshCredentialFor(accessToken)
	if !found {
		// A model-list 401 plus no stored refresh credential is conclusive for
		// this access token. Mark it as a typed permanent failure so callers can
		// retire the account rather than repeatedly retrying an unusable token.
		return events.RefreshTextTokenResult{}, &oauth.Error{Class: "invalid_token"}
	}
	if current, ok := s.store.ViewForAccessToken(accessToken); ok && credential.AccessToken != strings.TrimSpace(accessToken) {
		// Another in-flight request already rotated this credential. Reuse the
		// successor rather than spending the newly rotated refresh token again.
		return events.RefreshTextTokenResult{AccessToken: current.AccessToken, Account: current, Refreshed: true}, nil
	}

	s.textRefreshMu.Lock()
	if flight := s.textRefreshes[credential.AccessToken]; flight != nil {
		s.textRefreshMu.Unlock()
		<-flight.done
		return flight.result, flight.err
	}
	flight := &textRefreshFlight{done: make(chan struct{})}
	s.textRefreshes[credential.AccessToken] = flight
	s.textRefreshMu.Unlock()

	result, err := s.refreshTextTokenOnce(credential)

	s.textRefreshMu.Lock()
	flight.result, flight.err = result, err
	delete(s.textRefreshes, credential.AccessToken)
	close(flight.done)
	s.textRefreshMu.Unlock()
	return result, err
}

func (s *Account) refreshTextTokenOnce(credential store.OAuthRefreshCredential) (events.RefreshTextTokenResult, error) {
	if s.oauth == nil || s.stopping.Load() {
		// A local lifecycle problem says nothing about the credential itself.
		// Keep the account eligible for a later discovery retry.
		return events.RefreshTextTokenResult{}, &oauth.Error{Class: "unavailable", Retryable: true}
	}
	refreshed, err := s.oauth.Refresh(s.shutdownCtx, oauth.Request{RefreshToken: credential.RefreshToken, Proxy: credential.Proxy})
	if err != nil {
		if !s.stopping.Load() {
			_ = s.store.RecordTokenRefreshFailure(credential.AccessToken, oauth.FailureClass(err))
		}
		return events.RefreshTextTokenResult{}, fmt.Errorf("refresh oauth access token: %w", err)
	}
	if s.stopping.Load() {
		return events.RefreshTextTokenResult{}, context.Canceled
	}
	accessToken, _, err := s.store.ApplyRefreshedToken(credential.AccessToken, refreshed.AccessToken, refreshed.RefreshToken, refreshed.IDToken)
	if err != nil {
		if !s.stopping.Load() {
			_ = s.store.RecordTokenRefreshFailure(credential.AccessToken, "unavailable")
		}
		return events.RefreshTextTokenResult{}, fmt.Errorf("apply refreshed oauth access token: %w", err)
	}
	account, found := s.store.ViewForAccessToken(accessToken)
	if !found {
		return events.RefreshTextTokenResult{}, fmt.Errorf("refreshed account not found")
	}
	return events.RefreshTextTokenResult{AccessToken: accessToken, Account: account, Refreshed: true}, nil
}
