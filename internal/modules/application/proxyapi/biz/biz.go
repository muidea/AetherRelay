package biz

import (
	"context"
	"sync"

	"ai-proxy/internal/modules/application/proxyapi/internal/searchhistory"
	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	basebiz "ai-proxy/internal/modules/base/biz"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	metricsevents "ai-proxy/internal/modules/blocks/metricsruntime/pkg/events"
	usageevents "ai-proxy/internal/modules/blocks/usageruntime/pkg/events"
	"ai-proxy/internal/pkg/aiproxyarchive"
	"ai-proxy/internal/pkg/aiproxyclientauth"
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

// FeatureExecutor is implemented by proxyapi's local HTTP service. Other
// modules reach it only through proxy-owned typed EventHub commands.
type FeatureExecutor interface {
	FeatureCatalog(context.Context) proxyevents.FeatureCatalogResult
	ExecuteFeatureText(context.Context, proxyevents.ExecuteFeatureTextCommand) (proxyevents.ExecuteFeatureTextResult, error)
	ExecuteFeatureSearch(context.Context, proxyevents.ExecuteFeatureSearchCommand) (proxyevents.ExecuteFeatureSearchResult, error)
	ExecuteFeatureImage(context.Context, proxyevents.ExecuteFeatureImageCommand) (proxyevents.ExecuteFeatureImageResult, error)
}

type ClientKeyRuntime interface {
	PrepareClientKeyIndex(map[string]usage.ClientAPIKeyRecord) (*clientauth.Index, error)
	ActivateClientKeyIndex(*clientauth.Index)
}

type Proxy struct {
	basebiz.Base
	config   config.Config
	usage    usage.Store
	metrics  metricsport.Port
	recorder *archive.Recorder
	history  *searchhistory.Store

	mu                 sync.RWMutex
	updater            ConfigUpdater
	featureExecutor    FeatureExecutor
	clientKeyRuntime   ClientKeyRuntime
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
		MaxRounds: bootstrap.Config.InteractionRetention, FullContent: bootstrap.Config.ArchiveFullContent, ScopeByAPIKey: true,
	})
	if err != nil {
		return nil, cd.NewError(cd.Unexpected, "init interaction archive: "+err.Error())
	}
	history, openErr := searchhistory.Open(bootstrap.Config.State.Database, bootstrap.Config.State.MemoryLimit, bootstrap.Config.State.Threads, searchhistory.Config{})
	if openErr != nil {
		return nil, cd.NewError(cd.Unexpected, "open web search history: "+openErr.Error())
	}
	biz.history = history
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
	biz.SubscribeFunc(proxyevents.TopicFeatureCatalog, biz.handleFeatureCatalog)
	biz.SubscribeFunc(proxyevents.TopicExecuteFeatureText, biz.handleExecuteFeatureText)
	biz.SubscribeFunc(proxyevents.TopicExecuteFeatureSearch, biz.handleExecuteFeatureSearch)
	biz.SubscribeFunc(proxyevents.TopicListFeatureSearchHistory, biz.handleListFeatureSearchHistory)
	biz.SubscribeFunc(proxyevents.TopicGetFeatureSearchHistory, biz.handleGetFeatureSearchHistory)
	biz.SubscribeFunc(proxyevents.TopicExecuteFeatureImage, biz.handleExecuteFeatureImage)
	biz.SubscribeFunc(proxyevents.TopicPrepareClientKeyIndex, biz.handlePrepareClientKeyIndex)
	biz.SubscribeFunc(proxyevents.TopicActivateClientKeyIndex, biz.handleActivateClientKeyIndex)
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
	s.UnsubscribeFunc(proxyevents.TopicFeatureCatalog)
	s.UnsubscribeFunc(proxyevents.TopicExecuteFeatureText)
	s.UnsubscribeFunc(proxyevents.TopicExecuteFeatureSearch)
	s.UnsubscribeFunc(proxyevents.TopicListFeatureSearchHistory)
	s.UnsubscribeFunc(proxyevents.TopicGetFeatureSearchHistory)
	s.UnsubscribeFunc(proxyevents.TopicExecuteFeatureImage)
	s.UnsubscribeFunc(proxyevents.TopicPrepareClientKeyIndex)
	s.UnsubscribeFunc(proxyevents.TopicActivateClientKeyIndex)
	s.mu.Lock()
	s.updater = nil
	s.featureExecutor = nil
	s.clientKeyRuntime = nil
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
	if s.history != nil {
		_ = s.history.Close()
		s.history = nil
	}
}

func (s *Proxy) BindFeatureExecutor(executor FeatureExecutor) {
	s.mu.Lock()
	s.featureExecutor = executor
	s.mu.Unlock()
}

func (s *Proxy) BindClientKeyRuntime(runtime ClientKeyRuntime) {
	s.mu.Lock()
	s.clientKeyRuntime = runtime
	s.mu.Unlock()
}

