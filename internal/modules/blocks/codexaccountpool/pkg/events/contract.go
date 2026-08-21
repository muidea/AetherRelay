// Package events defines the Codex OAuth account-pool owner's typed EventHub contract.
package events

const (
	TopicList                      = "aetherrelay.codex.accountpool.command.list"
	TopicImport                    = "aetherrelay.codex.accountpool.command.import"
	TopicDelete                    = "aetherrelay.codex.accountpool.command.delete"
	TopicUpdate                    = "aetherrelay.codex.accountpool.command.update"
	TopicAcquire                   = "aetherrelay.codex.accountpool.command.acquire"
	TopicRelease                   = "aetherrelay.codex.accountpool.command.release"
	TopicRecordResult              = "aetherrelay.codex.accountpool.command.record_result"
	TopicRecordTransportCapability = "aetherrelay.codex.accountpool.command.record_transport_capability"
	TopicRefreshToken              = "aetherrelay.codex.accountpool.command.refresh_token"
	TopicRefreshByID               = "aetherrelay.codex.accountpool.command.refresh_by_id"
	TopicExportByID                = "aetherrelay.codex.accountpool.command.export_by_id"
	TopicHealth                    = "aetherrelay.codex.accountpool.command.health"
	TopicOAuthStart                = "aetherrelay.codex.accountpool.command.oauth_start"
	TopicOAuthFinish               = "aetherrelay.codex.accountpool.command.oauth_finish"
	// Discovery contracts keep the constrained, account-scoped model cache in
	// the account-pool owner. Tokens only cross the EventHub for the discovery
	// request and are never exposed through the Admin HTTP API.
	TopicListDiscoveryCandidates     = "aetherrelay.codex.accountpool.command.list_discovery_candidates"
	TopicPutModelSnapshot            = "aetherrelay.codex.accountpool.command.put_model_snapshot"
	TopicRecordModelDiscoveryFailure = "aetherrelay.codex.accountpool.command.record_model_discovery_failure"
	TopicCatalogSnapshot             = "aetherrelay.codex.accountpool.command.catalog_snapshot"
	// Usage contracts preserve a redacted, bounded account-level projection of
	// Codex's upstream usage windows. Credentials are available only to the
	// proxyapi orchestration path and are never included in AccountView.
	TopicListUsageCandidates = "aetherrelay.codex.accountpool.command.list_usage_candidates"
	TopicPutUsageSnapshot    = "aetherrelay.codex.accountpool.command.put_usage_snapshot"
	TopicMergeUsageSnapshot  = "aetherrelay.codex.accountpool.command.merge_usage_snapshot"
	TopicRecordUsageFailure  = "aetherrelay.codex.accountpool.command.record_usage_failure"
)

const (
	StatusNormal   = "normal"
	StatusAbnormal = "abnormal"
	StatusDisabled = "disabled"
)

const (
	// Fingerprint convergence is deliberately opt-in. Missing and invalid
	// persisted values are normalized to off by the account owner.
	FingerprintModeOff     = "off"
	FingerprintModeDevice  = "device"
	FingerprintModeSession = "session"
	FingerprintModeFull    = "full"
)

const (
	ErrorInvalidToken   = "invalid_token"
	ErrorRateLimit      = "rate_limit"
	ErrorTimeout        = "timeout"
	ErrorNetwork        = "network"
	ErrorUpstream       = "upstream"
	ErrorProtocol       = "protocol"
	ErrorInvalidRequest = "invalid_request"
	ErrorClient         = "client"
)

// AccountView is the management projection. Email is displayed as provided;
// access, refresh, and ID tokens, upstream account IDs, and proxy URLs are
// never included.
type AccountView struct {
	ID                         string                `json:"id"`
	IdentityKey                string                `json:"identity_key,omitempty"`
	Email                      string                `json:"email,omitempty"`
	PlanType                   string                `json:"plan_type,omitempty"`
	Status                     string                `json:"status"`
	Success                    int                   `json:"success"`
	Fail                       int                   `json:"fail"`
	CreatedAt                  string                `json:"created_at,omitempty"`
	LastUsedAt                 string                `json:"last_used_at,omitempty"`
	LastTokenRefreshAt         string                `json:"last_token_refresh_at,omitempty"`
	LastTokenRefreshErrorAt    string                `json:"last_token_refresh_error_at,omitempty"`
	LastTokenRefreshErrorClass string                `json:"last_token_refresh_error_class,omitempty"`
	Cooldowns                  []CooldownView        `json:"cooldowns,omitempty"`
	QuotaObservations          []QuotaObservation    `json:"quota_observations,omitempty"`
	ModelSnapshot              *AccountModelSnapshot `json:"model_snapshot,omitempty"`
	ModelDiscoveryRetryAt      string                `json:"model_discovery_retry_at,omitempty"`
	// Model discovery health uses a stable category rather than raw upstream
	// diagnostics, which can contain proxy or transport details.
	ModelDiscoveryErrorClass string                `json:"model_discovery_error_class,omitempty"`
	UsageSnapshot            *AccountUsageSnapshot `json:"usage_snapshot,omitempty"`
	UsageRefreshErrorAt      string                `json:"usage_refresh_error_at,omitempty"`
	UsageRefreshError        string                `json:"usage_refresh_error,omitempty"`
	CompactSupported         *bool                 `json:"compact_supported,omitempty"`
	WebsocketSupported       *bool                 `json:"websocket_supported,omitempty"`
	FingerprintMode          string                `json:"fingerprint_mode"`
}

