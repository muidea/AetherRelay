// Package events defines the ChatGPT account-pool owner's typed EventHub contract.
package events

const (
	TopicList   = "aiproxy.chatgpt.accountpool.command.list"
	TopicAdd    = "aiproxy.chatgpt.accountpool.command.add"
	TopicDelete = "aiproxy.chatgpt.accountpool.command.delete"
	TopicUpdate = "aiproxy.chatgpt.accountpool.command.update"
	// The ID-based commands are the public management contract. Access-token
	// selectors are retained only for owner-local compatibility flows.
	TopicDeleteByID          = "aiproxy.chatgpt.accountpool.command.delete_by_id"
	TopicUpdateByID          = "aiproxy.chatgpt.accountpool.command.update_by_id"
	TopicExportByID          = "aiproxy.chatgpt.accountpool.command.export_by_id"
	TopicRefreshByID         = "aiproxy.chatgpt.accountpool.command.refresh_by_id"
	TopicAcquireImageToken   = "aiproxy.chatgpt.accountpool.command.acquire_image_token"
	TopicAcquireImageAccount = "aiproxy.chatgpt.accountpool.command.acquire_image_account"
	TopicReleaseImageSlot    = "aiproxy.chatgpt.accountpool.command.release_image_slot"
	TopicMarkImageResult     = "aiproxy.chatgpt.accountpool.command.mark_image_result"
	TopicAcquireTextToken    = "aiproxy.chatgpt.accountpool.command.acquire_text_token"
	TopicAcquireTextAccount  = "aiproxy.chatgpt.accountpool.command.acquire_text_account"
	TopicRecordTextResult    = "aiproxy.chatgpt.accountpool.command.record_text_result"
	TopicRemoveInvalid       = "aiproxy.chatgpt.accountpool.command.remove_invalid"
	TopicHealth              = "aiproxy.chatgpt.accountpool.command.health"
	TopicOAuthStart          = "aiproxy.chatgpt.accountpool.command.oauth_start"
	TopicOAuthFinish         = "aiproxy.chatgpt.accountpool.command.oauth_finish"
	TopicExport              = "aiproxy.chatgpt.accountpool.command.export"
	TopicRefresh             = "aiproxy.chatgpt.accountpool.command.refresh"
	TopicRefreshProgress     = "aiproxy.chatgpt.accountpool.command.refresh_progress"
	// Discovery / capability snapshot contracts owned by the account pool.
	TopicListDiscoveryCandidates     = "aiproxy.chatgpt.accountpool.command.list_discovery_candidates"
	TopicPutModelSnapshot            = "aiproxy.chatgpt.accountpool.command.put_model_snapshot"
	TopicRecordModelDiscoveryFailure = "aiproxy.chatgpt.accountpool.command.record_model_discovery_failure"
	TopicCatalogSnapshot             = "aiproxy.chatgpt.accountpool.command.catalog_snapshot"
)

// Model operations mirrored from the upstream models enumeration contract.
const (
	ModelOperationChatCompletions  = "chat_completions"
	ModelOperationImageGenerations = "image_generations"
)

type AccountView struct {
	ID            string `json:"id"`
	Email         string `json:"email,omitempty"`
	Type          string `json:"type,omitempty"`
	SourceType    string `json:"source_type,omitempty"`
	Status        string `json:"status"`
	Quota         int    `json:"quota"`
	RestoreAt     string `json:"restore_at,omitempty"`
	ImageInflight int    `json:"image_inflight"`
	Success       int    `json:"success"`
	Fail          int    `json:"fail"`
	CreatedAt     string `json:"created_at,omitempty"`
	AccessToken   string `json:"access_token,omitempty"`
	Proxy         string `json:"proxy,omitempty"`
	LastUsedAt    string `json:"last_used_at,omitempty"`
}

type ListCommand struct{}
type ListResult struct {
	Items []AccountView `json:"items"`
}

type AddCommand struct {
	Tokens     []string
	SourceType string
}
type AddResult struct {
	Added   int `json:"added"`
	Skipped int `json:"skipped"`
}

type DeleteCommand struct{ Tokens []string }
type DeleteResult struct {
	Deleted int `json:"deleted"`
}

type UpdateCommand struct {
	AccessToken string
	Type        string
	Status      string
	Quota       *int
	Proxy       string
}
type UpdateResult struct {
	Item AccountView `json:"item"`
}
type DeleteByIDCommand struct{ IDs []string }
type UpdateByIDCommand struct {
	ID     string
	Type   *string
	Status *string
	Quota  *int
	Proxy  *string
}

