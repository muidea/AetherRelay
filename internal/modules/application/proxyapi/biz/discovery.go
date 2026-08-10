package biz

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	acccommon "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	codexcommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	codexevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	codexupcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	codexupevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/google/uuid"
	"github.com/muidea/magicCommon/event"
)

const (
	// discoveryInterval is the steady-state full re-scan cadence.
	discoveryInterval = 5 * time.Minute
	// discoveryWatchInterval is a light tick that rebuilds the effective catalog
	// from existing snapshots and starts a discovery round when any account still
	// needs discovery (for example right after account import/OAuth).
	discoveryWatchInterval = 30 * time.Second
	// modelSnapshotTTL bounds how long a successful per-account snapshot remains
	// eligible for routing without a refresh.
	modelSnapshotTTL = 6 * time.Hour
	// discoveryAccountTimeout bounds a single account's models enumeration.
	discoveryAccountTimeout = 45 * time.Second
	// discoveryConcurrency caps concurrent upstream model enumerations.
	discoveryConcurrency = 2
	// codexDiscoveryProgressRetention bounds process-local manual job history.
	codexDiscoveryProgressRetention = 30 * time.Minute
)

// CatalogPublisher is the narrow service-side capability used to atomically
// publish the request-time effective catalog to the HTTP Handler.
type CatalogPublisher interface {
	ReplaceEffectiveCatalog(effectivecatalog.Snapshot)
}

func (s *Proxy) BindCatalogPublisher(publisher CatalogPublisher) {
	s.mu.Lock()
	s.catalogPublisher = publisher
	s.mu.Unlock()
}

func (s *Proxy) startModelDiscovery(ctx context.Context) {
	// Initial empty-but-enabled snapshot so the service can start before the
	// first successful account-scoped discovery round completes.
	s.publishCatalog(effectivecatalog.BuildWithCodex(s.config, effectivecatalog.CatalogInput{UpdatedAt: time.Now().UTC().Format(time.RFC3339)}, effectivecatalog.CatalogInput{UpdatedAt: time.Now().UTC().Format(time.RFC3339)}))
	// Kick immediate discovery for both account domains, then schedule
	// periodic full scans and a faster retry watch.
	_ = s.BackgroundRoutine().AsyncFunction(func() { s.runDiscoveryRound(ctx, false) })
	_ = s.BackgroundRoutine().AsyncFunction(func() { s.runCodexDiscoveryRound(ctx, false) })
	s.Timer(ctx, discoveryInterval, 0, func() {
		s.runDiscoveryRound(ctx, false)
		s.runCodexDiscoveryRound(ctx, false)
	})
	s.Timer(ctx, discoveryWatchInterval, discoveryWatchInterval, func() {
		s.watchDiscovery(ctx)
	})
}

func (s *Proxy) watchDiscovery(ctx context.Context) {
	// Always rebuild from durable snapshots so Admin/import races converge even
	// when no upstream round is needed.
	s.refreshEffectiveCatalog(ctx)
	candidates, err := s.listDiscoveryCandidates(ctx)
	if err == nil {
		for _, candidate := range candidates.Candidates {
			if candidate.DiscoveryDue {
				_ = s.BackgroundRoutine().AsyncFunction(func() { s.runDiscoveryRound(ctx, true) })
				break
			}
		}
	}
	codexCandidates, err := s.listCodexDiscoveryCandidates(ctx, nil)
	if err == nil {
		for _, candidate := range codexCandidates.Candidates {
			if candidate.DiscoveryDue {
				_ = s.BackgroundRoutine().AsyncFunction(func() { s.runCodexDiscoveryRound(ctx, true) })
				break
			}
		}
	}
}

func (s *Proxy) publishCatalog(snap effectivecatalog.Snapshot) {
	s.mu.RLock()
	publisher := s.catalogPublisher
	s.mu.RUnlock()
	if publisher != nil {
		publisher.ReplaceEffectiveCatalog(snap)
	}
	s.mu.Lock()
	s.catalog = snap
	s.mu.Unlock()
}

func (s *Proxy) EffectiveCatalog() effectivecatalog.Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.catalog.StaticModels != nil || s.catalog.BuiltinModels != nil {
		return s.catalog
	}
	return effectivecatalog.FromStatic(s.config)
}

