package biz

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	codexcommon "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/common"
	codexevents "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/events"
	codexupcommon "ai-proxy/internal/modules/blocks/codexupstream/pkg/common"
	codexupevents "ai-proxy/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/google/uuid"
	"github.com/muidea/magicCommon/event"
)

const codexUsageSnapshotTTL = 15 * time.Minute

// StartCodexUsageRefresh begins an explicit refresh of the redacted Codex
// usage-window cache. It deliberately does not schedule a high-frequency
// background poll: the upstream observation is operator-facing and independent
// from request routing.
func (s *Proxy) StartCodexUsageRefresh(ctx context.Context, accountIDs []string) (proxyevents.CodexUsageProgress, error) {
	if !s.config.CodexOAuth.Enabled {
		return proxyevents.CodexUsageProgress{}, fmt.Errorf("Codex OAuth is not enabled")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	progress := proxyevents.CodexUsageProgress{
		ProgressID: uuid.NewString(),
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	s.putCodexUsageProgress(progress)
	requested := uniqueCodexAccountIDs(accountIDs)
	if err := s.BackgroundRoutine().AsyncFunction(func() {
		s.runCodexUsageRefreshJob(context.WithoutCancel(ctx), progress.ProgressID, requested)
	}); err != nil {
		s.finishCodexUsageProgress(progress.ProgressID, err.Error())
		return proxyevents.CodexUsageProgress{}, fmt.Errorf("Codex usage refresh task unavailable")
	}
	return progress, nil
}

func (s *Proxy) CodexUsageRefreshProgress(progressID string) (proxyevents.CodexUsageProgress, bool) {
	s.discoveryJobsMu.RLock()
	progress, found := s.codexUsageJobs[strings.TrimSpace(progressID)]
	s.discoveryJobsMu.RUnlock()
	if found && progress.Done && progress.CompletedAt != "" {
		if completedAt, err := time.Parse(time.RFC3339, progress.CompletedAt); err == nil && time.Since(completedAt) > codexDiscoveryProgressRetention {
			return proxyevents.CodexUsageProgress{}, false
		}
	}
	return progress, found
}

func (s *Proxy) runCodexUsageRefreshJob(ctx context.Context, progressID string, accountIDs []string) {
	// A manual refresh waits behind another manual refresh to prevent a click
	// storm from issuing duplicate account-scoped upstream requests.
	s.codexUsageMu.Lock()
	defer s.codexUsageMu.Unlock()

	candidates, err := s.listCodexUsageCandidates(ctx, accountIDs)
	if err != nil {
		s.finishCodexUsageProgress(progressID, err.Error())
		return
	}
	requested := len(accountIDs) > 0
	s.setCodexUsageTotal(progressID, len(candidates.Candidates), requested && len(candidates.Candidates) == 0)
	if len(candidates.Candidates) == 0 {
		s.finishCodexUsageProgress(progressID, "")
		return
	}

	sem := make(chan struct{}, discoveryConcurrency)
	var wg sync.WaitGroup
	for _, candidate := range candidates.Candidates {
		candidate := candidate
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			err := s.refreshOneCodexUsage(ctx, candidate)
			s.advanceCodexUsageProgress(progressID, err)
		}()
	}
	wg.Wait()
	s.finishCodexUsageProgress(progressID, "")
}

func (s *Proxy) refreshOneCodexUsage(ctx context.Context, candidate codexevents.UsageCandidate) error {
	usage, err := s.getCodexUsage(ctx, candidate)
	if err != nil {
		return s.recordCodexUsageFailure(ctx, candidate.AccountID, "upstream")
	}
	if usage.ErrorClass == codexupevents.ErrorInvalidToken {
		refreshed, refreshErr := s.refreshCodexUsageCredential(ctx, candidate.AccountID)
		if refreshErr != nil || !refreshed.Refreshed || strings.TrimSpace(refreshed.AccessToken) == "" {
			return s.recordCodexUsageFailure(ctx, candidate.AccountID, "invalid_token")
		}
		candidate.AccessToken = refreshed.AccessToken
		candidate.AccountIDHeader = refreshed.AccountIDHeader
		candidate.Proxy = refreshed.Proxy
		usage, err = s.getCodexUsage(ctx, candidate)
		if err != nil {
			return s.recordCodexUsageFailure(ctx, candidate.AccountID, "upstream")
		}
	}
	if usage.ErrorClass != "" {
		return s.recordCodexUsageFailure(ctx, candidate.AccountID, string(usage.ErrorClass))
	}
	now := time.Now().UTC()
	snapshot := codexevents.AccountUsageSnapshot{
		PlanType:   usage.PlanType,
		ObservedAt: now.Format(time.RFC3339),
		ExpiresAt:  now.Add(codexUsageSnapshotTTL).Format(time.RFC3339),
		Windows:    make([]codexevents.UsageWindow, 0, len(usage.Windows)),
	}
	for _, window := range usage.Windows {
		snapshot.Windows = append(snapshot.Windows, codexevents.UsageWindow{
			ID:               window.ID,
			Label:            window.Label,
			UsedPercent:      window.UsedPercent,
			UsedPercentKnown: window.UsedPercentKnown,
			WindowSeconds:    window.WindowSeconds,
			ResetAt:          window.ResetAt,
			Allowed:          window.Allowed,
			AllowedKnown:     window.AllowedKnown,
			LimitReached:     window.LimitReached,
		})
	}
	_, putErr := s.SendEvent(event.NewEventWithContext(codexevents.TopicPutUsageSnapshot, s.ID(), codexcommon.UnitID, event.NewHeader(), ctx, codexevents.PutUsageSnapshotCommand{AccountID: candidate.AccountID, Snapshot: snapshot})).Get()
	if putErr != nil {
		return s.recordCodexUsageFailure(ctx, candidate.AccountID, "storage")
	}
	return nil
}

func (s *Proxy) getCodexUsage(ctx context.Context, candidate codexevents.UsageCandidate) (codexupevents.GetUsageResult, error) {
	reqCtx, cancel := context.WithTimeout(ctx, discoveryAccountTimeout)
	defer cancel()
	value, err := s.SendEvent(event.NewEventWithContext(codexupevents.TopicGetUsage, s.ID(), codexupcommon.UnitID, event.NewHeader(), reqCtx, codexupevents.GetUsageCommand{
		AccessToken:     candidate.AccessToken,
		AccountIDHeader: candidate.AccountIDHeader,
		Proxy:           candidate.Proxy,
	})).Get()
	if err != nil {
		return codexupevents.GetUsageResult{}, err
	}
	usage, ok := value.(codexupevents.GetUsageResult)
	if !ok {
		return codexupevents.GetUsageResult{}, fmt.Errorf("invalid Codex usage result")
	}
	return usage, nil
}

// refreshCodexUsageCredential keeps a 401-driven refresh/retry within the
// existing account-pool owner. Only proxyapi sees the transient credential.
func (s *Proxy) refreshCodexUsageCredential(ctx context.Context, accountID string) (codexevents.RefreshTokenResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(codexevents.TopicRefreshToken, s.ID(), codexcommon.UnitID, event.NewHeader(), ctx, codexevents.RefreshTokenCommand{AccountID: accountID})).Get()
	if err != nil {
		return codexevents.RefreshTokenResult{}, err
	}
	result, ok := value.(codexevents.RefreshTokenResult)
	if !ok {
		return codexevents.RefreshTokenResult{}, fmt.Errorf("invalid Codex usage refresh credential result")
	}
	return result, nil
}

