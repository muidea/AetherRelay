package biz

import (
	"context"
	"sync"

	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	basebiz "ai-proxy/internal/modules/base/biz"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	metricsevents "ai-proxy/internal/modules/blocks/metricsruntime/pkg/events"
	usageevents "ai-proxy/internal/modules/blocks/usageruntime/pkg/events"
	"ai-proxy/internal/pkg/aiproxyarchive"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetricsport"
	"ai-proxy/internal/pkg/aiproxyusage"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

// ConfigUpdater is the narrow service-side capability required to atomically
// switch proxy configuration. Biz owns the EventHub command; the HTTP adapter
// only supplies this local owner implementation.
type ConfigUpdater interface {
	UpdateConfig(config.Config) error
}

type Proxy struct {
	basebiz.Base
	config   config.Config
	usage    usage.Store
	metrics  metricsport.Port
	recorder *archive.Recorder

	mu                 sync.RWMutex
	updater            ConfigUpdater
	catalogPublisher   CatalogPublisher
	catalog            effectivecatalog.Snapshot
	discoveryMu        sync.Mutex
	codexDiscoveryMu   sync.Mutex
	codexUsageMu       sync.Mutex
	discoveryJobsMu    sync.RWMutex
	codexDiscoveryJobs map[string]proxyevents.CodexDiscoveryProgress
	codexUsageJobs     map[string]proxyevents.CodexUsageProgress
}

func New(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) (*Proxy, *cd.Error) {
	biz := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background), codexDiscoveryJobs: map[string]proxyevents.CodexDiscoveryProgress{}, codexUsageJobs: map[string]proxyevents.CodexUsageProgress{}}
	bootstrap, err := configevents.RequestBootstrap(ctx, biz.EventHub(), biz.ID())
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	usageStore, err := usageevents.RequestStore(ctx, biz.EventHub(), biz.ID())
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	metrics, err := metricsevents.RequestPort(ctx, biz.EventHub(), biz.ID())
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	recorder, err := archive.NewRecorderOptions(bootstrap.Config.InteractionDir, archive.RecorderOptions{
		MaxRounds: bootstrap.Config.InteractionRetention, FullContent: bootstrap.Config.ArchiveFullContent,
	})
	if err != nil {
		return nil, cd.NewError(cd.Unexpected, "init interaction archive: "+err.Error())
	}
	biz.config = bootstrap.Config
	biz.usage = usageStore
	biz.metrics = metrics
	biz.recorder = recorder
	biz.SubscribeFunc(proxyevents.TopicUpdateConfig, biz.handleUpdate)
	biz.SubscribeFunc(proxyevents.TopicEffectiveCatalog, biz.handleEffectiveCatalog)
	biz.SubscribeFunc(proxyevents.TopicStartCodexDiscovery, biz.handleStartCodexDiscovery)
	biz.SubscribeFunc(proxyevents.TopicCodexDiscoveryProgress, biz.handleCodexDiscoveryProgress)
	biz.SubscribeFunc(proxyevents.TopicStartCodexUsageRefresh, biz.handleStartCodexUsageRefresh)
	biz.SubscribeFunc(proxyevents.TopicCodexUsageProgress, biz.handleCodexUsageProgress)
	return biz, nil
}

func (s *Proxy) Run(ctx context.Context) *cd.Error {
	s.startModelDiscovery(ctx)
	return nil
}

func (s *Proxy) Teardown(context.Context) {
	s.UnsubscribeFunc(proxyevents.TopicUpdateConfig)
	s.UnsubscribeFunc(proxyevents.TopicEffectiveCatalog)
	s.UnsubscribeFunc(proxyevents.TopicStartCodexDiscovery)
	s.UnsubscribeFunc(proxyevents.TopicCodexDiscoveryProgress)
	s.UnsubscribeFunc(proxyevents.TopicStartCodexUsageRefresh)
	s.UnsubscribeFunc(proxyevents.TopicCodexUsageProgress)
	s.mu.Lock()
	s.updater = nil
	s.catalogPublisher = nil
	s.catalog = effectivecatalog.Snapshot{}
	s.mu.Unlock()
	s.discoveryJobsMu.Lock()
	s.codexDiscoveryJobs = nil
	s.codexUsageJobs = nil
	s.discoveryJobsMu.Unlock()
	s.config = config.Config{}
	s.usage = nil
	s.metrics = nil
	s.recorder = nil
}