func (s *Proxy) runDiscoveryRound(ctx context.Context, dueOnly bool) {
	// One discovery generation at a time for the process.
	if !s.discoveryMu.TryLock() {
		return
	}
	defer s.discoveryMu.Unlock()

	candidates, err := s.listDiscoveryCandidates(ctx)
	if err != nil {
		slog.Warn("chatgpt model discovery failed", "stage", "list_candidates", "error", err.Error())
		return
	}

	sem := make(chan struct{}, discoveryConcurrency)
	var wg sync.WaitGroup
	for _, candidate := range candidates.Candidates {
		// The five-minute full scan refreshes every healthy account. The faster
		// watch path retries only accounts whose persisted backoff has elapsed.
		if strings.TrimSpace(candidate.AccessToken) == "" || strings.TrimSpace(candidate.AccountID) == "" {
			continue
		}
		if dueOnly && !candidate.DiscoveryDue {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		cand := candidate
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.discoverOneAccount(ctx, cand)
		}()
	}
	wg.Wait()

	// Always re-read the pool catalog after the round so routing sees the union.
	s.refreshEffectiveCatalog(ctx)
}

func (s *Proxy) discoverOneAccount(ctx context.Context, candidate accevents.DiscoveryCandidate) {
	reqCtx, cancel := context.WithTimeout(ctx, discoveryAccountTimeout)
	defer cancel()
	value, err := s.SendEvent(event.NewEventWithContext(upevents.TopicListModels, s.ID(), upcommon.UnitID, event.NewHeader(), reqCtx, upevents.ListModelsCommand{
		AccessToken: candidate.AccessToken,
		Proxy:       candidate.Proxy,
	})).Get()
	if err != nil {
		s.recordDiscoveryFailure(ctx, candidate, "list_models", err)
		return
	}
	listed, ok := value.(upevents.ListModelsResult)
	if !ok {
		s.recordDiscoveryFailure(ctx, candidate, "list_models", fmt.Errorf("invalid model list result"))
		return
	}
	now := time.Now().UTC()
	snap := accevents.AccountModelSnapshot{
		AccountID:    candidate.AccountID,
		DiscoveredAt: now.Format(time.RFC3339),
		ExpiresAt:    now.Add(modelSnapshotTTL).Format(time.RFC3339),
	}
	for _, model := range listed.Models {
		capabilities := make([]string, 0, len(model.Capabilities))
		for _, capability := range model.Capabilities {
			capabilities = append(capabilities, string(capability))
		}
		if strings.TrimSpace(model.ID) == "" || len(capabilities) == 0 {
			continue
		}
		snap.Models = append(snap.Models, accevents.AccountModelEntry{
			ID:           model.ID,
			Capabilities: capabilities,
			CreatedAt:    model.CreatedAt,
			OwnedBy:      model.OwnedBy,
		})
	}
	_, putErr := s.SendEvent(event.NewEventWithContext(accevents.TopicPutModelSnapshot, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.PutModelSnapshotCommand{
		AccountID: candidate.AccountID,
		Snapshot:  snap,
	})).Get()
	if putErr != nil {
		s.recordDiscoveryFailure(ctx, candidate, "put_snapshot", putErr)
	}
}

func (s *Proxy) runCodexDiscoveryRound(ctx context.Context, dueOnly bool) {
	if !s.codexDiscoveryMu.TryLock() {
		return
	}
	defer s.codexDiscoveryMu.Unlock()
	s.runCodexDiscoveryRoundLocked(ctx, dueOnly, nil, "")
}