func (s *Proxy) handlePrepareClientKeyIndex(ev event.Event, result event.Result) {
	command, ok := ev.Data().(proxyevents.PrepareClientKeyIndexCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid client key index command"))
		return
	}
	s.mu.RLock()
	runtime := s.clientKeyRuntime
	s.mu.RUnlock()
	if runtime == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "client key runtime is unavailable"))
		return
	}
	index, err := runtime.PrepareClientKeyIndex(command.Records)
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	result.Set(proxyevents.PrepareClientKeyIndexResult{Index: index}, nil)
}

func (s *Proxy) handleActivateClientKeyIndex(ev event.Event, result event.Result) {
	command, ok := ev.Data().(proxyevents.ActivateClientKeyIndexCommand)
	if !ok || command.Index == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid client key activation command"))
		return
	}
	s.mu.RLock()
	runtime := s.clientKeyRuntime
	s.mu.RUnlock()
	if runtime == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "client key runtime is unavailable"))
		return
	}
	runtime.ActivateClientKeyIndex(command.Index)
	result.Set(struct{}{}, nil)
}

func (s *Proxy) handleFeatureCatalog(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	s.mu.RLock()
	executor := s.featureExecutor
	s.mu.RUnlock()
	if executor == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "feature executor is unavailable"))
		return
	}
	result.Set(executor.FeatureCatalog(ev.Context()), nil)
}

func (s *Proxy) handleExecuteFeatureText(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(proxyevents.ExecuteFeatureTextCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid feature text command"))
		return
	}
	s.mu.RLock()
	executor := s.featureExecutor
	s.mu.RUnlock()
	if executor == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "feature executor is unavailable"))
		return
	}
	out, err := executor.ExecuteFeatureText(ev.Context(), cmd)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(out, nil)
}

func (s *Proxy) handleExecuteFeatureSearch(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(proxyevents.ExecuteFeatureSearchCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid feature search command"))
		return
	}
	s.mu.RLock()
	executor := s.featureExecutor
	s.mu.RUnlock()
	if executor == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "feature executor is unavailable"))
		return
	}
	out, err := executor.ExecuteFeatureSearch(ev.Context(), cmd)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	if s.history == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "search history is unavailable"))
		return
	}
	sources := make([]searchhistory.Source, 0, len(out.Sources))
	for _, source := range out.Sources {
		sources = append(sources, searchhistory.Source{Title: source.Title, URL: source.URL, Snippet: source.Snippet})
	}
	persisted, historyErr := s.history.Record(searchhistory.Record{
		OwnerID: cmd.OwnerID, Model: cmd.Model, ActualModel: out.ActualModel, Query: cmd.Query,
		OutputText: out.Text, Provider: out.Provider, Sources: sources,
	})
	if historyErr != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "persist search history: "+historyErr.Error()))
		return
	}
	out.HistoryID = persisted.ID
	result.Set(out, nil)
}

func (s *Proxy) handleListFeatureSearchHistory(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(proxyevents.ListFeatureSearchHistoryCommand)
	if !ok || cmd.OwnerID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid feature search history list command"))
		return
	}
	if s.history == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "search history is unavailable"))
		return
	}
	items, err := s.history.List(cmd.OwnerID, cmd.Limit)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	out := proxyevents.ListFeatureSearchHistoryResult{Items: make([]proxyevents.FeatureSearchHistoryItem, 0, len(items))}
	for _, item := range items {
		out.Items = append(out.Items, proxyevents.FeatureSearchHistoryItem{
			ID: item.ID, Model: item.Model, ActualModel: item.ActualModel, Query: item.Query, Provider: item.Provider,
			CreatedAt: item.CreatedAt, ExpiresAt: item.ExpiresAt,
		})
	}
	result.Set(out, nil)
}

func (s *Proxy) handleGetFeatureSearchHistory(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(proxyevents.GetFeatureSearchHistoryCommand)
	if !ok || cmd.OwnerID == "" || cmd.ID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid feature search history get command"))
		return
	}
	if s.history == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "search history is unavailable"))
		return
	}
	detail, err := s.history.Get(cmd.OwnerID, cmd.ID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.NotFound, err.Error()))
		return
	}
	out := proxyevents.GetFeatureSearchHistoryResult{FeatureSearchHistoryItem: proxyevents.FeatureSearchHistoryItem{
		ID: detail.ID, Model: detail.Model, ActualModel: detail.ActualModel, Query: detail.Query, Provider: detail.Provider,
		CreatedAt: detail.CreatedAt, ExpiresAt: detail.ExpiresAt,
	}, Text: detail.OutputText, Sources: make([]proxyevents.FeatureSearchSource, 0, len(detail.Sources))}
	for _, source := range detail.Sources {
		out.Sources = append(out.Sources, proxyevents.FeatureSearchSource{Title: source.Title, URL: source.URL, Snippet: source.Snippet})
	}
	result.Set(out, nil)
}

func (s *Proxy) handleExecuteFeatureImage(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(proxyevents.ExecuteFeatureImageCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid feature image command"))
		return
	}
	s.mu.RLock()
	executor := s.featureExecutor
	s.mu.RUnlock()
	if executor == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "feature executor is unavailable"))
		return
	}
	out, err := executor.ExecuteFeatureImage(ev.Context(), cmd)
	if err != nil {
		result.Set(out, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(out, nil)
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
	s.refreshEffectiveCatalog(ev.Context())
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