type CooldownView struct {
	Model      string `json:"model"`
	Until      string `json:"until"`
	ErrorClass string `json:"error_class"`
}

// QuotaObservation is an upstream-observed account/model limit state. It is
// intentionally not a claimed remaining quota: Codex does not provide that
// value through the account model endpoint.
type QuotaObservation struct {
	Model      string `json:"model"`
	State      string `json:"state"`
	ObservedAt string `json:"observed_at"`
	ResetAt    string `json:"reset_at,omitempty"`
}

// CredentialInput is the deliberate secret-bearing Admin import contract.
// It never appears in a list or health response.
type CredentialInput struct {
	CredentialType  string `json:"credential_type,omitempty"`
	AccessToken     string `json:"access_token"`
	RefreshToken    string `json:"refresh_token"`
	IDToken         string `json:"id_token,omitempty"`
	AccountID       string `json:"account_id,omitempty"`
	Email           string `json:"email,omitempty"`
	Expired         string `json:"expired,omitempty"`
	Proxy           string `json:"proxy,omitempty"`
	FingerprintMode string `json:"fingerprint_mode,omitempty"`
	// TargetID is an internal import selector. It is populated only by the
	// Admin account-bundle orchestration and is never serialized or exported.
	TargetID string `json:"-"`
	// Reauthenticate marks an OAuth credential rotation. It lets the owner
	// preserve stable local metadata and clear failures from the retired token.
	Reauthenticate bool `json:"-"`
}

type ListCommand struct{}
type ListResult struct {
	Items []AccountView `json:"items"`
}

type ImportCommand struct{ Accounts []CredentialInput }
type ImportResult struct {
	Added      int      `json:"added"`
	Updated    int      `json:"updated"`
	Skipped    int      `json:"skipped"`
	AccountIDs []string `json:"account_ids,omitempty"`
}

type DeleteCommand struct{ IDs []string }
type DeleteResult struct {
	Deleted int `json:"deleted"`
}

type UpdateCommand struct {
	ID              string
	Status          *string
	Proxy           *string
	FingerprintMode *string
}
type UpdateResult struct {
	Item AccountView `json:"item"`
}

// AcquireResult contains only request-time credentials. It is restricted to
// the EventHub path and must not be returned from any HTTP adapter.
type AcquireCommand struct {
	Model       string
	Transport   string
	Exclude     []string
	SessionHash string
	PreferredID string
}
type AcquireResult struct {
	AccountID       string
	AccessToken     string
	AccountIDHeader string
	Proxy           string
	LeaseID         string
	FingerprintMode string
}

type ReleaseCommand struct{ LeaseID string }
type ReleaseResult struct{ Released bool }

type RecordResultCommand struct {
	AccountID         string
	Model             string
	Success           bool
	ErrorClass        string
	RetryAfterSeconds int
	QuotaExhausted    bool
	QuotaResetAt      string
	// AvailabilityNeutral records an observed request failure without changing
	// credential state, quota state, or the model cooldown used by normal
	// Responses routing. It is used for compact-only endpoint failures.
	AvailabilityNeutral bool
}
type RecordResultResult struct{ Account AccountView }

const (
	TransportResponses = "responses"
	TransportCompact   = "compact"
	TransportWebsocket = "websocket"
)

type RecordTransportCapabilityCommand struct {
	AccountID string
	Transport string
	Supported bool
}
type RecordTransportCapabilityResult struct{ Account AccountView }

type RefreshTokenCommand struct{ AccountID string }
type RefreshTokenResult struct {
	AccountID        string
	AccessToken      string
	AccountIDHeader  string
	Proxy            string
	FingerprintMode  string
	Refreshed        bool
	PermanentFailure bool
	ErrorClass       string
}

type RefreshByIDCommand struct{ IDs []string }
type RefreshByIDResult struct {
	Refreshed int           `json:"refreshed"`
	Failed    int           `json:"failed"`
	Items     []AccountView `json:"items"`
}

// ExportByID is the only deliberate secret-bearing management projection.
// It is selected by stable local IDs and must never be used by list APIs.
type ExportByIDCommand struct{ IDs []string }
type ExportByIDResult struct {
	Items []CredentialInput `json:"items"`
}

// AccountModelEntry is one authoritative model ID returned by the Codex
// account's /backend-api/codex/models endpoint. Codex OAuth is exposed only
// through Responses, so its model capability is fixed by the proxy owner
// instead of trusting arbitrary upstream capability fields.
type AccountModelEntry struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at,omitempty"`
	OwnedBy   string `json:"owned_by,omitempty"`
}