// StartCodexModelDiscovery starts an explicit, account-scoped model refresh.
// It is intentionally owned by proxyapi: the account pool owns credentials and
// snapshots while codexupstream owns HTTP, and only proxyapi may coordinate the
// two. The returned progress survives only for a bounded time in this process.
func (s *Proxy) StartCodexModelDiscovery(ctx context.Context, accountIDs []string) (proxyevents.CodexDiscoveryProgress, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	progress := proxyevents.CodexDiscoveryProgress{
		ProgressID: uuid.NewString(),
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	s.putCodexDiscoveryProgress(progress)
	requested := uniqueCodexAccountIDs(accountIDs)
	if err := s.BackgroundRoutine().AsyncFunction(func() {
		s.runCodexDiscoveryJob(context.WithoutCancel(ctx), progress.ProgressID, requested)
	}); err != nil {
		s.finishCodexDiscoveryProgress(progress.ProgressID, err.Error())
		return proxyevents.CodexDiscoveryProgress{}, fmt.Errorf("Codex model discovery task unavailable")
	}
	return progress, nil
}

// CodexModelDiscoveryProgress returns a bounded projection of an explicit
// discovery task. The account pool snapshot remains the durable source of
// truth after the task record expires.
func (s *Proxy) CodexModelDiscoveryProgress(progressID string) (proxyevents.CodexDiscoveryProgress, bool) {
	s.discoveryJobsMu.RLock()
	progress, found := s.codexDiscoveryJobs[strings.TrimSpace(progressID)]
	s.discoveryJobsMu.RUnlock()
	if found && progress.Done && progress.CompletedAt != "" {
		if completedAt, err := time.Parse(time.RFC3339, progress.CompletedAt); err == nil && time.Since(completedAt) > codexDiscoveryProgressRetention {
			return proxyevents.CodexDiscoveryProgress{}, false
		}
	}
	return progress, found
}

func (s *Proxy) runCodexDiscoveryJob(ctx context.Context, progressID string, accountIDs []string) {
	// Automatic rounds use TryLock to avoid a timer backlog. An operator-asked
	// round instead waits its turn so a click is never silently dropped.
	s.codexDiscoveryMu.Lock()
	defer s.codexDiscoveryMu.Unlock()
	s.runCodexDiscoveryRoundLocked(ctx, false, accountIDs, progressID)
}

// runCodexDiscoveryRoundLocked performs one discovery generation. Callers
// hold codexDiscoveryMu. Empty progressID denotes the periodic internal round.
func (s *Proxy) runCodexDiscoveryRoundLocked(ctx context.Context, dueOnly bool, accountIDs []string, progressID string) {

	candidates, err := s.listCodexDiscoveryCandidates(ctx, accountIDs)
	if err != nil {
		slog.Warn("Codex model discovery failed", "stage", "list_candidates", "error", err.Error())
		s.finishCodexDiscoveryProgress(progressID, err.Error())
		return
	}
	requested := make(map[string]struct{}, len(accountIDs))
	for _, accountID := range accountIDs {
		requested[accountID] = struct{}{}
	}
	selected := make([]codexevents.DiscoveryCandidate, 0, len(candidates.Candidates))
	for _, candidate := range candidates.Candidates {
		if strings.TrimSpace(candidate.AccountID) == "" || strings.TrimSpace(candidate.AccessToken) == "" {
			continue
		}
		if len(requested) > 0 {
			if _, ok := requested[candidate.AccountID]; !ok {
				continue
			}
		}
		if dueOnly && !candidate.DiscoveryDue {
			continue
		}
		selected = append(selected, candidate)
	}
	s.setCodexDiscoveryTotal(progressID, len(selected), len(requested))
	if len(selected) == 0 {
		s.refreshEffectiveCatalog(ctx)
		s.finishCodexDiscoveryProgress(progressID, "")
		return
	}
	sem := make(chan struct{}, discoveryConcurrency)
	var wg sync.WaitGroup
	for _, candidate := range selected {
		wg.Add(1)
		sem <- struct{}{}
		cand := candidate
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			err := s.discoverOneCodexAccount(ctx, cand)
			s.advanceCodexDiscoveryProgress(progressID, err)
		}()
	}
	wg.Wait()
	s.refreshEffectiveCatalog(ctx)
	s.finishCodexDiscoveryProgress(progressID, "")
}

func (s *Proxy) discoverOneCodexAccount(ctx context.Context, candidate codexevents.DiscoveryCandidate) error {
	reqCtx, cancel := context.WithTimeout(ctx, discoveryAccountTimeout)
	defer cancel()
	value, err := s.SendEvent(event.NewEventWithContext(codexupevents.TopicListModels, s.ID(), codexupcommon.UnitID, event.NewHeader(), reqCtx, codexupevents.ListModelsCommand{
		AccessToken:     candidate.AccessToken,
		AccountIDHeader: candidate.AccountIDHeader,
		Proxy:           candidate.Proxy,
	})).Get()
	if err != nil {
		s.recordCodexDiscoveryFailure(ctx, candidate, "list_models", err)
		return err
	}
	listed, ok := value.(codexupevents.ListModelsResult)
	if !ok {
		err := fmt.Errorf("invalid model list result")
		s.recordCodexDiscoveryFailure(ctx, candidate, "list_models", err)
		return err
	}
	now := time.Now().UTC()
	snapshot := codexevents.AccountModelSnapshot{
		AccountID:    candidate.AccountID,
		DiscoveredAt: now.Format(time.RFC3339),
		ExpiresAt:    now.Add(modelSnapshotTTL).Format(time.RFC3339),
	}
	for _, model := range listed.Models {
		if strings.TrimSpace(model.ID) == "" {
			continue
		}
		snapshot.Models = append(snapshot.Models, codexevents.AccountModelEntry{ID: model.ID, CreatedAt: model.CreatedAt, OwnedBy: model.OwnedBy})
	}
	_, putErr := s.SendEvent(event.NewEventWithContext(codexevents.TopicPutModelSnapshot, s.ID(), codexcommon.UnitID, event.NewHeader(), ctx, codexevents.PutModelSnapshotCommand{AccountID: candidate.AccountID, Snapshot: snapshot})).Get()
	if putErr != nil {
		s.recordCodexDiscoveryFailure(ctx, candidate, "put_snapshot", putErr)
		return putErr
	}
	return nil
}

