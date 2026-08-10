package usage

import (
	"context"
	"io"
	"time"

	"aetherrelay/internal/pkg/aetherrelayclientaccess"
)

// 持久化状态。
const (
	StateStarted   = "started"
	StateCompleted = "completed"
)

// OutcomeProcessInterrupted 用于启动恢复遗留 started 行。
const OutcomeProcessInterrupted = "process_interrupted"

// StartRecord 是请求被接受后、访问上游前写入的登记。
type StartRecord struct {
	EventID        string
	RoundID        int64
	StartedAt      time.Time
	APIKeyID       string
	Operation      string
	Route          string
	ClientEndpoint string
	ClientProtocol string
	Provider       string
	Model          string
}

// CompleteRecord 是请求退出路径上的最终结算。
type CompleteRecord struct {
	EventID                  string
	CompletedAt              time.Time
	Provider                 string
	Model                    string
	UpstreamProtocol         string
	UpstreamEndpoint         string
	ConversionMode           string
	ConversionLevel          int
	ConversionDuration       time.Duration
	ConversionDegraded       bool
	IgnoredFeatures          []string
	UnsupportedFeatures      []string
	InputTokens              int64
	OutputTokens             int64
	CachedInputTokens        int64
	CacheCreationInputTokens int64
	HTTPStatus               int
	Outcome                  string
	ErrorCode                string
	Duration                 time.Duration
	UpstreamDuration         time.Duration
	UpstreamStatus           int
	UpstreamContentType      string
	UpstreamContentLength    int64
	UpstreamTransferEncoding string
	Stream                   bool
	Estimated                bool
}

// UsageFilter 用于 Dashboard / 导出筛选。
type UsageFilter struct {
	From      time.Time
	To        time.Time
	APIKeyID  string
	Provider  string
	Model     string
	Outcome   string
	Estimated *bool
	AllTime   bool
}

// EventFilter 用于明细分页。
type EventFilter struct {
	UsageFilter
	PageSize int
	Cursor   string
}

// Summary 是聚合统计口径。
type Summary struct {
	Requests        int64   `json:"requests"`
	SuccessRequests int64   `json:"success_requests"`
	FailedRequests  int64   `json:"failed_requests"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	TotalTokens     int64   `json:"total_tokens"`
	AvgTokensPerReq float64 `json:"average_tokens_per_request"`
	SuccessRate     float64 `json:"success_rate"`
}

// DailyBucket 是按 UTC 日期的趋势点。
type DailyBucket struct {
	Date         string `json:"date"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
}

// KeySummary 是按 api_key_id 的汇总。
type KeySummary struct {
	APIKeyID        string     `json:"api_key_id"`
	Status          string     `json:"status,omitempty"`
	Requests        int64      `json:"requests"`
	SuccessRequests int64      `json:"success_requests"`
	FailedRequests  int64      `json:"failed_requests"`
	InputTokens     int64      `json:"input_tokens"`
	OutputTokens    int64      `json:"output_tokens"`
	TotalTokens     int64      `json:"total_tokens"`
	LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
}

