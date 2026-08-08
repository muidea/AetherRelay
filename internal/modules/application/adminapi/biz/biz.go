package biz

import (
	"context"
	"fmt"
	"time"

	admincommon "ai-proxy/internal/modules/application/adminapi/pkg/common"
	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	basebiz "ai-proxy/internal/modules/base/biz"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	metricsevents "ai-proxy/internal/modules/blocks/metricsruntime/pkg/events"
	usageevents "ai-proxy/internal/modules/blocks/usageruntime/pkg/events"
	"ai-proxy/internal/pkg/aiproxyclientauth"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetricsport"
	"ai-proxy/internal/pkg/aiproxyusage"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

// Admin owns the EventHub-backed dependencies used by the admin application
// adapter. HTTP handlers depend on this narrow RuntimeConfig implementation,
// never on EventHub directly.
type Admin struct {
	basebiz.Base
	bootstrap configevents.Bootstrap
	usage     usage.Store
	metrics   metricsport.Port
}

func New(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) (*Admin, *cd.Error) {
	biz := &Admin{Base: basebiz.New(admincommon.UnitID, hub, background)}
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
	biz.bootstrap = bootstrap
	biz.usage = usageStore
	biz.metrics = metrics
	return biz, nil
}

func (s *Admin) Run(context.Context) *cd.Error { return nil }

func (s *Admin) Teardown(context.Context) {
	s.bootstrap = configevents.Bootstrap{}
	s.usage = nil
	s.metrics = nil
}

func (s *Admin) ConfigPath() string         { return s.bootstrap.ConfigPath }
func (s *Admin) Config() config.Config      { return s.bootstrap.Config }
func (s *Admin) UsageStore() usage.Store    { return s.usage }
func (s *Admin) Metrics() metricsport.Port  { return s.metrics }
func (s *Admin) SystemVersion() string      { return s.bootstrap.Version }
func (s *Admin) SystemStartedAt() time.Time { return s.bootstrap.StartedAt }

func (s *Admin) ConfigSnapshot() config.Config {
	bootstrap, err := configevents.RequestBootstrap(context.Background(), s.EventHub(), s.ID())
	if err != nil {
		return config.Config{}
	}
	return bootstrap.Config
}

func (s *Admin) UpdateConfig(cfg config.Config) error {
	return configevents.Activate(context.Background(), s.EventHub(), s.ID(), cfg)
}

func (s *Admin) ReplaceProviders(providers map[string]config.Provider) error {
	return configevents.ReplaceProviders(context.Background(), s.EventHub(), s.ID(), providers)
}

func (s *Admin) ProviderStorageAvailable() bool {
	bootstrap, err := configevents.RequestBootstrap(context.Background(), s.EventHub(), s.ID())
	return err == nil && bootstrap.ProviderStorageAvailable
}

func (s *Admin) PrepareClientKeyIndex(records map[string]usage.ClientAPIKeyRecord) (*clientauth.Index, error) {
	value, err := s.SendEvent(event.NewEventWithContext(proxyevents.TopicPrepareClientKeyIndex, s.ID(), proxycommon.UnitID, event.NewHeader(), context.Background(), proxyevents.PrepareClientKeyIndexCommand{Records: records})).Get()
	if err != nil {
		return nil, fmt.Errorf("prepare client key index: %s", err.Message)
	}
	response, ok := value.(proxyevents.PrepareClientKeyIndexResult)
	if !ok || response.Index == nil {
		return nil, fmt.Errorf("invalid client key index result")
	}
	return response.Index, nil
}

func (s *Admin) ActivateClientKeyIndex(index *clientauth.Index) {
	_, _ = s.SendEvent(event.NewEventWithContext(proxyevents.TopicActivateClientKeyIndex, s.ID(), proxycommon.UnitID, event.NewHeader(), context.Background(), proxyevents.ActivateClientKeyIndexCommand{Index: index})).Get()
}

func (s *Admin) EffectiveCatalogSnapshot() effectivecatalog.Snapshot {
	value, err := s.SendEvent(event.NewEventWithContext(proxyevents.TopicEffectiveCatalog, s.ID(), proxycommon.UnitID, event.NewHeader(), context.Background(), proxyevents.EffectiveCatalogCommand{})).Get()
	if err != nil {
		return effectivecatalog.Snapshot{}
	}
	response, ok := value.(proxyevents.EffectiveCatalogResult)
	if !ok {
		return effectivecatalog.Snapshot{}
	}
	return response.Snapshot
}