func uniqueCodexAccountIDs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (s *Proxy) putCodexDiscoveryProgress(progress proxyevents.CodexDiscoveryProgress) {
	s.discoveryJobsMu.Lock()
	if s.codexDiscoveryJobs == nil {
		s.codexDiscoveryJobs = map[string]proxyevents.CodexDiscoveryProgress{}
	}
	now := time.Now().UTC()
	for id, item := range s.codexDiscoveryJobs {
		if !item.Done || item.CompletedAt == "" {
			continue
		}
		completedAt, err := time.Parse(time.RFC3339, item.CompletedAt)
		if err == nil && now.Sub(completedAt) > codexDiscoveryProgressRetention {
			delete(s.codexDiscoveryJobs, id)
		}
	}
	s.codexDiscoveryJobs[progress.ProgressID] = progress
	s.discoveryJobsMu.Unlock()
}

func (s *Proxy) setCodexDiscoveryTotal(progressID string, candidateTotal, requestedTotal int) {
	if strings.TrimSpace(progressID) == "" {
		return
	}
	s.discoveryJobsMu.Lock()
	progress, found := s.codexDiscoveryJobs[progressID]
	if found {
		progress.Total = candidateTotal
		if requestedTotal > 0 {
			progress.Total = requestedTotal
			if rejected := max(0, requestedTotal-candidateTotal); rejected > 0 {
				progress.Processed = rejected
				progress.Failed = rejected
				if candidateTotal == 0 {
					progress.LastError = "none of the requested Codex OAuth accounts is eligible for model discovery"
				} else {
					progress.LastError = "some requested Codex OAuth accounts are not eligible for model discovery"
				}
			}
		} else if candidateTotal == 0 {
			progress.LastError = "no eligible Codex OAuth account was found for model discovery"
		}
		s.codexDiscoveryJobs[progressID] = progress
	}
	s.discoveryJobsMu.Unlock()
}

func (s *Proxy) advanceCodexDiscoveryProgress(progressID string, err error) {
	if strings.TrimSpace(progressID) == "" {
		return
	}
	s.discoveryJobsMu.Lock()
	progress, found := s.codexDiscoveryJobs[progressID]
	if found {
		progress.Processed++
		if err == nil {
			progress.Succeeded++
		} else {
			progress.Failed++
			progress.LastError = boundedDiscoveryError(err.Error())
		}
		s.codexDiscoveryJobs[progressID] = progress
	}
	s.discoveryJobsMu.Unlock()
}

func (s *Proxy) finishCodexDiscoveryProgress(progressID, failure string) {
	if strings.TrimSpace(progressID) == "" {
		return
	}
	s.discoveryJobsMu.Lock()
	progress, found := s.codexDiscoveryJobs[progressID]
	if found {
		if strings.TrimSpace(failure) != "" {
			progress.LastError = boundedDiscoveryError(failure)
			if progress.Total == 0 {
				progress.Failed = 1
			}
		}
		progress.Done = true
		progress.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		s.codexDiscoveryJobs[progressID] = progress
	}
	s.discoveryJobsMu.Unlock()
}