// ClientAPIKeyMetadata stores lifecycle timestamps independently from the
// credential definition. It never contains the raw key or its digest.
type ClientAPIKeyMetadata struct {
	ID         string     `json:"id"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}
type ClientAPIKeyRecord struct {
	ID             string
	Hash           string
	Enabled        bool
	CreatedAt      time.Time
	LastUsedAt     *time.Time
	LastRotatedAt  *time.Time
	RevokedAt      *time.Time
	ProviderAccess clientaccess.Policy
}

// Dashboard 是 Web / Admin API 的主查询结果。
type Dashboard struct {
	Scope    ScopeInfo     `json:"scope"`
	Summary  Summary       `json:"summary"`
	Daily    []DailyBucket `json:"daily"`
	ByAPIKey []KeySummary  `json:"by_api_key"`
}

// ScopeInfo 描述查询时间范围。
type ScopeInfo struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Timezone string    `json:"timezone"`
}

// Event 是一条安全明细(无正文/密钥)。
type Event struct {
	EventID                  string     `json:"event_id"`
	RoundID                  int64      `json:"round_id,omitempty"`
	StartedAt                time.Time  `json:"started_at"`
	CompletedAt              *time.Time `json:"completed_at,omitempty"`
	APIKeyID                 string     `json:"api_key_id"`
	Provider                 string     `json:"provider,omitempty"`
	Model                    string     `json:"model,omitempty"`
	Operation                string     `json:"operation,omitempty"`
	Route                    string     `json:"route,omitempty"`
	ClientEndpoint           string     `json:"client_endpoint,omitempty"`
	ClientProtocol           string     `json:"client_protocol,omitempty"`
	UpstreamProtocol         string     `json:"upstream_protocol,omitempty"`
	UpstreamEndpoint         string     `json:"upstream_endpoint,omitempty"`
	ConversionMode           string     `json:"conversion_mode,omitempty"`
	ConversionLevel          int        `json:"conversion_level,omitempty"`
	ConversionDurationMS     int64      `json:"conversion_duration_ms,omitempty"`
	ConversionDegraded       bool       `json:"conversion_degraded,omitempty"`
	IgnoredFeatures          []string   `json:"ignored_features,omitempty"`
	UnsupportedFeatures      []string   `json:"unsupported_features,omitempty"`
	InputTokens              int64      `json:"input_tokens"`
	OutputTokens             int64      `json:"output_tokens"`
	TotalTokens              int64      `json:"total_tokens"`
	CachedInputTokens        int64      `json:"cached_input_tokens"`
	CacheCreationInputTokens int64      `json:"cache_creation_input_tokens"`
	HTTPStatus               int        `json:"http_status,omitempty"`
	Outcome                  string     `json:"outcome,omitempty"`
	ErrorCode                string     `json:"error_code,omitempty"`
	DurationMS               int64      `json:"duration_ms,omitempty"`
	UpstreamDurationMS       int64      `json:"upstream_duration_ms,omitempty"`
	UpstreamStatus           int        `json:"upstream_status,omitempty"`
	UpstreamContentType      string     `json:"upstream_content_type,omitempty"`
	UpstreamContentLength    int64      `json:"upstream_content_length,omitempty"`
	UpstreamTransferEncoding string     `json:"upstream_transfer_encoding,omitempty"`
	Stream                   bool       `json:"stream"`
	Estimated                bool       `json:"estimated"`
	State                    string     `json:"state"`
}

// EventPage 是 cursor 分页结果。
type EventPage struct {
	Events     []Event `json:"events"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

// FilterOptionsQuery 限定用量观测 facet 的提取时间窗。
// 配置侧合并由 admin 层完成；Store 只返回 usage 半结果。
type FilterOptionsQuery struct {
	From    time.Time
	To      time.Time
	AllTime bool
}

// FilterOptionsTruncation 标记各维度是否因上限被截断。
type FilterOptionsTruncation struct {
	APIKeyIDs bool
	Providers bool
	Models    bool
}

// FilterOptionsResult 是 usage 观测到的 distinct 值（不含配置合并）。
type FilterOptionsResult struct {
	APIKeyIDs []string
	Providers []string
	Models    []string
	Truncated FilterOptionsTruncation
	// 实际扫描窗口。
	From time.Time
	To   time.Time
}

// Store 是用量持久化权威接口。
type Store interface {
	Start(context.Context, StartRecord) error
	Complete(context.Context, CompleteRecord) error
	Dashboard(context.Context, UsageFilter) (Dashboard, error)
	Count(context.Context, UsageFilter) (int64, error)
	Events(context.Context, EventFilter) (EventPage, error)
	ExportCSV(context.Context, UsageFilter, io.Writer) error
	// FilterOptions 返回时间窗内的 distinct api_key_id/provider/model（有上限）。
	FilterOptions(context.Context, FilterOptionsQuery) (FilterOptionsResult, error)
	RecoverInterrupted(context.Context, time.Time) (int64, error)
	Checkpoint(context.Context) error
	Close() error
	Healthy() bool
	// AllTimeByKey 供 metrics 启动镜像初始化。
	AllTimeByKey(context.Context) (map[string]Summary, error)
	EnsureClientAPIKey(context.Context, string, time.Time) error
	TouchClientAPIKey(context.Context, string, time.Time) error
	ClientAPIKeyMetadata(context.Context) (map[string]ClientAPIKeyMetadata, error)
	ListClientAPIKeys(context.Context) (map[string]ClientAPIKeyRecord, error)
	CreateClientAPIKey(context.Context, ClientAPIKeyRecord) error
	SetClientAPIKeyEnabled(context.Context, string, bool) error
	RotateClientAPIKey(context.Context, string, string, time.Time) error
	RevokeClientAPIKey(context.Context, string, time.Time) error
	DeleteClientAPIKey(context.Context, string) error
	SetClientAPIKeyProviderAccess(context.Context, string, clientaccess.Policy) error
	ClientAPIKeyIDsForProvider(context.Context, string) ([]string, error)
}
