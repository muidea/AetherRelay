package events

import (
	"context"
	"fmt"
	"strings"

	"ai-proxy/internal/modules/application/proxyapi/pkg/common"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"ai-proxy/internal/pkg/aiproxyconfig"

	"github.com/muidea/magicCommon/event"
)

const (
	TopicUpdateConfig           = "aiproxy.proxy.command.update"
	TopicEffectiveCatalog       = "aiproxy.proxy.query.effective_catalog"
	TopicStartCodexDiscovery    = "aiproxy.proxy.command.start_codex_discovery"
	TopicCodexDiscoveryProgress = "aiproxy.proxy.query.codex_discovery_progress"
)

type UpdateConfigCommand struct{ Config config.Config }

// EffectiveCatalogCommand requests the proxy-owned, request-time model
// directory. Consumers must not rebuild this projection from account-pool
// state because only proxyapi applies static-model conflict precedence.
type EffectiveCatalogCommand struct{}

type EffectiveCatalogResult struct{ Snapshot effectivecatalog.Snapshot }

// StartCodexDiscoveryCommand asks the proxy orchestrator to refresh the
// account-scoped Codex model snapshot. An empty AccountIDs list refreshes all
// eligible Codex OAuth accounts; non-empty lists are limited to those local
// account IDs. Credentials never leave the proxy/account EventHub path.
type StartCodexDiscoveryCommand struct{ AccountIDs []string }

// CodexDiscoveryProgress is an in-memory, bounded job projection. It reports
// discovery work rather than claiming that the listed models are an OpenAI
// global model directory.
type CodexDiscoveryProgress struct {
	ProgressID  string `json:"progress_id"`
	Total       int    `json:"total"`
	Processed   int    `json:"processed"`
	Succeeded   int    `json:"succeeded"`
	Failed      int    `json:"failed"`
	Done        bool   `json:"done"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

type StartCodexDiscoveryResult struct{ Progress CodexDiscoveryProgress }
type CodexDiscoveryProgressCommand struct{ ProgressID string }
type CodexDiscoveryProgressResult struct{ Progress CodexDiscoveryProgress }

func (s CodexDiscoveryProgress) Valid() bool { return strings.TrimSpace(s.ProgressID) != "" }

func UpdateConfig(ctx context.Context, hub event.Hub, source string, cfg config.Config) error {
	if hub == nil {
		return fmt.Errorf("proxy update command event hub is unavailable")
	}
	ev := event.NewEventWithContext(TopicUpdateConfig, source, common.UnitID, event.NewHeader(), ctx, UpdateConfigCommand{Config: cfg})
	result := hub.Send(ev)
	if result == nil {
		return fmt.Errorf("proxy update command received no response")
	}
	if err := result.Error(); err != nil {
		return fmt.Errorf("proxy update command failed: %s", err.Message)
	}
	return nil
}
