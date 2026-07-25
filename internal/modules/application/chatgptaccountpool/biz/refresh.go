// Package biz implements account-pool use cases.
package biz

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ai-proxy/internal/modules/application/chatgptaccountpool/internal/oauth"
	"ai-proxy/internal/modules/application/chatgptaccountpool/internal/store"
	events "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
)

const refreshProgressTTL = time.Hour

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
	if !s.refreshing.CompareAndSwap(false, true) {
		return
	}
	defer s.refreshing.Store(false)

	s.refreshOAuthTokens()
	for _, account := range s.store.RefreshCandidates() {
		s.refreshAccount(account)
	}
}

func (s *Account) runManualRefresh(progressID string, tokens []string) {
	if !s.refreshing.CompareAndSwap(false, true) {
		s.finishProgress(progressID, "account refresh already running")
		return
	}
	defer s.refreshing.Store(false)

	// Match the scheduled refresh path: renew eligible OAuth access tokens
	// before requesting ChatGPT Web account information. A manual refresh is
	// expected to recover an expired OAuth access token, not merely report it.
	s.refreshOAuthTokens()
	candidates := s.store.RefreshCandidatesFor(tokens)
	s.replaceProgress(progressID, events.RefreshProgress{ProgressID: progressID, Total: len(candidates), Errors: []events.RefreshError{}})
	for _, account := range candidates {
		if err := s.refreshAccount(account); err != nil {
			s.updateProgress(progressID, account, err.Error(), false)
			continue
		}
		s.updateProgress(progressID, account, "", true)
	}
	s.finishProgress(progressID, "")
}

func (s *Account) runManualRefreshByID(progressID string, ids []string) {
	if !s.refreshing.CompareAndSwap(false, true) {
		s.finishProgress(progressID, "account refresh already running")
		return
	}
	defer s.refreshing.Store(false)
	s.refreshOAuthTokens()
	candidates := s.store.RefreshCandidatesForIDs(ids)
	s.replaceProgress(progressID, events.RefreshProgress{ProgressID: progressID, Total: len(candidates), Errors: []events.RefreshError{}})
	for _, account := range candidates {
		if err := s.refreshAccount(account); err != nil {
			s.updateProgress(progressID, account, err.Error(), false)
			continue
		}
		s.updateProgress(progressID, account, "", true)
	}
	s.finishProgress(progressID, "")
}

func (s *Account) refreshAccount(account events.AccountView) error {
	result := s.SendEvent(event.NewEvent(upevents.TopicGetUserInfo, s.ID(), upcommon.UnitID, nil, upevents.GetUserInfoCommand{
		AccessToken: account.AccessToken,
	}))
	value, err := result.Get()
	if err != nil {
		_ = s.store.RecordRefreshError(account.AccessToken, err.Error())
		return err
	}
	info, ok := value.(upevents.GetUserInfoResult)
	if !ok {
		err := "invalid upstream account status result"
		_ = s.store.RecordRefreshError(account.AccessToken, err)
		return fmt.Errorf("%s", err)
	}
	if _, err := s.store.ApplyUpstreamInfo(account.AccessToken, info.Email, info.PlanType, info.Quota, info.RestoreAt); err != nil {
		_ = s.store.RecordRefreshError(account.AccessToken, err.Error())
		return err
	}
	return nil
}

func (s *Account) refreshOAuthTokens() {
	if s.oauth == nil {
		return
	}
	now := time.Now().UTC()
	for _, candidate := range s.store.TokenRefreshCandidates(now, 24*time.Hour, 72*time.Hour, 6*time.Hour, 3) {
		refreshed, refreshErr := s.oauth.Refresh(context.Background(), oauth.Request{
			RefreshToken: candidate.RefreshToken,
		})
		if refreshErr != nil {
			_ = s.store.RecordTokenRefreshError(candidate.AccessToken, refreshErr.Error())
			continue
		}
		if _, _, err := s.store.ApplyRefreshedToken(candidate.AccessToken, refreshed.AccessToken, refreshed.RefreshToken, refreshed.IDToken); err != nil {
			_ = s.store.RecordTokenRefreshError(candidate.AccessToken, err.Error())
		}
	}
}
