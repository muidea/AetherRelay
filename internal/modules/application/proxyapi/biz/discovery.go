package biz

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
	codexcommon "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/common"
	codexevents "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/events"
	codexupcommon "ai-proxy/internal/modules/blocks/codexupstream/pkg/common"
	codexupevents "ai-proxy/internal/modules/blocks/codexupstream/pkg/events"
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
	if !s.config.ChatGPTWeb.Enabled && !s.config.CodexOAuth.Enabled {
		s.publishCatalog(effectivecatalog.FromStatic(s.config))
		return
	}
	// Initial empty-but-enabled snapshot so the service can start before the
	// first successful account-scoped discovery round completes.
	s.publishCatalog(effectivecatalog.BuildWithCodex(s.config, effectivecatalog.CatalogInput{UpdatedAt: time.Now().UTC().Format(time.RFC3339)}, effectivecatalog.CatalogInput{UpdatedAt: time.Now().UTC().Format(time.RFC3339)}))
	// Kick immediate discovery for each enabled OAuth domain, then schedule
	// periodic full scans and a faster retry watch.
	if s.config.ChatGPTWeb.Enabled {
		_ = s.BackgroundRoutine().AsyncFunction(func() { s.runDiscoveryRound(ctx, false) })
	}
	if s.config.CodexOAuth.Enabled {
		_ = s.BackgroundRoutine().AsyncFunction(func() { s.runCodexDiscoveryRound(ctx, false) })
	}
	s.Timer(ctx, discoveryInterval, 0, func() {
		if s.config.ChatGPTWeb.Enabled {
			s.runDiscoveryRound(ctx, false)
		}
		if s.config.CodexOAuth.Enabled {
			s.runCodexDiscoveryRound(ctx, false)
		}
	})
	s.Timer(ctx, discoveryWatchInterval, discoveryWatchInterval, func() {
		s.watchDiscovery(ctx)
	})
}

func (s *Proxy) watchDiscovery(ctx context.Context) {
	// Always rebuild from durable snapshots so Admin/import races converge even
	// when no upstream round is needed.
	s.refreshEffectiveCatalog(ctx)
	if s.config.ChatGPTWeb.Enabled {
		candidates, err := s.listDiscoveryCandidates(ctx)
		if err == nil {
			for _, candidate := range candidates.Candidates {
				if candidate.DiscoveryDue {
					_ = s.BackgroundRoutine().AsyncFunction(func() { s.runDiscoveryRound(ctx, true) })
					break
				}
			}
		}
	}
	if s.config.CodexOAuth.Enabled {
		candidates, err := s.listCodexDiscoveryCandidates(ctx)
		if err == nil {
			for _, candidate := range candidates.Candidates {
				if candidate.DiscoveryDue {
					_ = s.BackgroundRoutine().AsyncFunction(func() { s.runCodexDiscoveryRound(ctx, true) })
					break
				}
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
	if !s.config.ChatGPTWeb.Enabled {
		return
	}
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
		ops := make([]string, 0, len(model.Operations))
		for _, op := range model.Operations {
			ops = append(ops, string(op))
		}
		if strings.TrimSpace(model.ID) == "" || len(ops) == 0 {
			continue
		}
		snap.Models = append(snap.Models, accevents.AccountModelEntry{
			ID:         model.ID,
			Operations: ops,
			CreatedAt:  model.CreatedAt,
			OwnedBy:    model.OwnedBy,
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
	if !s.config.CodexOAuth.Enabled {
		return
	}
	if !s.codexDiscoveryMu.TryLock() {
		return
	}
	defer s.codexDiscoveryMu.Unlock()

	candidates, err := s.listCodexDiscoveryCandidates(ctx)
	if err != nil {
		slog.Warn("Codex model discovery failed", "stage", "list_candidates", "error", err.Error())
		return
	}
	sem := make(chan struct{}, discoveryConcurrency)
	var wg sync.WaitGroup
	for _, candidate := range candidates.Candidates {
		if strings.TrimSpace(candidate.AccountID) == "" || strings.TrimSpace(candidate.AccessToken) == "" {
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
			s.discoverOneCodexAccount(ctx, cand)
		}()
	}
	wg.Wait()
	s.refreshEffectiveCatalog(ctx)
}

func (s *Proxy) discoverOneCodexAccount(ctx context.Context, candidate codexevents.DiscoveryCandidate) {
	reqCtx, cancel := context.WithTimeout(ctx, discoveryAccountTimeout)
	defer cancel()
	value, err := s.SendEvent(event.NewEventWithContext(codexupevents.TopicListModels, s.ID(), codexupcommon.UnitID, event.NewHeader(), reqCtx, codexupevents.ListModelsCommand{
		AccessToken:     candidate.AccessToken,
		AccountIDHeader: candidate.AccountIDHeader,
		Proxy:           candidate.Proxy,
	})).Get()
	if err != nil {
		s.recordCodexDiscoveryFailure(ctx, candidate, "list_models", err)
		return
	}
	listed, ok := value.(codexupevents.ListModelsResult)
	if !ok {
		s.recordCodexDiscoveryFailure(ctx, candidate, "list_models", fmt.Errorf("invalid model list result"))
		return
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
	}
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

func (s *Proxy) listCodexDiscoveryCandidates(ctx context.Context) (codexevents.ListDiscoveryCandidatesResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(codexevents.TopicListDiscoveryCandidates, s.ID(), codexcommon.UnitID, event.NewHeader(), ctx, codexevents.ListDiscoveryCandidatesCommand{})).Get()
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
	if s.config.ChatGPTWeb.Enabled {
		value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicCatalogSnapshot, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.CatalogSnapshotCommand{})).Get()
		if err != nil {
			slog.Warn("chatgpt model discovery failed", "stage", "catalog_snapshot", "error", err.Error())
		} else if snapshot, ok := value.(accevents.CatalogSnapshotResult); ok {
			chatGPTResult = snapshot
		}
	}
	codexResult := codexevents.CatalogSnapshotResult{}
	if s.config.CodexOAuth.Enabled {
		value, err := s.SendEvent(event.NewEventWithContext(codexevents.TopicCatalogSnapshot, s.ID(), codexcommon.UnitID, event.NewHeader(), ctx, codexevents.CatalogSnapshotCommand{})).Get()
		if err != nil {
			slog.Warn("Codex model discovery failed", "stage", "catalog_snapshot", "error", err.Error())
		} else if snapshot, ok := value.(codexevents.CatalogSnapshotResult); ok {
			codexResult = snapshot
		}
	}
	chatGPTModels := make([]effectivecatalog.PoolModel, 0, len(chatGPTResult.Models))
	for _, model := range chatGPTResult.Models {
		chatGPTModels = append(chatGPTModels, effectivecatalog.PoolModel{
			ID:         model.ID,
			Operations: append([]string(nil), model.Operations...),
			CreatedAt:  model.CreatedAt,
			OwnedBy:    model.OwnedBy,
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
