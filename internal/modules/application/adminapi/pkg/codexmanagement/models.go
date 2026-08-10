// Package codexmanagement contains Admin-facing Codex account management
// projections. It is a pure DTO package: account state remains owned by
// codexaccountpool, while discovery orchestration remains owned by proxyapi.
package codexmanagement

import (
	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	codexevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

type ImportResult struct {
	Added               int                                 `json:"added"`
	Updated             int                                 `json:"updated"`
	Skipped             int                                 `json:"skipped"`
	AccountIDs          []string                            `json:"account_ids,omitempty"`
	ModelDiscovery      *proxyevents.CodexDiscoveryProgress `json:"model_discovery,omitempty"`
	ModelDiscoveryError string                              `json:"model_discovery_error,omitempty"`
	UsageRefresh        *proxyevents.CodexUsageProgress     `json:"usage_refresh,omitempty"`
	UsageRefreshError   string                              `json:"usage_refresh_error,omitempty"`
}

type RefreshResult struct {
	Refreshed           int                                 `json:"refreshed"`
	Failed              int                                 `json:"failed"`
	Items               []codexevents.AccountView           `json:"items"`
	ModelDiscovery      *proxyevents.CodexDiscoveryProgress `json:"model_discovery,omitempty"`
	ModelDiscoveryError string                              `json:"model_discovery_error,omitempty"`
	UsageRefresh        *proxyevents.CodexUsageProgress     `json:"usage_refresh,omitempty"`
	UsageRefreshError   string                              `json:"usage_refresh_error,omitempty"`
}

type OAuthFinishResult struct {
	Added               bool                                `json:"added"`
	Item                codexevents.AccountView             `json:"item"`
	ModelDiscovery      *proxyevents.CodexDiscoveryProgress `json:"model_discovery,omitempty"`
	ModelDiscoveryError string                              `json:"model_discovery_error,omitempty"`
	UsageRefresh        *proxyevents.CodexUsageProgress     `json:"usage_refresh,omitempty"`
	UsageRefreshError   string                              `json:"usage_refresh_error,omitempty"`
}