func (s *Proxy) recordCodexUsageFailure(ctx context.Context, accountID, class string) error {
	class = boundedCodexUsageError(class)
	_, err := s.SendEvent(event.NewEventWithContext(codexevents.TopicRecordUsageFailure, s.ID(), codexcommon.UnitID, event.NewHeader(), ctx, codexevents.RecordUsageFailureCommand{
		AccountID: accountID,
		Error:     class,
	})).Get()
	if err != nil {
		return fmt.Errorf("record Codex usage failure")
	}
	return fmt.Errorf("Codex usage refresh failed: %s", class)
}

func (s *Proxy) listCodexUsageCandidates(ctx context.Context, accountIDs []string) (codexevents.ListUsageCandidatesResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(codexevents.TopicListUsageCandidates, s.ID(), codexcommon.UnitID, event.NewHeader(), ctx, codexevents.ListUsageCandidatesCommand{AccountIDs: accountIDs})).Get()
	if err != nil {
		return codexevents.ListUsageCandidatesResult{}, fmt.Errorf("list Codex usage candidates failed")
	}
	result, ok := value.(codexevents.ListUsageCandidatesResult)
	if !ok {
		return codexevents.ListUsageCandidatesResult{}, fmt.Errorf("invalid Codex usage candidate result")
	}
	return result, nil
}

