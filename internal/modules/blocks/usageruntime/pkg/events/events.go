package events

import (
	"time"

	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

const (
	TopicAcquire           = "aetherrelay.usage.command.acquire"
	TopicStart             = "aetherrelay.usage.command.start"
	TopicComplete          = "aetherrelay.usage.command.complete"
	TopicDashboard         = "aetherrelay.usage.command.dashboard"
	TopicCount             = "aetherrelay.usage.command.count"
	TopicEvents            = "aetherrelay.usage.command.events"
	TopicExport            = "aetherrelay.usage.command.export"
	TopicFilterOptions     = "aetherrelay.usage.command.filter-options"
	TopicRecover           = "aetherrelay.usage.command.recover"
	TopicCheckpoint        = "aetherrelay.usage.command.checkpoint"
	TopicHealthy           = "aetherrelay.usage.command.healthy"
	TopicAllTime           = "aetherrelay.usage.command.all-time"
	TopicClientKeyEnsure   = "aetherrelay.usage.command.client-key-ensure"
	TopicClientKeyTouch    = "aetherrelay.usage.command.client-key-touch"
	TopicClientKeyMetadata = "aetherrelay.usage.command.client-key-metadata"
	TopicClientKeyList     = "aetherrelay.usage.command.client-key-list"
	TopicClientKeyCreate   = "aetherrelay.usage.command.client-key-create"
	TopicClientKeyEnable   = "aetherrelay.usage.command.client-key-enable"
	TopicClientKeyRotate   = "aetherrelay.usage.command.client-key-rotate"
	TopicClientKeyRevoke   = "aetherrelay.usage.command.client-key-revoke"
	TopicClientKeyDelete   = "aetherrelay.usage.command.client-key-delete"
	TopicClientKeyAccess   = "aetherrelay.usage.command.client-key-access"
	TopicClientKeyRefs     = "aetherrelay.usage.command.client-key-provider-refs"
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