// AccountModelSnapshot is the persisted, bounded discovery cache for one
// account. It deliberately excludes raw upstream payloads and credentials.
type AccountModelSnapshot struct {
	AccountID    string              `json:"account_id"`
	Models       []AccountModelEntry `json:"models"`
	DiscoveredAt string              `json:"discovered_at"`
	ExpiresAt    string              `json:"expires_at,omitempty"`
}

// UsageWindow is a bounded projection of one upstream Codex usage window.
// It conveys relative utilization, not token or request counts. ID and Label
// are presentation metadata; no raw upstream payload crosses this contract.
type UsageWindow struct {
	ID               string  `json:"id"`
	Label            string  `json:"label,omitempty"`
	UsedPercent      float64 `json:"used_percent,omitempty"`
	UsedPercentKnown bool    `json:"used_percent_known"`
	WindowSeconds    int     `json:"window_seconds,omitempty"`
	ResetAt          string  `json:"reset_at,omitempty"`
	Allowed          bool    `json:"allowed"`
	AllowedKnown     bool    `json:"allowed_known"`
	LimitReached     bool    `json:"limit_reached"`
}

// AccountUsageSnapshot is the durable, redacted usage observation for a
// single account. ExpiresAt only marks staleness for presentation; a failed
// refresh keeps the prior snapshot so operators retain the last observation.
type AccountUsageSnapshot struct {
	PlanType   string        `json:"plan_type,omitempty"`
	Windows    []UsageWindow `json:"windows,omitempty"`
	ObservedAt string        `json:"observed_at"`
	ExpiresAt  string        `json:"expires_at,omitempty"`
}

type DiscoveryCandidate struct {
	AccountID       string
	AccessToken     string
	AccountIDHeader string
	Proxy           string
	NeedsDiscovery  bool
	DiscoveryDue    bool
	// DiscoveryBackedOff remains true during the persisted retry window even
	// when a prior model snapshot has not expired yet.
	DiscoveryBackedOff bool
}

type ListDiscoveryCandidatesCommand struct{ AccountIDs []string }
type ListDiscoveryCandidatesResult struct {
	Candidates []DiscoveryCandidate
	Version    uint64
}

type PutModelSnapshotCommand struct {
	AccountID string
	Snapshot  AccountModelSnapshot
}
type PutModelSnapshotResult struct {
	Version uint64
	OK      bool
}

type RecordModelDiscoveryFailureCommand struct {
	AccountID  string
	ErrorClass string
}
type RecordModelDiscoveryFailureResult struct {
	RetryAt string
	OK      bool
}

// UsageCandidate is credential-bearing only on the typed EventHub path.
// The Admin HTTP API can never receive it.
type UsageCandidate struct {
	AccountID       string
	AccessToken     string
	AccountIDHeader string
	Proxy           string
}

type ListUsageCandidatesCommand struct{ AccountIDs []string }
type ListUsageCandidatesResult struct {
	Candidates []UsageCandidate
}

type PutUsageSnapshotCommand struct {
	AccountID string
	Snapshot  AccountUsageSnapshot
}
type PutUsageSnapshotResult struct{ OK bool }

type MergeUsageSnapshotCommand struct {
	AccountID string
	Snapshot  AccountUsageSnapshot
}
type MergeUsageSnapshotResult struct{ OK bool }

type RecordUsageFailureCommand struct {
	AccountID string
	Error     string
}
type RecordUsageFailureResult struct{ OK bool }

// CatalogModel is the deduplicated model union for healthy accounts. Account
// IDs stay visible only to the proxy/account owners and are not an Admin API
// response shape.
type CatalogModel struct {
	ID         string   `json:"id"`
	CreatedAt  int64    `json:"created_at,omitempty"`
	OwnedBy    string   `json:"owned_by,omitempty"`
	AccountIDs []string `json:"account_ids,omitempty"`
}

type CatalogSnapshotCommand struct{}
type CatalogSnapshotResult struct {
	Version           uint64
	Models            []CatalogModel
	AvailableAccounts int
	UpdatedAt         string
}

type HealthCommand struct{}
type HealthResult struct {
	Total     int `json:"total"`
	Available int `json:"available"`
	Abnormal  int `json:"abnormal"`
	Disabled  int `json:"disabled"`
}

type OAuthStartCommand struct {
	EmailHint string
	Proxy     string
	TargetID  string
}
type OAuthStartResult struct {
	SessionID         string `json:"session_id"`
	AuthorizeURL      string `json:"authorize_url"`
	ExpiresIn         int    `json:"expires_in"`
	RedirectURIPrefix string `json:"redirect_uri_prefix"`
}
type OAuthFinishCommand struct {
	SessionID string
	Callback  string
}
type OAuthFinishResult struct {
	Added bool        `json:"added"`
	Item  AccountView `json:"item"`
}
