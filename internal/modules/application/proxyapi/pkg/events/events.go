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
	TopicStartCodexUsageRefresh = "aiproxy.proxy.command.start_codex_usage_refresh"
	TopicCodexUsageProgress     = "aiproxy.proxy.query.codex_usage_progress"
	TopicFeatureCatalog         = "aiproxy.proxy.query.feature_catalog"
	TopicExecuteFeatureText     = "aiproxy.proxy.command.execute_feature_text"
	TopicExecuteFeatureImage    = "aiproxy.proxy.command.execute_feature_image"
)

type UpdateConfigCommand struct{ Config config.Config }

// EffectiveCatalogCommand requests the proxy-owned, request-time model
// directory. Consumers must not rebuild this projection from account-pool
// state because only proxyapi applies static-model conflict precedence.
type EffectiveCatalogCommand struct{}

type EffectiveCatalogResult struct{ Snapshot effectivecatalog.Snapshot }

// FeatureCatalog is the proxy-owned projection used by Admin feature pages.
// It is derived from the same request-time transport plans as /v1 endpoints,
// so a model is never offered unless at least one compatible Provider exists.
type FeatureCatalogCommand struct{}

type FeatureProvider struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Priority int    `json:"priority"`
}

type FeatureModel struct {
	ID        string            `json:"id"`
	Providers []FeatureProvider `json:"providers"`
}

type FeatureCatalogResult struct {
	TextModels      []FeatureModel `json:"text_models"`
	ImageModels     []FeatureModel `json:"image_models"`
	ImageEditModels []FeatureModel `json:"image_edit_models"`
}

type FeatureTextMessage struct {
	Role    string
	Content string
	Images  [][]byte
}

type ExecuteFeatureTextCommand struct {
	OwnerID        string
	Model          string
	Messages       []FeatureTextMessage
	ThinkingEffort string
}

type ExecuteFeatureTextResult struct {
	Provider    string
	ActualModel string
	Text        string
}

type ExecuteFeatureImageCommand struct {
	OwnerID string
	Model   string
	Prompt  string
	Size    string
	Quality string
	Images  [][]byte
}

type FeatureImageData struct {
	URL           string
	B64JSON       string
	RevisedPrompt string
}

type ExecuteFeatureImageResult struct {
	Provider string
	Model    string
	Data     []FeatureImageData
}

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

// StartCodexUsageRefreshCommand refreshes an account-scoped, upstream-observed
// usage projection. Empty AccountIDs means every normal Codex OAuth account.
// It is intentionally separate from model discovery because usage windows do
// not determine whether an account supports a model.
type StartCodexUsageRefreshCommand struct{ AccountIDs []string }

type CodexUsageProgress struct {
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

type StartCodexUsageRefreshResult struct{ Progress CodexUsageProgress }
type CodexUsageProgressCommand struct{ ProgressID string }
type CodexUsageProgressResult struct{ Progress CodexUsageProgress }

func (s CodexDiscoveryProgress) Valid() bool { return strings.TrimSpace(s.ProgressID) != "" }
func (s CodexUsageProgress) Valid() bool     { return strings.TrimSpace(s.ProgressID) != "" }

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