type AcquireImageTokenCommand struct {
	PlanType   string
	SourceType string
	Exclude    []string
	// Model and Operation filter candidates by the account's latest model
	// snapshot. Empty values keep the pre-discovery acquire behavior so
	// non-catalog callers (for example image task recovery) still work.
	Model     string
	Operation string
}
type AcquireImageTokenResult struct {
	AccessToken string
	Account     AccountView
}
type AcquireImageAccountCommand struct{ AccountID string }
type ReleaseImageSlotCommand struct{ AccessToken string }
type ReleaseImageSlotResult struct{ OK bool }
type MarkImageResultCommand struct {
	AccessToken string
	Success     bool
}
type MarkImageResultResult struct{ Account AccountView }
type AcquireTextTokenCommand struct {
	Exclude []string
	// Model and Operation filter candidates by the account's latest model
	// snapshot. Empty values keep the pre-discovery acquire behavior.
	Model     string
	Operation string
}
type AcquireTextTokenResult struct {
	AccessToken string
	Account     AccountView
}

// AcquireTextAccountCommand acquires a specific account for text turns. It does
// not reuse image in-flight slots and requires the account to support the model
// for chat_completions when Model/Operation are provided.
type AcquireTextAccountCommand struct {
	AccountID string
	Model     string
	Operation string
}
type AcquireTextAccountResult struct {
	AccessToken string
	Account     AccountView
}

// RecordTextResultCommand reports a text turn outcome by account ID. invalid_token
// transitions the account to abnormal; transient upstream failures only record fail.
type RecordTextResultCommand struct {
	AccountID  string
	Success    bool
	ErrorClass string
}
type RecordTextResultResult struct {
	Account AccountView
}

// AccountModelEntry is one model+operations projection stored on an account.
type AccountModelEntry struct {
	ID         string   `json:"id"`
	Operations []string `json:"operations"`
	CreatedAt  int64    `json:"created_at,omitempty"`
	OwnedBy    string   `json:"owned_by,omitempty"`
}

// AccountModelSnapshot is the derived capability state of one account.
// It never carries raw upstream responses or tokens.
type AccountModelSnapshot struct {
	AccountID    string              `json:"account_id"`
	Models       []AccountModelEntry `json:"models"`
	DiscoveredAt string              `json:"discovered_at"`
	ExpiresAt    string              `json:"expires_at,omitempty"`
}

// DiscoveryCandidate is a non-sensitive account identifier plus the access
// credential needed by the discovery orchestrator. Credentials must stay on
// the EventHub path and never appear in Admin HTTP responses.
type DiscoveryCandidate struct {
	AccountID      string
	AccessToken    string
	Proxy          string
	Status         string
	NeedsDiscovery bool
	DiscoveryDue   bool
}

type ListDiscoveryCandidatesCommand struct{}
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
	AccountID string
	Error     string
}
type RecordModelDiscoveryFailureResult struct {
	RetryAt string
	OK      bool
}

// CatalogModel is the pool-level union entry for one model ID.
type CatalogModel struct {
	ID         string   `json:"id"`
	Operations []string `json:"operations"`
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
type RemoveInvalidCommand struct {
	AccessToken string
	Event       string
}
type RemoveInvalidResult struct{ Removed bool }
type HealthCommand struct{}
type HealthResult struct {
	Total    int `json:"total"`
	Normal   int `json:"normal"`
	Limited  int `json:"limited"`
	Abnormal int `json:"abnormal"`
	Disabled int `json:"disabled"`
}

type OAuthStartCommand struct{ EmailHint string }
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

type ExportItem struct {
	Type         string `json:"type"`
	Email        string `json:"email"`
	AccountID    string `json:"account_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	Expired      string `json:"expired"`
	LastRefresh  string `json:"last_refresh"`
	Password     string `json:"password,omitempty"`
}
type ExportCommand struct{ AccessTokens []string }
type ExportResult struct {
	Items []ExportItem `json:"items"`
}
type ExportByIDCommand struct{ IDs []string }

type RefreshCommand struct{ AccessTokens []string }
type RefreshByIDCommand struct{ IDs []string }
type RefreshResult struct {
	ProgressID string `json:"progress_id"`
}
type RefreshProgressCommand struct{ ProgressID string }
type RefreshStatusCounts struct {
	Normal   int `json:"正常"`
	Limited  int `json:"限流"`
	Abnormal int `json:"异常"`
	Disabled int `json:"禁用"`
}
type RefreshError struct {
	AccountID string `json:"account_id"`
	Error     string `json:"error"`
}
type RefreshProgress struct {
	ProgressID   string              `json:"progress_id"`
	Total        int                 `json:"total"`
	Processed    int                 `json:"processed"`
	Done         bool                `json:"done"`
	Error        string              `json:"error,omitempty"`
	StatusCounts RefreshStatusCounts `json:"status_counts"`
	TotalQuota   int                 `json:"total_quota"`
	Refreshed    int                 `json:"refreshed"`
	Errors       []RefreshError      `json:"errors"`
}
type RefreshProgressResult struct{ Progress RefreshProgress }
