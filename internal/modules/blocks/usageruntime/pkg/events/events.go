package events

import (
	"time"

	"ai-proxy/internal/pkg/aiproxyclientaccess"
	"ai-proxy/internal/pkg/aiproxyusage"
)

const (
	TopicAcquire           = "aiproxy.usage.command.acquire"
	TopicStart             = "aiproxy.usage.command.start"
	TopicComplete          = "aiproxy.usage.command.complete"
	TopicDashboard         = "aiproxy.usage.command.dashboard"
	TopicCount             = "aiproxy.usage.command.count"
	TopicEvents            = "aiproxy.usage.command.events"
	TopicExport            = "aiproxy.usage.command.export"
	TopicFilterOptions     = "aiproxy.usage.command.filter-options"
	TopicRecover           = "aiproxy.usage.command.recover"
	TopicCheckpoint        = "aiproxy.usage.command.checkpoint"
	TopicHealthy           = "aiproxy.usage.command.healthy"
	TopicAllTime           = "aiproxy.usage.command.all-time"
	TopicClientKeyEnsure   = "aiproxy.usage.command.client-key-ensure"
	TopicClientKeyTouch    = "aiproxy.usage.command.client-key-touch"
	TopicClientKeyMetadata = "aiproxy.usage.command.client-key-metadata"
	TopicClientKeyList     = "aiproxy.usage.command.client-key-list"
	TopicClientKeyCreate   = "aiproxy.usage.command.client-key-create"
	TopicClientKeyEnable   = "aiproxy.usage.command.client-key-enable"
	TopicClientKeyRotate   = "aiproxy.usage.command.client-key-rotate"
	TopicClientKeyRevoke   = "aiproxy.usage.command.client-key-revoke"
	TopicClientKeyDelete   = "aiproxy.usage.command.client-key-delete"
	TopicClientKeyAccess   = "aiproxy.usage.command.client-key-access"
	TopicClientKeyRefs     = "aiproxy.usage.command.client-key-provider-refs"
)

type AcquireCommand struct{}
type AcquireResult struct{}
type StartCommand struct{ Record usage.StartRecord }
type CompleteCommand struct{ Record usage.CompleteRecord }
type DashboardCommand struct{ Filter usage.UsageFilter }
type DashboardResult struct{ Value usage.Dashboard }
type CountCommand struct{ Filter usage.UsageFilter }
type CountResult struct{ Value int64 }
type EventsCommand struct{ Filter usage.EventFilter }
type EventsResult struct{ Value usage.EventPage }
type ExportCommand struct{ Filter usage.UsageFilter }
type ExportResult struct{ Data []byte }
type FilterOptionsCommand struct{ Query usage.FilterOptionsQuery }
type FilterOptionsResult struct{ Value usage.FilterOptionsResult }
type RecoverCommand struct{ At time.Time }
type RecoverResult struct{ Count int64 }
type CheckpointCommand struct{}
type HealthyCommand struct{}
type HealthyResult struct{ Value bool }
type AllTimeCommand struct{}
type AllTimeResult struct{ Value map[string]usage.Summary }
type ClientKeyEnsureCommand struct {
	ID        string
	CreatedAt time.Time
}
type ClientKeyTouchCommand struct {
	ID     string
	UsedAt time.Time
}
type ClientKeyMetadataCommand struct{}
type ClientKeyMetadataResult struct {
	Value map[string]usage.ClientAPIKeyMetadata
}
type ClientKeyListCommand struct{}
type ClientKeyListResult struct {
	Value map[string]usage.ClientAPIKeyRecord
}
type ClientKeyCreateCommand struct{ Value usage.ClientAPIKeyRecord }
type ClientKeyEnableCommand struct {
	ID      string
	Enabled bool
}
type ClientKeyRotateCommand struct {
	ID, Hash string
	At       time.Time
}
type ClientKeyRevokeCommand struct {
	ID string
	At time.Time
}
type ClientKeyDeleteCommand struct{ ID string }
type ClientKeyAccessCommand struct {
	ID     string
	Policy clientaccess.Policy
}
type ClientKeyProviderRefsCommand struct{ ProviderID string }
type ClientKeyProviderRefsResult struct{ IDs []string }
