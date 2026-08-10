package events

import (
	"context"
	"fmt"
	"time"

	"aetherrelay/internal/modules/blocks/configruntime/pkg/common"
	"aetherrelay/internal/pkg/aetherrelayconfig"

	"github.com/muidea/magicCommon/event"
)

const (
	TopicBootstrap        = "aetherrelay.config.command.bootstrap"
	TopicActivate         = "aetherrelay.config.command.activate"
	TopicReplaceProviders = "aetherrelay.config.command.replace-providers"
)

type Bootstrap struct {
	Config                   config.Config
	ConfigPath               string
	Version                  string
	StartedAt                time.Time
	ProviderStorageAvailable bool
}

type BootstrapCommand struct{}
type BootstrapResult struct{ Bootstrap Bootstrap }
type ActivateCommand struct{ Config config.Config }
type ReplaceProvidersCommand struct{ Providers map[string]config.Provider }

func RequestBootstrap(ctx context.Context, hub event.Hub, source string) (Bootstrap, error) {
	ev := event.NewEventWithContext(TopicBootstrap, source, common.UnitID, event.NewHeader(), ctx, BootstrapCommand{})
	result, err := send(hub, ev, "bootstrap")
	if err != nil {
		return Bootstrap{}, err
	}
	value, getErr := result.Get()
	if getErr != nil {
		return Bootstrap{}, fmt.Errorf("bootstrap command failed: %s", getErr.Message)
	}
	response, ok := value.(BootstrapResult)
	if !ok {
		return Bootstrap{}, fmt.Errorf("invalid bootstrap response")
	}
	return response.Bootstrap, nil
}

func Activate(ctx context.Context, hub event.Hub, source string, cfg config.Config) error {
	ev := event.NewEventWithContext(TopicActivate, source, common.UnitID, event.NewHeader(), ctx, ActivateCommand{Config: cfg})
	_, err := send(hub, ev, "config activate")
	return err
}

func ReplaceProviders(ctx context.Context, hub event.Hub, source string, providers map[string]config.Provider) error {
	ev := event.NewEventWithContext(TopicReplaceProviders, source, common.UnitID, event.NewHeader(), ctx, ReplaceProvidersCommand{Providers: providers})
	_, err := send(hub, ev, "replace providers")
	return err
}

func send(hub event.Hub, ev event.Event, name string) (event.Result, error) {
	if hub == nil {
		return nil, fmt.Errorf("%s command event hub is unavailable", name)
	}
	result := hub.Send(ev)
	if result == nil {
		return nil, fmt.Errorf("%s command received no response", name)
	}
	if err := result.Error(); err != nil {
		return nil, fmt.Errorf("%s command failed: %s", name, err.Message)
	}
	return result, nil
}