func boundedDiscoveryError(value string) string {
	value = strings.TrimSpace(value)
	const limit = 240
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func (s *Proxy) recordDiscoveryFailure(ctx context.Context, candidate accevents.DiscoveryCandidate, stage string, cause error) {
	if cause == nil {
		return
	}
	// Account IDs are opaque local identifiers; do not log access tokens or
	// upstream response bodies.
	slog.Warn("chatgpt model discovery failed", "stage", stage, "account_id", candidate.AccountID, "error", cause.Error())
	_, err := s.SendEvent(event.NewEventWithContext(accevents.TopicRecordModelDiscoveryFailure, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.RecordModelDiscoveryFailureCommand{
		AccountID: candidate.AccountID,
		Error:     cause.Error(),
	})).Get()
	if err != nil {
		slog.Warn("chatgpt model discovery failure was not recorded", "account_id", candidate.AccountID, "error", err.Error())
	}
}

func (s *Proxy) recordCodexDiscoveryFailure(ctx context.Context, candidate codexevents.DiscoveryCandidate, stage string, cause error) {
	if cause == nil {
		return
	}
	slog.Warn("Codex model discovery failed", "stage", stage, "account_id", candidate.AccountID, "error", cause.Error())
	_, err := s.SendEvent(event.NewEventWithContext(codexevents.TopicRecordModelDiscoveryFailure, s.ID(), codexcommon.UnitID, event.NewHeader(), ctx, codexevents.RecordModelDiscoveryFailureCommand{AccountID: candidate.AccountID, Error: cause.Error()})).Get()
	if err != nil {
		slog.Warn("Codex model discovery failure was not recorded", "account_id", candidate.AccountID, "error", err.Error())
	}
}

func (s *Proxy) listDiscoveryCandidates(ctx context.Context) (accevents.ListDiscoveryCandidatesResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicListDiscoveryCandidates, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.ListDiscoveryCandidatesCommand{})).Get()
	if err != nil {
		return accevents.ListDiscoveryCandidatesResult{}, fmt.Errorf("list discovery candidates failed")
	}
	result, ok := value.(accevents.ListDiscoveryCandidatesResult)
	if !ok {
		return accevents.ListDiscoveryCandidatesResult{}, fmt.Errorf("invalid discovery candidates result")
	}
	return result, nil
}

func (s *Proxy) listCodexDiscoveryCandidates(ctx context.Context, accountIDs []string) (codexevents.ListDiscoveryCandidatesResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(codexevents.TopicListDiscoveryCandidates, s.ID(), codexcommon.UnitID, event.NewHeader(), ctx, codexevents.ListDiscoveryCandidatesCommand{AccountIDs: accountIDs})).Get()
	if err != nil {
		return codexevents.ListDiscoveryCandidatesResult{}, fmt.Errorf("list Codex discovery candidates failed")
	}
	result, ok := value.(codexevents.ListDiscoveryCandidatesResult)
	if !ok {
		return codexevents.ListDiscoveryCandidatesResult{}, fmt.Errorf("invalid Codex discovery candidates result")
	}
	return result, nil
}

func (s *Proxy) refreshEffectiveCatalog(ctx context.Context) {
	chatGPTResult := accevents.CatalogSnapshotResult{}
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicCatalogSnapshot, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.CatalogSnapshotCommand{})).Get()
	if err != nil {
		slog.Warn("chatgpt model discovery failed", "stage", "catalog_snapshot", "error", err.Error())
	} else if snapshot, ok := value.(accevents.CatalogSnapshotResult); ok {
		chatGPTResult = snapshot
	}
	codexResult := codexevents.CatalogSnapshotResult{}
	value, err = s.SendEvent(event.NewEventWithContext(codexevents.TopicCatalogSnapshot, s.ID(), codexcommon.UnitID, event.NewHeader(), ctx, codexevents.CatalogSnapshotCommand{})).Get()
	if err != nil {
		slog.Warn("Codex model discovery failed", "stage", "catalog_snapshot", "error", err.Error())
	} else if snapshot, ok := value.(codexevents.CatalogSnapshotResult); ok {
		codexResult = snapshot
	}
	chatGPTModels := make([]effectivecatalog.PoolModel, 0, len(chatGPTResult.Models))
	for _, model := range chatGPTResult.Models {
		chatGPTModels = append(chatGPTModels, effectivecatalog.PoolModel{
			ID:        model.ID,
			CreatedAt: model.CreatedAt,
			OwnedBy:   model.OwnedBy,
		})
	}
	codexModels := make([]effectivecatalog.PoolModel, 0, len(codexResult.Models))
	for _, model := range codexResult.Models {
		codexModels = append(codexModels, effectivecatalog.PoolModel{ID: model.ID, CreatedAt: model.CreatedAt, OwnedBy: model.OwnedBy})
	}
	snap := effectivecatalog.BuildWithCodex(s.config,
		effectivecatalog.CatalogInput{Version: chatGPTResult.Version, AvailableAccounts: chatGPTResult.AvailableAccounts, Models: chatGPTModels, UpdatedAt: chatGPTResult.UpdatedAt},
		effectivecatalog.CatalogInput{Version: codexResult.Version, AvailableAccounts: codexResult.AvailableAccounts, Models: codexModels, UpdatedAt: codexResult.UpdatedAt},
	)
	s.publishCatalog(snap)
}
