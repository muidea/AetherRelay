package biz

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	basebiz "ai-proxy/internal/modules/base/biz"
	"ai-proxy/internal/modules/blocks/configruntime/internal/providerstore"
	"ai-proxy/internal/modules/blocks/configruntime/pkg/common"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	"ai-proxy/internal/pkg/aiproxybootstrap"
	config "ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxycredential"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type ConfigRuntime struct {
	basebiz.Base

	mu               sync.RWMutex
	bootstrap        configevents.Bootstrap
	providers        *providerstore.Store
	managedProviders bool
}

func New(hub event.Hub, background task.BackgroundRoutine) (*ConfigRuntime, *cd.Error) {
	bootstrap, ok := aiproxybootstrap.Current()
	if !ok {
		return nil, cd.NewError(cd.IllegalParam, "ai-proxy bootstrap is not configured")
	}
	if hub == nil {
		return nil, cd.NewError(cd.IllegalParam, "event hub is unavailable")
	}

	biz := &ConfigRuntime{
		Base:      basebiz.New(common.UnitID, hub, background),
		bootstrap: configevents.Bootstrap{Config: bootstrap.Config, ConfigPath: bootstrap.ConfigPath, Version: bootstrap.Version, StartedAt: bootstrap.StartedAt},
	}
	if strings.TrimSpace(os.Getenv(aiproxycredential.EnvironmentKey)) != "" {
		codec, codecErr := aiproxycredential.FromEnvironment()
		if codecErr != nil {
			return nil, cd.NewError(cd.IllegalParam, codecErr.Error())
		}
		store, storeErr := providerstore.Open(bootstrap.Config.State.Database, bootstrap.Config.State.MemoryLimit, bootstrap.Config.State.Threads, codec)
		if storeErr != nil {
			return nil, cd.NewError(cd.Unexpected, "open provider catalog: "+storeErr.Error())
		}
		providers, initialized, loadErr := store.Load()
		if loadErr != nil {
			_ = store.Close()
			return nil, cd.NewError(cd.Unexpected, "load provider catalog: "+loadErr.Error())
		}
		if initialized {
			loaded, replaceErr := config.ReplaceProviders(biz.bootstrap.Config, providers)
			if replaceErr != nil {
				_ = store.Close()
				return nil, cd.NewError(cd.IllegalParam, "stored provider catalog: "+replaceErr.Error())
			}
			biz.bootstrap.Config = loaded
			biz.managedProviders = true
		}
		biz.providers = store
		biz.bootstrap.ProviderStorageAvailable = true
	} else {
		initialized, storeErr := providerstore.Initialized(bootstrap.Config.State.Database, bootstrap.Config.State.MemoryLimit, bootstrap.Config.State.Threads)
		if storeErr != nil {
			return nil, cd.NewError(cd.Unexpected, "inspect provider catalog: "+storeErr.Error())
		}
		if initialized {
			return nil, cd.NewError(cd.IllegalParam, aiproxycredential.EnvironmentKey+" is required to load the stored provider catalog")
		}
	}
	biz.SubscribeFunc(configevents.TopicBootstrap, biz.handleBootstrap)
	biz.SubscribeFunc(configevents.TopicActivate, biz.handleActivate)
	biz.SubscribeFunc(configevents.TopicReplaceProviders, biz.handleReplaceProviders)
	return biz, nil
}

func (s *ConfigRuntime) Run(context.Context) *cd.Error { return nil }

func (s *ConfigRuntime) Teardown(context.Context) {
	s.UnsubscribeFunc(configevents.TopicBootstrap)
	s.UnsubscribeFunc(configevents.TopicActivate)
	s.UnsubscribeFunc(configevents.TopicReplaceProviders)
	s.mu.Lock()
	s.bootstrap = configevents.Bootstrap{}
	s.managedProviders = false
	if s.providers != nil {
		_ = s.providers.Close()
		s.providers = nil
	}
	s.mu.Unlock()
}

func (s *ConfigRuntime) handleReplaceProviders(ev event.Event, result event.Result) {
	command, ok := ev.Data().(configevents.ReplaceProvidersCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid replace providers command"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.providers == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, aiproxycredential.EnvironmentKey+" is required for managed Provider storage"))
		return
	}
	current := s.bootstrap.Config
	next, err := config.ReplaceProviders(current, command.Providers)
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	if err := s.providers.Replace(next.Providers); err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "persist provider catalog: "+err.Error()))
		return
	}
	if err := proxyevents.UpdateConfig(ev.Context(), s.EventHub(), s.ID(), next); err != nil {
		rollbackErr := s.providers.Replace(current.Providers)
		message := "activate proxy config: " + err.Error()
		if rollbackErr != nil {
			message += "; rollback provider catalog: " + rollbackErr.Error()
		}
		result.Set(nil, cd.NewError(cd.Unexpected, message))
		return
	}
	s.bootstrap.Config = next
	s.managedProviders = true
	result.Set(struct{}{}, nil)
}

func (s *ConfigRuntime) handleBootstrap(ev event.Event, result event.Result) {
	s.mu.RLock()
	bootstrap := s.bootstrap
	s.mu.RUnlock()
	switch ev.Data().(type) {
	case configevents.BootstrapCommand:
		result.Set(configevents.BootstrapResult{Bootstrap: bootstrap}, nil)
	default:
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid bootstrap command"))
	}
}

func (s *ConfigRuntime) handleActivate(ev event.Event, result event.Result) {
	command, ok := ev.Data().(configevents.ActivateCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid config activate command"))
		return
	}
	s.mu.RLock()
	current := s.bootstrap.Config
	managedProviders := s.managedProviders
	s.mu.RUnlock()
	if managedProviders {
		var err error
		command.Config, err = config.ReplaceProviders(command.Config, current.Providers)
		if err != nil {
			result.Set(nil, cd.NewError(cd.IllegalParam, "preserve managed providers: "+err.Error()))
			return
		}
	}
	if err := validateHotReload(current, command.Config); err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	if err := proxyevents.UpdateConfig(ev.Context(), s.EventHub(), s.ID(), command.Config); err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "activate proxy config: "+err.Error()))
		return
	}
	s.mu.Lock()
	s.bootstrap.Config = command.Config
	s.mu.Unlock()
	result.Set(struct{}{}, nil)
}

// validateHotReload keeps configuration truthful. These capabilities own
// stores, EventHub subscriptions, timers, and upstream clients assembled at
// process start; changing their lifecycle settings in a YAML rewrite cannot
// make those components appear, disappear, or reschedule safely.
func validateHotReload(current, next config.Config) error {
	if current.ChatGPTWeb.RefreshAccountIntervalMinute != next.ChatGPTWeb.RefreshAccountIntervalMinute ||
		current.ChatGPTWeb.TemporaryChat != next.ChatGPTWeb.TemporaryChat {
		return fmt.Errorf("chatgpt_web runtime settings require an ai-proxy restart")
	}
	if current.CodexOAuth.RefreshAccountIntervalMinute != next.CodexOAuth.RefreshAccountIntervalMinute {
		return fmt.Errorf("codex_oauth.refresh_account_interval_minute requires an ai-proxy restart")
	}
	return nil
}
