package biz

import (
	"context"
	"fmt"
	"sync"

	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	basebiz "ai-proxy/internal/modules/base/biz"
	"ai-proxy/internal/modules/blocks/configruntime/pkg/common"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	"ai-proxy/internal/pkg/aiproxybootstrap"
	config "ai-proxy/internal/pkg/aiproxyconfig"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type ConfigRuntime struct {
	basebiz.Base

	mu        sync.RWMutex
	bootstrap configevents.Bootstrap
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
		bootstrap: configevents.Bootstrap{Config: bootstrap.Config, ConfigPath: bootstrap.ConfigPath},
	}
	biz.SubscribeFunc(configevents.TopicBootstrap, biz.handleBootstrap)
	biz.SubscribeFunc(configevents.TopicActivate, biz.handleActivate)
	return biz, nil
}

func (s *ConfigRuntime) Run(context.Context) *cd.Error { return nil }

func (s *ConfigRuntime) Teardown(context.Context) {
	s.UnsubscribeFunc(configevents.TopicBootstrap)
	s.UnsubscribeFunc(configevents.TopicActivate)
	s.mu.Lock()
	s.bootstrap = configevents.Bootstrap{}
	s.mu.Unlock()
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
	s.mu.RUnlock()
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
	if current.ChatGPTWeb != next.ChatGPTWeb {
		return fmt.Errorf("chatgpt_web runtime settings require an ai-proxy restart")
	}
	if current.CodexOAuth.Enabled != next.CodexOAuth.Enabled || current.CodexOAuth.RefreshAccountIntervalMinute != next.CodexOAuth.RefreshAccountIntervalMinute {
		return fmt.Errorf("codex_oauth.enabled and codex_oauth.refresh_account_interval_minute require an ai-proxy restart")
	}
	return nil
}