func (s *Proxy) putCodexUsageProgress(progress proxyevents.CodexUsageProgress) {
	s.discoveryJobsMu.Lock()
	if s.codexUsageJobs == nil {
		s.codexUsageJobs = map[string]proxyevents.CodexUsageProgress{}
	}
	s.pruneCodexProgressLocked(time.Now().UTC())
	s.codexUsageJobs[progress.ProgressID] = progress
	s.discoveryJobsMu.Unlock()
}

func (s *Proxy) setCodexUsageTotal(progressID string, total int, noEligibleSelected bool) {
	if strings.TrimSpace(progressID) == "" {
		return
	}
	s.discoveryJobsMu.Lock()
	progress, found := s.codexUsageJobs[progressID]
	if found {
		progress.Total = total
		if total == 0 {
			if noEligibleSelected {
				progress.LastError = "none of the requested Codex OAuth accounts is eligible for usage refresh"
			} else {
				progress.LastError = "no eligible Codex OAuth account was found for usage refresh"
			}
		}
		s.codexUsageJobs[progressID] = progress
	}
	s.discoveryJobsMu.Unlock()
}

func (s *Proxy) advanceCodexUsageProgress(progressID string, err error) {
	if strings.TrimSpace(progressID) == "" {
		return
	}
	s.discoveryJobsMu.Lock()
	progress, found := s.codexUsageJobs[progressID]
	if found {
		progress.Processed++
		if err == nil {
			progress.Succeeded++
		} else {
			progress.Failed++
			progress.LastError = boundedCodexUsageError(err.Error())
		}
		s.codexUsageJobs[progressID] = progress
	}
	s.discoveryJobsMu.Unlock()
}

func (s *Proxy) finishCodexUsageProgress(progressID, failure string) {
	if strings.TrimSpace(progressID) == "" {
		return
	}
	s.discoveryJobsMu.Lock()
	progress, found := s.codexUsageJobs[progressID]
	if found {
		if failure = boundedCodexUsageError(failure); failure != "" {
			progress.LastError = failure
			if progress.Total == 0 {
				progress.Failed = 1
			}
		}
		progress.Done = true
		progress.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		s.codexUsageJobs[progressID] = progress
	}
	s.discoveryJobsMu.Unlock()
}

func (s *Proxy) pruneCodexProgressLocked(now time.Time) {
	for id, item := range s.codexDiscoveryJobs {
		if item.Done && item.CompletedAt != "" {
			if completedAt, err := time.Parse(time.RFC3339, item.CompletedAt); err == nil && now.Sub(completedAt) > codexDiscoveryProgressRetention {
				delete(s.codexDiscoveryJobs, id)
			}
		}
	}
	for id, item := range s.codexUsageJobs {
		if item.Done && item.CompletedAt != "" {
			if completedAt, err := time.Parse(time.RFC3339, item.CompletedAt); err == nil && now.Sub(completedAt) > codexDiscoveryProgressRetention {
				delete(s.codexUsageJobs, id)
			}
		}
	}
}

func boundedCodexUsageError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 160 {
		return value[:160]
	}
	return value
}
