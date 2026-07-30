// Package events defines the Codex OAuth account-pool owner's typed EventHub contract.
package events

const (
	TopicList         = "aiproxy.codex.accountpool.command.list"
	TopicImport       = "aiproxy.codex.accountpool.command.import"
	TopicDelete       = "aiproxy.codex.accountpool.command.delete"
	TopicUpdate       = "aiproxy.codex.accountpool.command.update"
	TopicAcquire      = "aiproxy.codex.accountpool.command.acquire"
	TopicRecordResult = "aiproxy.codex.accountpool.command.record_result"
	TopicRefreshToken = "aiproxy.codex.accountpool.command.refresh_token"
	TopicRefreshByID  = "aiproxy.codex.accountpool.command.refresh_by_id"
	TopicHealth       = "aiproxy.codex.accountpool.command.health"
	TopicOAuthStart   = "aiproxy.codex.accountpool.command.oauth_start"
	TopicOAuthFinish  = "aiproxy.codex.accountpool.command.oauth_finish"
)

const (
	StatusNormal   = "normal"
	StatusAbnormal = "abnormal"
	StatusDisabled = "disabled"
)

const (
	ErrorInvalidToken = "invalid_token"
	ErrorRateLimit    = "rate_limit"
	ErrorTimeout      = "timeout"
	ErrorNetwork      = "network"
	ErrorUpstream     = "upstream"
	ErrorClient       = "client"
)

// AccountView is the redacted management projection. It never contains
// access, refresh, or ID tokens, account IDs, or proxy URLs.
type AccountView struct {
	ID                         string         `json:"id"`
	Email                      string         `json:"email,omitempty"`
	PlanType                   string         `json:"plan_type,omitempty"`
	Status                     string         `json:"status"`
	Success                    int            `json:"success"`
	Fail                       int            `json:"fail"`
	CreatedAt                  string         `json:"created_at,omitempty"`
	LastUsedAt                 string         `json:"last_used_at,omitempty"`
	LastTokenRefreshAt         string         `json:"last_token_refresh_at,omitempty"`
	LastTokenRefreshErrorAt    string         `json:"last_token_refresh_error_at,omitempty"`
	LastTokenRefreshErrorClass string         `json:"last_token_refresh_error_class,omitempty"`
	Cooldowns                  []CooldownView `json:"cooldowns,omitempty"`
}

type CooldownView struct {
	Model      string `json:"model"`
	Until      string `json:"until"`
	ErrorClass string `json:"error_class"`
}

// CredentialInput is the deliberate secret-bearing Admin import contract.
// It never appears in a list or health response.
type CredentialInput struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	Email        string `json:"email,omitempty"`
	Expired      string `json:"expired,omitempty"`
	Proxy        string `json:"proxy,omitempty"`
}

type ListCommand struct{}
type ListResult struct {
	Items []AccountView `json:"items"`
}

type ImportCommand struct{ Accounts []CredentialInput }
type ImportResult struct {
	Added   int `json:"added"`
	Updated int `json:"updated"`
	Skipped int `json:"skipped"`
}

type DeleteCommand struct{ IDs []string }
type DeleteResult struct {
	Deleted int `json:"deleted"`
}

type UpdateCommand struct {
	ID     string
	Status *string
	Proxy  *string
}
type UpdateResult struct {
	Item AccountView `json:"item"`
}

// AcquireResult contains only request-time credentials. It is restricted to
// the EventHub path and must not be returned from any HTTP adapter.
type AcquireCommand struct {
	Model   string
	Exclude []string
}
type AcquireResult struct {
	AccountID       string
	AccessToken     string
	AccountIDHeader string
	Proxy           string
}

type RecordResultCommand struct {
	AccountID         string
	Model             string
	Success           bool
	ErrorClass        string
	RetryAfterSeconds int
}
type RecordResultResult struct{ Account AccountView }

type RefreshTokenCommand struct{ AccountID string }
type RefreshTokenResult struct {
	AccountID        string
	AccessToken      string
	AccountIDHeader  string
	Proxy            string
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
