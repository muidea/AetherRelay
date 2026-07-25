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
	TopicRemoveInvalid       = "aiproxy.chatgpt.accountpool.command.remove_invalid"
	TopicHealth              = "aiproxy.chatgpt.accountpool.command.health"
	TopicOAuthStart          = "aiproxy.chatgpt.accountpool.command.oauth_start"
	TopicOAuthFinish         = "aiproxy.chatgpt.accountpool.command.oauth_finish"
	TopicExport              = "aiproxy.chatgpt.accountpool.command.export"
	TopicRefresh             = "aiproxy.chatgpt.accountpool.command.refresh"
	TopicRefreshProgress     = "aiproxy.chatgpt.accountpool.command.refresh_progress"
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
type AcquireTextTokenCommand struct{ Exclude []string }
type AcquireTextTokenResult struct {
	AccessToken string
	Account     AccountView
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
