package biz

import (
	configcommon "aetherrelay/internal/modules/blocks/configruntime/pkg/common"
	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"github.com/muidea/magicCommon/event"
	"path/filepath"
	"testing"
)

func wireBootstrap(hub event.Hub, t *testing.T) (dbPath string) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	stateDir := t.TempDir()
	cfgObs := event.NewSimpleObserver(configcommon.UnitID, hub)
	cfgObs.Subscribe(configevents.TopicBootstrap, func(_ event.Event, result event.Result) {
		result.Set(configevents.BootstrapResult{Bootstrap: configevents.Bootstrap{Config: config.Config{
			ChatGPTWeb: config.ChatGPTWebConfig{
				TemporaryChat: config.TemporaryChatConfig{
					Enabled: true, RetentionDays: 30, MaxConversations: 20,
					MaxMessagesPerConversation: 50, MaxMessageBytes: 8192, TurnTimeoutSeconds: 30,
				},
			},
			State: config.StateConfig{Dir: stateDir, Database: dbPath, MemoryLimit: "64MB", Threads: 1},
		}}}, nil)
	})
	return dbPath
}