func (s *Proxy) Config() config.Config       { return s.config }
func (s *Proxy) UsageStore() usage.Store     { return s.usage }
func (s *Proxy) Metrics() metricsport.Port   { return s.metrics }
func (s *Proxy) Recorder() *archive.Recorder { return s.recorder }

func (s *Proxy) BindConfigUpdater(updater ConfigUpdater) {
	s.mu.Lock()
	s.updater = updater
	s.mu.Unlock()
}

func (s *Proxy) handleUpdate(ev event.Event, result event.Result) {
	command, ok := ev.Data().(proxyevents.UpdateConfigCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid proxy update command"))
		return
	}
	s.mu.RLock()
	updater := s.updater
	s.mu.RUnlock()
	if updater == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "proxy module is not ready"))
		return
	}
	if err := updater.UpdateConfig(command.Config); err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "update proxy config: "+err.Error()))
		return
	}
	previous := s.EffectiveCatalog()
	s.config = command.Config
	// Keep the constrained auto-discovered projection while the account-pool
	// query runs, so a hot reload never makes ChatGPT Web models disappear.
	s.publishCatalog(effectivecatalog.Reconfigure(command.Config, previous))
	if command.Config.ChatGPTWeb.Enabled || command.Config.CodexOAuth.Enabled {
		s.refreshEffectiveCatalog(ev.Context())
	}
	result.Set(struct{}{}, nil)
}

func (s *Proxy) handleEffectiveCatalog(ev event.Event, result event.Result) {
	if _, ok := ev.Data().(proxyevents.EffectiveCatalogCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid effective catalog command"))
		return
	}
	result.Set(proxyevents.EffectiveCatalogResult{Snapshot: s.EffectiveCatalog()}, nil)
}

func (s *Proxy) handleStartCodexDiscovery(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	command, ok := ev.Data().(proxyevents.StartCodexDiscoveryCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex discovery command"))
		return
	}
	progress, err := s.StartCodexModelDiscovery(context.WithoutCancel(ev.Context()), command.AccountIDs)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(proxyevents.StartCodexDiscoveryResult{Progress: progress}, nil)
}

func (s *Proxy) handleCodexDiscoveryProgress(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	command, ok := ev.Data().(proxyevents.CodexDiscoveryProgressCommand)
	if !ok || command.ProgressID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex discovery progress command"))
		return
	}
	progress, found := s.CodexModelDiscoveryProgress(command.ProgressID)
	if !found {
		result.Set(nil, cd.NewError(cd.NotFound, "Codex model discovery progress not found"))
		return
	}
	result.Set(proxyevents.CodexDiscoveryProgressResult{Progress: progress}, nil)
}

func (s *Proxy) handleStartCodexUsageRefresh(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	command, ok := ev.Data().(proxyevents.StartCodexUsageRefreshCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex usage refresh command"))
		return
	}
	progress, err := s.StartCodexUsageRefresh(context.WithoutCancel(ev.Context()), command.AccountIDs)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(proxyevents.StartCodexUsageRefreshResult{Progress: progress}, nil)
}

func (s *Proxy) handleCodexUsageProgress(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	command, ok := ev.Data().(proxyevents.CodexUsageProgressCommand)
	if !ok || command.ProgressID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex usage progress command"))
		return
	}
	progress, found := s.CodexUsageRefreshProgress(command.ProgressID)
	if !found {
		result.Set(nil, cd.NewError(cd.NotFound, "Codex usage progress not found"))
		return
	}
	result.Set(proxyevents.CodexUsageProgressResult{Progress: progress}, nil)
}
