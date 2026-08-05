package config

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 默认体大小上限:入站 32MiB,上游响应 64MiB。
// 流式:累计输出默认 64MiB,单条 SSE 行默认 1MiB。
const (
	DefaultMaxRequestBodyBytes      int64 = 32 << 20
	DefaultMaxUpstreamResponseBytes int64 = 64 << 20
	DefaultMaxStreamBytes           int64 = 64 << 20
	DefaultMaxSSELineBytes          int64 = 1 << 20

	// Admin 登录安全默认值与边界。
	DefaultAdminBasePath                 = "/admin"
	DefaultAdminLanguage                 = "zh-CN"
	DefaultAdminSessionTTLSeconds        = 28800
	MinAdminSessionTTLSeconds            = 300
	MaxAdminSessionTTLSeconds            = 86400
	MaxAdminBasePathLength               = 128
	MaxAdminUsernameLength               = 64
	AdminArgon2MemoryKiB          uint32 = 65536
	AdminArgon2Time               uint32 = 3
	AdminArgon2Parallelism        uint8  = 1
	AdminArgon2MinSaltBytes              = 16
	AdminArgon2MinKeyBytes               = 32
)

type Config struct {
	ListenAddr string
	// MaxRequestBodyBytes 限制入站请求体大小;<=0 时使用默认值。
	MaxRequestBodyBytes int64
	// MaxUpstreamResponseBytes 限制上游非流式响应读取上限;<=0 时使用默认值。
	MaxUpstreamResponseBytes int64
	// MaxStreamBytes 限制单次流式响应累计转发/累积字节;<=0 时使用默认值。
	MaxStreamBytes int64
	// MaxSSELineBytes 限制单条 SSE 行(到 \n)最大字节;<=0 时使用默认值。
	MaxSSELineBytes int64
	// ArchiveFullContent 为 false 时仅写元数据,不落盘完整请求/响应正文。
	ArchiveFullContent bool
	// State is the single persistent workspace authority. The fields below are
	// runtime projections and are never independently configured.
	State                StateConfig
	InteractionDir       string
	InteractionRetention int
	DebugLog             bool
	LogFormat            string
	RequestTimeout       time.Duration
	StreamIdleTimeout    time.Duration
	MetricsRemoteAccess  bool
	MetricsAllowedCIDRs  []string
	SLO                  SLOConfig
	// AdminAuth 描述可选的 Admin 登录安全配置。默认关闭,保持 loopback-only 兼容行为。
	AdminAuth AdminAuthConfig
	// ClientAPIKeys 是客户端调用方识别与用量归属的唯一配置 authority。
	// 业务请求必须携带并匹配一个 enabled Key；配置可暂时为空，以便通过本地 Admin 创建首个 Key。
	ClientAPIKeys map[string]ClientAPIKey
	// UsageStore 描述进程内嵌 DuckDB 持久化统计存储。路径与资源参数变更需重启。
	UsageStore UsageStoreConfig
	// ChatGPTWeb 是 ChatGPT Web 账号池和图片能力的本地运行配置。
	// DataDir is derived from State.Dir and is not independently configured.
	ChatGPTWeb ChatGPTWebConfig
	// CodexOAuth is a separate Codex CLI OAuth account domain. It never shares
	// ChatGPT Web credentials, conversations, or account runtime state.
	CodexOAuth CodexOAuthConfig
	Providers  map[string]Provider
	// ModelMetadata only enriches models supplied by providers or runtime discovery.
	// Entries never publish models and never participate in route ownership.
	ModelMetadata map[string]ModelMetadata
}

// StateConfig defines the single local workspace for durable ai-proxy state.
// Database and all managed directories are resolved beneath Dir.
type StateConfig struct {
	Dir                  string
	Database             string
	MemoryLimit          string
	Threads              int
	QueryCacheSeconds    int
	InteractionRetention int
}

func (s StateConfig) ImagesDir() string       { return filepath.Join(s.Dir, "images") }
func (s StateConfig) ThumbnailsDir() string   { return filepath.Join(s.Dir, "image_thumbnails") }
func (s StateConfig) InteractionsDir() string { return filepath.Join(s.Dir, "interactions") }

// AdminAuthConfig 描述 Admin 可选账号密码登录与 basePath。
// 开关默认 false;开启后必须配置单管理员账号与 Argon2id PHC 密码哈希。
// admin_base_path 是启动期路由配置,热更新变更后需重启才生效。
type AdminAuthConfig struct {
	// Enabled 为 true 时取消 Admin loopback 限制,强制先登录再访问。
	Enabled bool
	// BasePath 是 Admin 页面与 API 前缀,默认 /admin。
	BasePath string
	// DefaultLanguage 是 Admin Web 的实例默认显示语言。仅支持 zh-CN 与 en-US。
	// 浏览器或 URL 的临时选择可覆盖此值，不影响代理数据面行为。
	DefaultLanguage string
	// Username 是单管理员账号,区分大小写。
	Username string
	// PasswordHash 是 Argon2id PHC 字符串;禁止明文。
	PasswordHash string
	// SessionCookieSecure 为 true 时会话 Cookie 仅随 HTTPS 请求发送；默认 false，兼容 HTTP Admin。
	SessionCookieSecure bool
	// SessionTTLSeconds 是会话绝对有效期,默认 28800(8h),范围 300~86400。
	SessionTTLSeconds int
}

// ClientAPIKey 描述一个客户端调用方密钥条目。
// ID 规范化为小写;APIKey 可含 ${ENV} 展开结果;Enabled=false 时请求返回 401。
type ClientAPIKey struct {
	ID         string
	APIKey     string
	APIKeyHash string
	Enabled    bool
}

// UsageStoreConfig is the runtime projection consumed by the usage owner. It
// is always derived from State and has no independent YAML or environment key.
type UsageStoreConfig struct {
	// Path is state.database.
	Path string
	// MemoryLimit 默认 256MB;应用层解析后下发 SET memory_limit,禁止透传任意 SQL。
	MemoryLimit string
	// Threads 默认 2,最小 1。
	Threads int
	// QueryCacheSeconds 默认 15;0 关闭缓存。
	QueryCacheSeconds int
}

// ChatGPTWebConfig 描述 ChatGPT Web 专属本地数据与账号刷新策略。
// DataDir is derived from state.dir and cannot be configured separately.
// 启用后，ChatGPT Web 模型由内建 Provider 自动发现并与静态目录合成。
type ChatGPTWebConfig struct {
	Enabled bool
	// ProviderEnabled controls whether the already configured account pool
	// participates in request routing. It is deliberately separate from
	// Enabled: the latter owns process-start lifecycle and storage setup.
	// An omitted value follows Enabled for backwards compatibility.
	ProviderEnabled           bool
	providerEnabledConfigured bool
	// Priority controls the builtin ChatGPT Web candidate position. An omitted
	// value uses DefaultChatGPTWebProviderPriority; an explicit zero is valid.
	Priority                     int
	priorityConfigured           bool
	DataDir                      string
	RefreshAccountIntervalMinute int
	TemporaryChat                TemporaryChatConfig
}

// CodexOAuthConfig enables the native Codex Responses account pool. Its
// routable model catalog is always derived from account-level discovery.
type CodexOAuthConfig struct {
	Enabled bool
	// ProviderEnabled controls routing only. It does not create or destroy the
	// Codex OAuth account-pool runtime, so Admin can safely hot-update it.
	// An omitted value follows Enabled for backwards compatibility.
	ProviderEnabled           bool
	providerEnabledConfigured bool
	// Priority controls the builtin Codex OAuth candidate position. An omitted
	// value uses DefaultCodexOAuthProviderPriority; an explicit zero is valid.
	Priority                     int
	priorityConfigured           bool
	RefreshAccountIntervalMinute int
}

// TemporaryChatConfig controls Admin temporary multi-turn text conversations.
// Retention and capacity limits are positive; cleanup never silently deletes
// history just to make room for a new conversation.
type TemporaryChatConfig struct {
	Enabled                    bool
	RetentionDays              int
	MaxConversations           int
	MaxMessagesPerConversation int
	MaxMessageBytes            int
	TurnTimeoutSeconds         int
}

// ModelMetadata describes optional client-visible metadata for an exact model ID.
type ModelMetadata struct {
	ID                  string
	ContextWindowTokens int
	MaxOutputTokens     int
}

// MaxModelMetadataIDLength 限制 model_metadata id 长度,避免异常配置与标签膨胀。
const MaxModelMetadataIDLength = 256

const (
	// DefaultProviderPriority preserves the existing behavior for configurations
	// that have not opted into explicit provider ordering.
	DefaultProviderPriority = 100
	MinProviderPriority     = -1000
	MaxProviderPriority     = 1000
	// Builtin defaults preserve the established static > Codex > ChatGPT Web
	// route order while allowing an explicit configuration override.
	DefaultCodexOAuthProviderPriority = 90
	DefaultChatGPTWebProviderPriority = 10
)

// SLOConfig 描述可观测性层面的服务等级目标。
type SLOConfig struct {
	// CacheHitRateMin 是单 provider 缓存命中率的最低要求(0~1)。
	CacheHitRateMin float64
	// UpstreamErrorRateMax 是单 provider 上游错误率上限(0~1)。
	UpstreamErrorRateMax float64
	// P99LatencyMaxMS 是单 provider p99 延迟上限(毫秒)。
	P99LatencyMaxMS float64
	// CheckIntervalSeconds 是后台巡检周期;0 表示禁用周期检查。
	CheckIntervalSeconds int
	// ViolationWebhook 是可选 webhook URL,命中 SLO 时异步 POST JSON(短超时/有限并发)。
	// 尽力而为:不与 shutdown 协同;进程退出时在途告警可能丢失。日志仅记录 scheme://host。
	ViolationWebhook string
}

type Provider struct {
	Name     string
	Protocol string
	BaseURL  string
	APIKey   string
	Models   []string
	// Priority selects the first provider for an otherwise equivalent model
	// candidate. Higher values win. An omitted YAML value normalizes to
	// DefaultProviderPriority; an explicit zero remains a valid priority.
	Priority int
	// priorityConfigured distinguishes an omitted YAML key from an explicit
	// priority: 0. It is private so configuration loading remains the sole
	// authority for defaulting.
	priorityConfigured bool
	// Fallback permits this provider to serve after a higher-priority candidate
	// fails before the client response is committed.
	Fallback bool
	// fallbackConfigured distinguishes omitted YAML from false. It is private so
	// only configuration loading owns defaulting.
	fallbackConfigured bool
	// Endpoints 为 provider 显式声明的直通端点(非 protocol 推断)。
	// 取值: chat_completions / messages / responses / completions / embeddings / images。
	Endpoints []string
	// AllowUnauthenticated 仅允许受信 loopback 上游在无 API Key 时启动。
	// 远程 base_url 即使设置 true 也必须 fail-fast。
	AllowUnauthenticated bool
	Disabled             bool
}

func Load(path string) (Config, error) {
	cfg := Config{
		ListenAddr:               "127.0.0.1:8080",
		MaxRequestBodyBytes:      DefaultMaxRequestBodyBytes,
		MaxUpstreamResponseBytes: DefaultMaxUpstreamResponseBytes,
		MaxStreamBytes:           DefaultMaxStreamBytes,
		MaxSSELineBytes:          DefaultMaxSSELineBytes,
		ArchiveFullContent:       true,
		InteractionDir:           "interactions",
		InteractionRetention:     500,
		DebugLog:                 true,
		LogFormat:                "json",
		RequestTimeout:           5 * time.Minute,
		StreamIdleTimeout:        5 * time.Minute,
		AdminAuth: AdminAuthConfig{
			Enabled:             false,
			BasePath:            DefaultAdminBasePath,
			DefaultLanguage:     DefaultAdminLanguage,
			SessionCookieSecure: false,
			SessionTTLSeconds:   DefaultAdminSessionTTLSeconds,
		},
		ClientAPIKeys: map[string]ClientAPIKey{},
		State: StateConfig{
			Dir:                  "var",
			Database:             "state.duckdb",
			MemoryLimit:          "256MB",
			Threads:              2,
			QueryCacheSeconds:    15,
			InteractionRetention: 500,
		},
		ChatGPTWeb: ChatGPTWebConfig{
			Enabled: false,
			TemporaryChat: TemporaryChatConfig{
				Enabled:                    true,
				RetentionDays:              30,
				MaxConversations:           2000,
				MaxMessagesPerConversation: 200,
				MaxMessageBytes:            262144,
				TurnTimeoutSeconds:         300,
			},
		},
		CodexOAuth:    CodexOAuthConfig{Enabled: false},
		Providers:     map[string]Provider{},
		ModelMetadata: map[string]ModelMetadata{},
	}

	path = ResolvePath(path)
	if path != "" {
		if err := loadFile(path, &cfg); err != nil {
			return Config{}, err
		}
	}

	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	ensureProviderNames(&cfg)
	if err := normalize(&cfg, path); err != nil {
		return Config{}, err
	}
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ResolvePath 返回服务实际读取的配置文件路径。显式路径原样保留；未指定时，
// 若当前目录存在 config.yaml 则返回该默认路径。
func ResolvePath(path string) string {
	if strings.TrimSpace(path) != "" {
		return path
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml"
	}
	return ""
}

func loadFile(path string, cfg *Config) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	section := ""
	providerName := ""
	modelName := ""
	clientKeyID := ""
	chatgptWebSub := ""
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := stripComment(scanner.Text())
		if strings.TrimSpace(raw) == "" {
			continue
		}
		indent := countIndent(raw)
		line := strings.TrimSpace(raw)
		key, value, hasValue := splitKV(line)
		if key == "" {
			return fmt.Errorf("%s:%d: invalid config line", path, lineNo)
		}

		var setErr error
		switch {
		case indent == 0 && !hasValue:
			switch key {
			case "server", "state", "providers", "model_metadata", "client_api_keys", "chatgpt_web", "codex_oauth":
				section = key
				providerName = ""
				modelName = ""
				clientKeyID = ""
				chatgptWebSub = ""
			default:
				return fmt.Errorf("%s:%d: unknown section %q", path, lineNo, key)
			}
		case indent == 0:
			section = ""
			providerName = ""
			modelName = ""
			clientKeyID = ""
			if key == "admin_password_hash" {
				setErr = setTopLevel(cfg, key, expandDollarBraceOnly(value))
			} else {
				setErr = setTopLevel(cfg, key, expand(value))
			}
		case section == "server" && indent >= 2:
			if key == "admin_password_hash" {
				setErr = setServer(cfg, key, expandDollarBraceOnly(value))
			} else {
				setErr = setServer(cfg, key, expand(value))
			}
		case section == "state" && indent >= 2:
			setErr = setState(cfg, key, expand(value))
		case section == "chatgpt_web" && indent == 2 && !hasValue && key == "temporary_chat":
			chatgptWebSub = "temporary_chat"
		case section == "chatgpt_web" && indent >= 4 && chatgptWebSub == "temporary_chat":
			setErr = setChatGPTTemporaryChat(cfg, key, expand(value))
		case section == "chatgpt_web" && indent >= 2:
			chatgptWebSub = ""
			setErr = setChatGPTWeb(cfg, key, expand(value))
		case section == "codex_oauth" && indent >= 2:
			setErr = setCodexOAuth(cfg, key, expand(value))
		case section == "client_api_keys" && indent == 2 && !hasValue:
			clientKeyID = key
			providerName = ""
			modelName = ""
			ensureClientAPIKey(cfg, clientKeyID)
		case section == "client_api_keys" && indent >= 4 && clientKeyID != "":
			setErr = setClientAPIKey(cfg, clientKeyID, key, expand(value))
		case section == "providers" && indent == 2 && !hasValue:
			providerName = key
			modelName = ""
			clientKeyID = ""
			ensureProvider(cfg, providerName)
		case section == "providers" && indent >= 4 && providerName != "":
			setErr = setProvider(cfg, providerName, key, expand(value))
		case section == "model_metadata" && indent == 2 && !hasValue:
			modelName = key
			providerName = ""
			clientKeyID = ""
			ensureModelMetadata(cfg, modelName)
		case section == "model_metadata" && indent >= 4 && modelName != "":
			setErr = setModelMetadata(cfg, modelName, key, expand(value))
		default:
			return fmt.Errorf("%s:%d: unsupported config shape", path, lineNo)
		}
		if setErr != nil {
			return fmt.Errorf("%s:%d: %w", path, lineNo, setErr)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func setTopLevel(cfg *Config, key, value string) error {
	switch key {
	case "usage_file", "AI_PROXY_USAGE_FILE", "usage_store", "interaction_dir", "interaction_retention":
		return fmt.Errorf("%s is not supported; configure the state workspace instead", key)
	case "inbound_api_key":
		return fmt.Errorf("inbound_api_key is not supported; use client_api_keys for caller identity and usage attribution")
	case "debug_log":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("debug_log: %w", err)
		}
		cfg.DebugLog = b
	case "log_format":
		cfg.LogFormat = value
	case "port":
		cfg.ListenAddr = addrFromPort(value)
	case "listen_addr":
		cfg.ListenAddr = value
	case "max_request_body_bytes":
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("max_request_body_bytes: %w", err)
		}
		cfg.MaxRequestBodyBytes = n
	case "max_upstream_response_bytes":
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("max_upstream_response_bytes: %w", err)
		}
		cfg.MaxUpstreamResponseBytes = n
	case "max_stream_bytes":
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("max_stream_bytes: %w", err)
		}
		cfg.MaxStreamBytes = n
	case "max_sse_line_bytes":
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("max_sse_line_bytes: %w", err)
		}
		cfg.MaxSSELineBytes = n
	case "archive_full_content":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("archive_full_content: %w", err)
		}
		cfg.ArchiveFullContent = b
	case "request_timeout_seconds":
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("request_timeout_seconds: %w", err)
		}
		cfg.RequestTimeout = time.Duration(n) * time.Second
	case "stream_idle_timeout_seconds":
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("stream_idle_timeout_seconds: %w", err)
		}
		cfg.StreamIdleTimeout = time.Duration(n) * time.Second
	case "default_provider":
		return fmt.Errorf("default_provider is not supported; routing uses effective model candidates only")
	case "metrics_remote_access":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("metrics_remote_access: %w", err)
		}
		cfg.MetricsRemoteAccess = b
	case "metrics_allowed_cidrs":
		cfg.MetricsAllowedCIDRs = parseList(value)
	case "slo_cache_hit_rate_min":
		f, err := parseStrictFloat(value)
		if err != nil {
			return fmt.Errorf("slo_cache_hit_rate_min: %w", err)
		}
		cfg.SLO.CacheHitRateMin = f
	case "slo_upstream_error_rate_max":
		f, err := parseStrictFloat(value)
		if err != nil {
			return fmt.Errorf("slo_upstream_error_rate_max: %w", err)
		}
		cfg.SLO.UpstreamErrorRateMax = f
	case "slo_p99_latency_max_ms":
		f, err := parseStrictFloat(value)
		if err != nil {
			return fmt.Errorf("slo_p99_latency_max_ms: %w", err)
		}
		cfg.SLO.P99LatencyMaxMS = f
	case "slo_check_interval_seconds":
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("slo_check_interval_seconds: %w", err)
		}
		cfg.SLO.CheckIntervalSeconds = n
	case "slo_violation_webhook":
		cfg.SLO.ViolationWebhook = value
	case "admin_auth_enabled":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("admin_auth_enabled: %w", err)
		}
		cfg.AdminAuth.Enabled = b
	case "admin_base_path":
		cfg.AdminAuth.BasePath = strings.TrimSpace(value)
	case "admin_default_language":
		cfg.AdminAuth.DefaultLanguage = strings.TrimSpace(value)
	case "admin_username":
		// 账号区分大小写；仅 trim 首尾空白以便配置友好。
		cfg.AdminAuth.Username = strings.TrimSpace(value)
	case "admin_password_hash":
		// 不在错误中回显哈希原文。
		cfg.AdminAuth.PasswordHash = strings.TrimSpace(value)
	case "admin_session_cookie_secure":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("admin_session_cookie_secure: %w", err)
		}
		cfg.AdminAuth.SessionCookieSecure = b
	case "admin_session_ttl_seconds":
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("admin_session_ttl_seconds: %w", err)
		}
		cfg.AdminAuth.SessionTTLSeconds = n
	default:
		return fmt.Errorf("unknown config key %q", key)
	}
	return nil
}

func setState(cfg *Config, key, value string) error {
	switch key {
	case "dir":
		cfg.State.Dir = strings.TrimSpace(value)
	case "database":
		cfg.State.Database = strings.TrimSpace(value)
	case "memory_limit":
		cfg.State.MemoryLimit = strings.TrimSpace(value)
	case "threads":
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("state.threads: %w", err)
		}
		cfg.State.Threads = n
	case "query_cache_seconds":
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("state.query_cache_seconds: %w", err)
		}
		cfg.State.QueryCacheSeconds = n
	case "interaction_retention":
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("state.interaction_retention: %w", err)
		}
		cfg.State.InteractionRetention = n
	default:
		return fmt.Errorf("state: unknown key %q", key)
	}
	return nil
}

func setChatGPTWeb(cfg *Config, key, value string) error {
	switch key {
	case "enabled":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("chatgpt_web.enabled: %w", err)
		}
		cfg.ChatGPTWeb.Enabled = b
	case "provider_enabled":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("chatgpt_web.provider_enabled: %w", err)
		}
		cfg.ChatGPTWeb.ProviderEnabled = b
		cfg.ChatGPTWeb.providerEnabledConfigured = true
	case "priority":
		n, err := parseStrictInt(value)
		if err != nil {
			return fmt.Errorf("chatgpt_web.priority: %w", err)
		}
		cfg.ChatGPTWeb.Priority = n
		cfg.ChatGPTWeb.priorityConfigured = true
	case "refresh_account_interval_minute":
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("chatgpt_web.refresh_account_interval_minute: %w", err)
		}
		cfg.ChatGPTWeb.RefreshAccountIntervalMinute = n
	default:
		return fmt.Errorf("chatgpt_web: unknown key %q", key)
	}
	return nil
}

func setCodexOAuth(cfg *Config, key, value string) error {
	switch key {
	case "enabled":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("codex_oauth.enabled: %w", err)
		}
		cfg.CodexOAuth.Enabled = b
	case "provider_enabled":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("codex_oauth.provider_enabled: %w", err)
		}
		cfg.CodexOAuth.ProviderEnabled = b
		cfg.CodexOAuth.providerEnabledConfigured = true
	case "priority":
		n, err := parseStrictInt(value)
		if err != nil {
			return fmt.Errorf("codex_oauth.priority: %w", err)
		}
		cfg.CodexOAuth.Priority = n
		cfg.CodexOAuth.priorityConfigured = true
	case "refresh_account_interval_minute":
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("codex_oauth.refresh_account_interval_minute: %w", err)
		}
		cfg.CodexOAuth.RefreshAccountIntervalMinute = n
	default:
		return fmt.Errorf("codex_oauth: unknown key %q", key)
	}
	return nil
}

func setChatGPTTemporaryChat(cfg *Config, key, value string) error {
	switch key {
	case "enabled":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("chatgpt_web.temporary_chat.enabled: %w", err)
		}
		cfg.ChatGPTWeb.TemporaryChat.Enabled = b
	case "retention_days":
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("chatgpt_web.temporary_chat.retention_days: %w", err)
		}
		cfg.ChatGPTWeb.TemporaryChat.RetentionDays = n
	case "max_conversations":
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("chatgpt_web.temporary_chat.max_conversations: %w", err)
		}
		cfg.ChatGPTWeb.TemporaryChat.MaxConversations = n
	case "max_messages_per_conversation":
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("chatgpt_web.temporary_chat.max_messages_per_conversation: %w", err)
		}
		cfg.ChatGPTWeb.TemporaryChat.MaxMessagesPerConversation = n
	case "max_message_bytes":
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("chatgpt_web.temporary_chat.max_message_bytes: %w", err)
		}
		cfg.ChatGPTWeb.TemporaryChat.MaxMessageBytes = n
	case "turn_timeout_seconds":
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("chatgpt_web.temporary_chat.turn_timeout_seconds: %w", err)
		}
		cfg.ChatGPTWeb.TemporaryChat.TurnTimeoutSeconds = n
	default:
		return fmt.Errorf("chatgpt_web.temporary_chat: unknown key %q", key)
	}
	return nil
}

func ensureClientAPIKey(cfg *Config, id string) ClientAPIKey {
	if cfg.ClientAPIKeys == nil {
		cfg.ClientAPIKeys = map[string]ClientAPIKey{}
	}
	if existing, ok := cfg.ClientAPIKeys[id]; ok {
		return existing
	}
	entry := ClientAPIKey{ID: id, Enabled: true}
	cfg.ClientAPIKeys[id] = entry
	return entry
}

func setClientAPIKey(cfg *Config, id, key, value string) error {
	entry := ensureClientAPIKey(cfg, id)
	switch key {
	case "api_key":
		entry.APIKey = value
	case "api_key_hash":
		entry.APIKeyHash = value
	case "enabled":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("client_api_keys.%s.enabled: %w", id, err)
		}
		entry.Enabled = b
	default:
		return fmt.Errorf("client_api_keys.%s: unknown key %q", id, key)
	}
	entry.ID = id
	cfg.ClientAPIKeys[id] = entry
	return nil
}

func setServer(cfg *Config, key, value string) error {
	// server 段与顶层键共享同一套字段。
	return setTopLevel(cfg, key, value)
}

func setProvider(cfg *Config, name, key, value string) error {
	provider := ensureProvider(cfg, name)
	switch key {
	case "base_url":
		provider.BaseURL = value
	case "api_key":
		provider.APIKey = value
	case "type", "protocol":
		provider.Protocol = strings.ToLower(value)
	case "models", "model_patterns":
		// models 严格区分大小写,与请求 body.model 原文匹配。
		provider.Models = parseModelList(value)
	case "priority":
		n, err := parseStrictInt(value)
		if err != nil {
			return fmt.Errorf("providers.%s.priority: %w", name, err)
		}
		provider.Priority = n
		provider.priorityConfigured = true
	case "fallback":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("providers.%s.fallback: %w", name, err)
		}
		provider.Fallback = b
		provider.fallbackConfigured = true
	case "fallbacks", "fallback_providers":
		return fmt.Errorf("providers.%s: use priority and fallback instead of %q", name, key)
	case "endpoints", "endpoint":
		endpoints, err := parseProviderEndpoints(value)
		if err != nil {
			return fmt.Errorf("providers.%s.endpoints: %w", name, err)
		}
		provider.Endpoints = endpoints
	case "allow_unauthenticated":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("providers.%s.allow_unauthenticated: %w", name, err)
		}
		provider.AllowUnauthenticated = b
	case "enabled":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("providers.%s.enabled: %w", name, err)
		}
		provider.Disabled = !b
	default:
		return fmt.Errorf("providers.%s: unknown key %q", name, key)
	}
	cfg.Providers[name] = provider
	return nil
}

func ensureProvider(cfg *Config, name string) Provider {
	provider, ok := cfg.Providers[name]
	if !ok {
		provider = Provider{Name: name}
	}
	cfg.Providers[name] = provider
	return provider
}

func ensureModelMetadata(cfg *Config, id string) ModelMetadata {
	if cfg.ModelMetadata == nil {
		cfg.ModelMetadata = map[string]ModelMetadata{}
	}
	info, ok := cfg.ModelMetadata[id]
	if !ok {
		info = ModelMetadata{ID: id}
	}
	if info.ID == "" {
		info.ID = id
	}
	cfg.ModelMetadata[id] = info
	return info
}

func setModelMetadata(cfg *Config, id, key, value string) error {
	info := ensureModelMetadata(cfg, id)
	switch strings.ToLower(key) {
	case "context_window_tokens", "contextwindowtokens", "context_window":
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("model_metadata.%s.%s: %w", id, key, err)
		}
		info.ContextWindowTokens = n
	case "max_output_tokens", "maxoutputtokens":
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("model_metadata.%s.%s: %w", id, key, err)
		}
		info.MaxOutputTokens = n
	default:
		return fmt.Errorf("model_metadata.%s: unknown key %q", id, key)
	}
	cfg.ModelMetadata[id] = info
	return nil
}

func applyEnv(cfg *Config) error {
	if value := os.Getenv("AI_PROXY_LISTEN_ADDR"); value != "" {
		cfg.ListenAddr = value
	}
	if value := os.Getenv("AI_PROXY_PORT"); value != "" {
		cfg.ListenAddr = addrFromPort(value)
	}
	if os.Getenv("AI_PROXY_INBOUND_API_KEY") != "" {
		return fmt.Errorf("AI_PROXY_INBOUND_API_KEY is not supported; configure client_api_keys instead")
	}
	if os.Getenv("AI_PROXY_USAGE_FILE") != "" || os.Getenv("AI_PROXY_USAGE_STORE_PATH") != "" || os.Getenv("AI_PROXY_CHATGPT_WEB_DATA_DIR") != "" || os.Getenv("AI_PROXY_INTERACTION_DIR") != "" || os.Getenv("AI_PROXY_INTERACTION_RETENTION") != "" {
		return fmt.Errorf("legacy state environment variables are not supported; configure the state workspace instead")
	}
	if value := os.Getenv("AI_PROXY_MAX_REQUEST_BODY_BYTES"); value != "" {
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_MAX_REQUEST_BODY_BYTES: %w", err)
		}
		cfg.MaxRequestBodyBytes = n
	}
	if value := os.Getenv("AI_PROXY_MAX_UPSTREAM_RESPONSE_BYTES"); value != "" {
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_MAX_UPSTREAM_RESPONSE_BYTES: %w", err)
		}
		cfg.MaxUpstreamResponseBytes = n
	}
	if value := os.Getenv("AI_PROXY_MAX_STREAM_BYTES"); value != "" {
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_MAX_STREAM_BYTES: %w", err)
		}
		cfg.MaxStreamBytes = n
	}
	if value := os.Getenv("AI_PROXY_MAX_SSE_LINE_BYTES"); value != "" {
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_MAX_SSE_LINE_BYTES: %w", err)
		}
		cfg.MaxSSELineBytes = n
	}
	if value := os.Getenv("AI_PROXY_ARCHIVE_FULL_CONTENT"); value != "" {
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_ARCHIVE_FULL_CONTENT: %w", err)
		}
		cfg.ArchiveFullContent = b
	}
	if value := os.Getenv("AI_PROXY_CHATGPT_WEB_ENABLED"); value != "" {
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_CHATGPT_WEB_ENABLED: %w", err)
		}
		cfg.ChatGPTWeb.Enabled = b
	}
	if value := os.Getenv("AI_PROXY_CHATGPT_WEB_PROVIDER_ENABLED"); value != "" {
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_CHATGPT_WEB_PROVIDER_ENABLED: %w", err)
		}
		cfg.ChatGPTWeb.ProviderEnabled = b
		cfg.ChatGPTWeb.providerEnabledConfigured = true
	}
	if value := os.Getenv("AI_PROXY_CHATGPT_WEB_PRIORITY"); value != "" {
		n, err := parseStrictInt(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_CHATGPT_WEB_PRIORITY: %w", err)
		}
		cfg.ChatGPTWeb.Priority = n
		cfg.ChatGPTWeb.priorityConfigured = true
	}
	if value := os.Getenv("AI_PROXY_CHATGPT_WEB_REFRESH_ACCOUNT_INTERVAL_MINUTE"); value != "" {
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_CHATGPT_WEB_REFRESH_ACCOUNT_INTERVAL_MINUTE: %w", err)
		}
		cfg.ChatGPTWeb.RefreshAccountIntervalMinute = n
	}
	if value := os.Getenv("AI_PROXY_CODEX_OAUTH_ENABLED"); value != "" {
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_CODEX_OAUTH_ENABLED: %w", err)
		}
		cfg.CodexOAuth.Enabled = b
	}
	if value := os.Getenv("AI_PROXY_CODEX_OAUTH_PROVIDER_ENABLED"); value != "" {
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_CODEX_OAUTH_PROVIDER_ENABLED: %w", err)
		}
		cfg.CodexOAuth.ProviderEnabled = b
		cfg.CodexOAuth.providerEnabledConfigured = true
	}
	if value := os.Getenv("AI_PROXY_CODEX_OAUTH_PRIORITY"); value != "" {
		n, err := parseStrictInt(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_CODEX_OAUTH_PRIORITY: %w", err)
		}
		cfg.CodexOAuth.Priority = n
		cfg.CodexOAuth.priorityConfigured = true
	}
	if value := os.Getenv("AI_PROXY_CODEX_OAUTH_REFRESH_ACCOUNT_INTERVAL_MINUTE"); value != "" {
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_CODEX_OAUTH_REFRESH_ACCOUNT_INTERVAL_MINUTE: %w", err)
		}
		cfg.CodexOAuth.RefreshAccountIntervalMinute = n
	}
	if value := os.Getenv("AI_PROXY_DEBUG_LOG"); value != "" {
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_DEBUG_LOG: %w", err)
		}
		cfg.DebugLog = b
	}
	if value := firstEnv("AI_PROXY_LOG_FORMAT", "LOG_FORMAT"); value != "" {
		cfg.LogFormat = value
	}
	if value := os.Getenv("AI_PROXY_REQUEST_TIMEOUT_SECONDS"); value != "" {
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_REQUEST_TIMEOUT_SECONDS: %w", err)
		}
		cfg.RequestTimeout = time.Duration(n) * time.Second
	}
	if value := os.Getenv("AI_PROXY_STREAM_IDLE_TIMEOUT_SECONDS"); value != "" {
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_STREAM_IDLE_TIMEOUT_SECONDS: %w", err)
		}
		cfg.StreamIdleTimeout = time.Duration(n) * time.Second
	}
	if value := os.Getenv("AI_PROXY_METRICS_REMOTE_ACCESS"); value != "" {
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_METRICS_REMOTE_ACCESS: %w", err)
		}
		cfg.MetricsRemoteAccess = b
	}
	if value := os.Getenv("AI_PROXY_METRICS_ALLOWED_CIDRS"); value != "" {
		cfg.MetricsAllowedCIDRs = parseList(value)
	}
	if value := os.Getenv("AI_PROXY_ADMIN_AUTH_ENABLED"); value != "" {
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_ADMIN_AUTH_ENABLED: %w", err)
		}
		cfg.AdminAuth.Enabled = b
	}
	if value := os.Getenv("AI_PROXY_ADMIN_BASE_PATH"); value != "" {
		cfg.AdminAuth.BasePath = strings.TrimSpace(value)
	}
	if value := os.Getenv("AI_PROXY_ADMIN_USERNAME"); value != "" {
		cfg.AdminAuth.Username = strings.TrimSpace(value)
	}
	if value := os.Getenv("AI_PROXY_ADMIN_PASSWORD_HASH"); value != "" {
		cfg.AdminAuth.PasswordHash = strings.TrimSpace(value)
	}
	if value := os.Getenv("AI_PROXY_ADMIN_SESSION_COOKIE_SECURE"); value != "" {
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_ADMIN_SESSION_COOKIE_SECURE: %w", err)
		}
		cfg.AdminAuth.SessionCookieSecure = b
	}
	if value := os.Getenv("AI_PROXY_ADMIN_SESSION_TTL_SECONDS"); value != "" {
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_ADMIN_SESSION_TTL_SECONDS: %w", err)
		}
		cfg.AdminAuth.SessionTTLSeconds = n
	}

	// Provider 仅从 config 文件声明;不支持通过 env 注入/创建 provider。
	// api_key 等字段仍可用 ${ENV} 在配置文件中展开。
	return nil
}

// ensureProviderNames 只补齐 Provider.Name = map key,不补 protocol/base_url/api_key。
// protocol 与 base_url 必须由配置显式声明,否则 validate 启动失败。
func ensureProviderNames(cfg *Config) {
	for name, provider := range cfg.Providers {
		if provider.Name == "" {
			provider.Name = name
			cfg.Providers[name] = provider
		}
	}
}

func normalize(cfg *Config, configPath string) error {
	cfg.LogFormat = normalizeLogFormat(cfg.LogFormat)
	if cfg.MaxRequestBodyBytes <= 0 {
		cfg.MaxRequestBodyBytes = DefaultMaxRequestBodyBytes
	}
	if cfg.MaxUpstreamResponseBytes <= 0 {
		cfg.MaxUpstreamResponseBytes = DefaultMaxUpstreamResponseBytes
	}
	if cfg.MaxStreamBytes <= 0 {
		cfg.MaxStreamBytes = DefaultMaxStreamBytes
	}
	if cfg.MaxSSELineBytes <= 0 {
		cfg.MaxSSELineBytes = DefaultMaxSSELineBytes
	}
	if err := normalizeState(&cfg.State, configPath); err != nil {
		return err
	}
	// Runtime consumers receive only values derived from the single state
	// workspace; they never interpret independent persistent paths.
	cfg.UsageStore = UsageStoreConfig{
		Path:              cfg.State.Database,
		MemoryLimit:       cfg.State.MemoryLimit,
		Threads:           cfg.State.Threads,
		QueryCacheSeconds: cfg.State.QueryCacheSeconds,
	}
	cfg.InteractionDir = cfg.State.InteractionsDir()
	cfg.InteractionRetention = cfg.State.InteractionRetention
	cfg.ChatGPTWeb.DataDir = cfg.State.Dir
	if !cfg.ChatGPTWeb.providerEnabledConfigured {
		cfg.ChatGPTWeb.ProviderEnabled = cfg.ChatGPTWeb.Enabled
	}
	if !cfg.ChatGPTWeb.priorityConfigured {
		cfg.ChatGPTWeb.Priority = DefaultChatGPTWebProviderPriority
	}
	if !cfg.CodexOAuth.priorityConfigured {
		cfg.CodexOAuth.Priority = DefaultCodexOAuthProviderPriority
	}
	if !cfg.CodexOAuth.providerEnabledConfigured {
		cfg.CodexOAuth.ProviderEnabled = cfg.CodexOAuth.Enabled
	}
	normalizeTemporaryChat(&cfg.ChatGPTWeb.TemporaryChat)
	if err := normalizeClientAPIKeys(cfg); err != nil {
		return err
	}
	normalized := make(map[string]Provider, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			return fmt.Errorf("provider name is empty")
		}
		if existing, ok := normalized[key]; ok {
			return fmt.Errorf("duplicate provider name after case fold: %q and %q both map to %q", existing.Name, name, key)
		}
		provider.Name = key
		// protocol 必须显式配置;空值留给 validate fail-fast,不按 provider 名推断。
		if provider.Protocol != "" {
			provider.Protocol = strings.ToLower(provider.Protocol)
		}
		provider.BaseURL = strings.TrimRight(provider.BaseURL, "/")
		if !provider.priorityConfigured {
			provider.Priority = DefaultProviderPriority
		}
		if !provider.fallbackConfigured {
			provider.Fallback = true
		}
		// models 严格区分大小写,与请求 body.model 原文匹配。
		provider.Models = normalizeModelPatterns(provider.Models)
		endpoints, err := normalizeProviderEndpoints(provider.Endpoints)
		if err != nil {
			return fmt.Errorf("provider %q endpoints: %w", key, err)
		}
		provider.Endpoints = endpoints
		normalized[key] = provider
	}
	cfg.Providers = normalized

	if cfg.ModelMetadata == nil {
		cfg.ModelMetadata = map[string]ModelMetadata{}
	}
	metadata := make(map[string]ModelMetadata, len(cfg.ModelMetadata))
	for name, info := range cfg.ModelMetadata {
		id := strings.TrimSpace(info.ID)
		if id == "" {
			id = strings.TrimSpace(name)
		}
		if id == "" {
			continue
		}
		if err := validateModelMetadataID(id); err != nil {
			return fmt.Errorf("model_metadata.%s: %w", id, err)
		}
		info.ID = id
		// model ID 严格区分大小写:DeepSeek-V4-Flash 与 deepseek-v4-flash 是两个不同模型。
		// 仅 exact id 唯一;不做 case-fold 去重。
		if prev, ok := metadata[id]; ok {
			return fmt.Errorf("duplicate model_metadata id: %q (also seen as %q)", id, prev.ID)
		}
		metadata[id] = info
	}
	cfg.ModelMetadata = metadata
	if err := normalizeAdminAuth(&cfg.AdminAuth); err != nil {
		return err
	}
	return nil
}

// validate 在启动期做完整校验,把配置错误尽早暴露。

func validateMetricsCIDRs(cidrs []string) error {
	for _, cidr := range cidrs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		if ip := net.ParseIP(cidr); ip != nil {
			continue
		}
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("metrics_allowed_cidrs: invalid entry %q", cidr)
		}
	}
	return nil
}

func validate(cfg Config) error {
	chatGPTProviderEnabled := EffectiveChatGPTWebProviderEnabled(cfg.ChatGPTWeb)
	codexProviderEnabled := EffectiveCodexOAuthProviderEnabled(cfg.CodexOAuth)
	if len(cfg.Providers) == 0 && !chatGPTProviderEnabled && !codexProviderEnabled {
		return fmt.Errorf("no providers configured; declare providers in config.yaml")
	}
	if !hasEnabledProvider(cfg.Providers) && !chatGPTProviderEnabled && !codexProviderEnabled {
		return fmt.Errorf("no enabled providers configured")
	}
	// client_api_keys 是归属机制而非强制登录;非 loopback 监听不再要求 inbound key。
	// 生产环境需由防火墙/反代等独立接入层保护(见闭包方案 §7.5)。
	if err := validateClientAPIKeys(cfg); err != nil {
		return err
	}
	if err := validateState(cfg.State); err != nil {
		return err
	}
	if err := validateChatGPTWeb(cfg.ChatGPTWeb); err != nil {
		return err
	}
	if err := validateCodexOAuth(cfg.CodexOAuth); err != nil {
		return err
	}
	if err := validateProviders(cfg); err != nil {
		return err
	}
	if err := validateModelMetadata(cfg); err != nil {
		return err
	}
	if err := validateSLO(cfg.SLO); err != nil {
		return err
	}
	if err := validateMetricsCIDRs(cfg.MetricsAllowedCIDRs); err != nil {
		return err
	}
	if err := validateAdminAuth(cfg.AdminAuth); err != nil {
		return err
	}
	return nil
}

// IsLoopbackListenAddr 判断监听地址是否绑定 loopback。
// 空 host(如 ":8080") 表示所有网卡,不算 loopback。
func IsLoopbackListenAddr(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		if strings.HasPrefix(addr, ":") {
			return false
		}
		ip := net.ParseIP(addr)
		return ip != nil && ip.IsLoopback()
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateHTTPBaseURL(raw string) error {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("unsupported scheme %q (want http or https)", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("missing host")
	}
	return nil
}

func validateProviders(cfg Config) error {
	for name, provider := range cfg.Providers {
		// chatgptweb is a reserved builtin provider when chatgpt_web.enabled is
		// true. Explicit YAML entries are a hard configuration error — there is
		// no compatibility bridge or silent migration.
		if strings.EqualFold(strings.TrimSpace(name), "chatgptweb") || strings.TrimSpace(provider.Protocol) == "chatgptweb" {
			return fmt.Errorf("provider %q: protocol chatgptweb is reserved for the builtin provider; remove providers.%s and enable chatgpt_web instead", name, name)
		}
		if provider.Disabled {
			continue
		}
		if provider.Priority < MinProviderPriority || provider.Priority > MaxProviderPriority {
			return fmt.Errorf("provider %q priority must be in [%d,%d]", name, MinProviderPriority, MaxProviderPriority)
		}
		if strings.TrimSpace(provider.Protocol) == "" {
			return fmt.Errorf("provider %q protocol is required (explicit; not inferred from name)", name)
		}
		switch provider.Protocol {
		case "openai", "anthropic":
		default:
			return fmt.Errorf("provider %q has unknown protocol %q (want openai or anthropic)", name, provider.Protocol)
		}
		if strings.TrimSpace(provider.BaseURL) == "" {
			return fmt.Errorf("provider %q base_url is required (explicit; not inferred from name)", name)
		}
		if validateHTTPBaseURL(provider.BaseURL) != nil {
			return fmt.Errorf("provider %q base_url is invalid", name)
		}
		if err := validateHTTPBaseURL(provider.BaseURL); err != nil {
			return fmt.Errorf("provider %q base_url: %w", name, err)
		}
		if err := validateProviderAPIKey(name, provider); err != nil {
			return err
		}
		if len(provider.Models) == 0 {
			return fmt.Errorf("provider %q models is required (explicit; not inferred from provider name or protocol)", name)
		}
		for _, pattern := range provider.Models {
			id := strings.TrimSpace(pattern)
			if id == "" || id == "*" || strings.HasSuffix(id, "*") {
				continue
			}
			if err := validateModelMetadataID(id); err != nil {
				return fmt.Errorf("providers.%s.models exact model %q: %w", name, id, err)
			}
		}
		if len(provider.Endpoints) == 0 {
			return fmt.Errorf("provider %q endpoints is required (explicit; not inferred from protocol)", name)
		}
		if err := validateProtocolEndpoints(provider.Protocol, provider.Endpoints); err != nil {
			return fmt.Errorf("provider %q: %w", name, err)
		}
	}
	return nil
}

// validateProviderAPIKey:远程上游必须有 API Key;仅 allow_unauthenticated + loopback base_url 允许空 Key。
func validateProviderAPIKey(name string, provider Provider) error {
	key := strings.TrimSpace(provider.APIKey)
	if provider.AllowUnauthenticated && key != "" {
		return fmt.Errorf("provider %q allow_unauthenticated requires empty api_key; authenticated and unauthenticated modes are mutually exclusive", name)
	}
	if key != "" {
		return nil
	}
	loopback, err := isLoopbackBaseURL(provider.BaseURL)
	if err != nil {
		return fmt.Errorf("provider %q base_url: %w", name, err)
	}
	if provider.AllowUnauthenticated {
		if !loopback {
			return fmt.Errorf("provider %q allow_unauthenticated requires loopback base_url; remote empty api_key is not allowed", name)
		}
		return nil
	}
	if loopback {
		return fmt.Errorf("provider %q has empty api_key; set api_key or allow_unauthenticated=true for trusted loopback upstream", name)
	}
	return fmt.Errorf("provider %q has empty api_key; remote providers require explicit credentials", name)
}

func isLoopbackBaseURL(raw string) (bool, error) {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil {
		return false, err
	}
	host := parsed.Hostname()
	if host == "" {
		return false, fmt.Errorf("missing host")
	}
	if host == "localhost" {
		return true, nil
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback(), nil
}

func validateSLO(slo SLOConfig) error {
	if slo.CacheHitRateMin < 0 || slo.CacheHitRateMin > 1 {
		return fmt.Errorf("slo_cache_hit_rate_min must be in [0,1], got %v", slo.CacheHitRateMin)
	}
	if slo.UpstreamErrorRateMax < 0 || slo.UpstreamErrorRateMax > 1 {
		return fmt.Errorf("slo_upstream_error_rate_max must be in [0,1], got %v", slo.UpstreamErrorRateMax)
	}
	if slo.P99LatencyMaxMS < 0 {
		return fmt.Errorf("slo_p99_latency_max_ms must be >= 0, got %v", slo.P99LatencyMaxMS)
	}
	if slo.CheckIntervalSeconds < 0 {
		return fmt.Errorf("slo_check_interval_seconds must be >= 0, got %d", slo.CheckIntervalSeconds)
	}
	if wh := strings.TrimSpace(slo.ViolationWebhook); wh != "" {
		if err := validateHTTPBaseURL(wh); err != nil {
			return fmt.Errorf("slo_violation_webhook: %w", err)
		}
	}
	return nil
}

func normalizeLogFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "text":
		return "text"
	default:
		return "json"
	}
}

func hasEnabledProvider(providers map[string]Provider) bool {
	for _, provider := range providers {
		if !provider.Disabled {
			return true
		}
	}
	return false
}

func stripComment(line string) string {
	inQuote := rune(0)
	for i, r := range line {
		switch r {
		case '\'', '"':
			if inQuote == 0 {
				inQuote = r
			} else if inQuote == r {
				inQuote = 0
			}
		case '#':
			if inQuote == 0 {
				return line[:i]
			}
		}
	}
	return line
}

func countIndent(line string) int {
	count := 0
	for _, r := range line {
		if r != ' ' {
			return count
		}
		count++
	}
	return count
}

func splitKV(line string) (string, string, bool) {
	idx := strings.IndexRune(line, ':')
	if idx < 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:idx])
	value := strings.TrimSpace(line[idx+1:])
	if value == "" {
		return key, "", false
	}
	return key, unquote(value), true
}

func unquote(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func expand(value string) string {
	return os.ExpandEnv(value)
}

// expandDollarBraceOnly 仅展开 ${NAME}，保留裸 $ 片段。
// Admin Argon2id PHC 哈希以 $ 分隔，不能走 os.ExpandEnv。
func expandDollarBraceOnly(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); {
		if value[i] == '$' && i+1 < len(value) && value[i+1] == '{' {
			end := strings.IndexByte(value[i+2:], '}')
			if end >= 0 {
				name := value[i+2 : i+2+end]
				if env, ok := os.LookupEnv(name); ok {
					b.WriteString(env)
				}
				i += 3 + end
				continue
			}
		}
		b.WriteByte(value[i])
		i++
	}
	return b.String()
}

// addrFromPort 将纯端口转为 loopback 地址,避免 AI_PROXY_PORT=8080 变成 :8080(全网卡)。
// 若传入已是 host:port 或 :port,则保留原语义(:port 仍表示全网卡，调用方访问控制由网络层负责)。
func addrFromPort(port string) string {
	port = strings.TrimSpace(port)
	if port == "" {
		return "127.0.0.1:8080"
	}
	// 已是 host:port 或 [ipv6]:port
	if strings.Contains(port, ":") && !strings.HasPrefix(port, ":") {
		return port
	}
	port = strings.TrimPrefix(port, ":")
	return "127.0.0.1:" + port
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func parseStrictBool(value string) (bool, error) {
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("invalid boolean %q", value)
	}
	return parsed, nil
}

func parseStrictPositiveInt(value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", value)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("expected positive integer, got %d", parsed)
	}
	return parsed, nil
}

func parseStrictNonNegativeInt(value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", value)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("expected non-negative integer, got %d", parsed)
	}
	return parsed, nil
}

func parseStrictInt(value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", value)
	}
	return parsed, nil
}

func parseStrictPositiveInt64(value string) (int64, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", value)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("expected positive integer, got %d", parsed)
	}
	return parsed, nil
}

func parseStrictFloat(value string) (float64, error) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", value)
	}
	return parsed, nil
}

// parseList 解析逗号分隔列表,并折叠为小写(用于 CIDR 等)。
func parseList(value string) []string {
	return parseCSVList(value, true)
}

// parseModelList 解析 models 列表,保留原文大小写。
func parseModelList(value string) []string {
	return parseCSVList(value, false)
}

func parseCSVList(value string, foldCase bool) []string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(unquote(part))
		if foldCase {
			item = strings.ToLower(item)
		}
		if item != "" {
			items = append(items, item)
		}
	}
	return items
}

// normalizeList 折叠为小写(CIDR 等)。
func normalizeList(values []string) []string {
	return normalizeCSVList(values, true)
}

// normalizeModelPatterns 保留 models 原文大小写,仅 trim。
func normalizeModelPatterns(values []string) []string {
	return normalizeCSVList(values, false)
}

func normalizeCSVList(values []string, foldCase bool) []string {
	if len(values) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if foldCase {
			value = strings.ToLower(value)
		}
		if value != "" {
			normalized = append(normalized, value)
		}
	}
	return normalized
}

func validateModelMetadataID(id string) error {
	if id == "" {
		return fmt.Errorf("id is empty")
	}
	if len(id) > MaxModelMetadataIDLength {
		return fmt.Errorf("id exceeds max length %d", MaxModelMetadataIDLength)
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("id contains control character")
		}
	}
	return nil
}

// validateModelMetadata validates optional enrichment only. Entries need not
// match a static provider because runtime-discovered models may consume them.
func validateModelMetadata(cfg Config) error {
	for id, info := range cfg.ModelMetadata {
		if info.ContextWindowTokens < 0 {
			return fmt.Errorf("model_metadata.%s: context_window_tokens must be zero or positive", id)
		}
		if info.MaxOutputTokens < 0 {
			return fmt.Errorf("model_metadata.%s: max_output_tokens must be zero or positive", id)
		}
		if info.ContextWindowTokens > 0 && info.MaxOutputTokens > 0 && info.MaxOutputTokens >= info.ContextWindowTokens {
			return fmt.Errorf("model_metadata.%s: max_output_tokens must be less than context_window_tokens", id)
		}
	}
	return nil
}

// EffectiveProviderPriority returns the compatibility default for an omitted
// priority. It is also safe for focused tests that construct Provider values
// without first running Load, where a zero value means "unspecified".
func EffectiveProviderPriority(provider Provider) int {
	if provider.Priority == 0 && !provider.priorityConfigured {
		return DefaultProviderPriority
	}
	return provider.Priority
}

// EffectiveProviderFallback returns the compatibility default for an omitted
// fallback key. Focused in-memory Config fixtures do not carry YAML presence
// metadata, so their zero-value Provider follows the production default too.
func EffectiveProviderFallback(provider Provider) bool {
	if !provider.fallbackConfigured {
		return true
	}
	return provider.Fallback
}

// EffectiveChatGPTWebProviderPriority returns the persisted builtin priority
// or its stable default for focused in-memory tests and legacy configs.
func EffectiveChatGPTWebProviderPriority(web ChatGPTWebConfig) int {
	if web.Priority == 0 && !web.priorityConfigured {
		return DefaultChatGPTWebProviderPriority
	}
	return web.Priority
}

// EffectiveChatGPTWebProviderEnabled returns the route-level enablement for
// the builtin provider. In-memory fixtures and legacy YAML without an
// explicit provider_enabled key preserve the lifecycle Enabled value.
func EffectiveChatGPTWebProviderEnabled(web ChatGPTWebConfig) bool {
	return web.Enabled && (!web.providerEnabledConfigured || web.ProviderEnabled)
}

// EffectiveCodexOAuthProviderPriority returns the persisted builtin priority
// or its stable default for focused in-memory tests and legacy configs.
func EffectiveCodexOAuthProviderPriority(codex CodexOAuthConfig) int {
	if codex.Priority == 0 && !codex.priorityConfigured {
		return DefaultCodexOAuthProviderPriority
	}
	return codex.Priority
}

// EffectiveCodexOAuthProviderEnabled is the Codex counterpart of
// EffectiveChatGPTWebProviderEnabled.
func EffectiveCodexOAuthProviderEnabled(codex CodexOAuthConfig) bool {
	return codex.Enabled && (!codex.providerEnabledConfigured || codex.ProviderEnabled)
}

// ProviderMatchesModel 判断 provider 的 models 模式是否匹配 model(区分大小写,仅 trim)。
func ProviderMatchesModel(_ string, provider Provider, model string) bool {
	for _, pattern := range provider.Models {
		if MatchModelPattern(model, pattern) {
			return true
		}
	}
	return false
}

// MatchModelPattern 精确或前缀通配(* 后缀);model 与 pattern 均区分大小写。
func MatchModelPattern(model, pattern string) bool {
	pattern = strings.TrimSpace(pattern)
	model = strings.TrimSpace(model)
	switch {
	case pattern == "":
		return false
	case pattern == "*":
		return true
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(model, strings.TrimSuffix(pattern, "*"))
	default:
		return model == pattern
	}
}

// Provider endpoint identifiers describe native upstream HTTP surfaces.
const (
	ProviderEndpointChatCompletions = "chat_completions"
	ProviderEndpointMessages        = "messages"
	ProviderEndpointResponses       = "responses"
	ProviderEndpointCompletions     = "completions"
	ProviderEndpointEmbeddings      = "embeddings"
	ProviderEndpointImages          = "images"
)

func parseProviderEndpoints(value string) ([]string, error) {
	raw := parseCSVList(value, true)
	return normalizeProviderEndpoints(raw)
}

func normalizeProviderEndpoints(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		endpoint := strings.ToLower(strings.TrimSpace(value))
		if endpoint == "" {
			continue
		}
		switch endpoint {
		case ProviderEndpointChatCompletions, ProviderEndpointMessages, ProviderEndpointResponses,
			ProviderEndpointCompletions, ProviderEndpointEmbeddings, ProviderEndpointImages:
		default:
			return nil, fmt.Errorf("unknown endpoint %q", value)
		}
		if _, ok := seen[endpoint]; ok {
			continue
		}
		seen[endpoint] = struct{}{}
		out = append(out, endpoint)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return providerEndpointRank(out[i]) < providerEndpointRank(out[j])
	})
	return out, nil
}

func providerEndpointRank(endpoint string) int {
	switch endpoint {
	case ProviderEndpointChatCompletions:
		return 0
	case ProviderEndpointMessages:
		return 1
	case ProviderEndpointResponses:
		return 2
	case ProviderEndpointCompletions:
		return 3
	case ProviderEndpointEmbeddings:
		return 4
	case ProviderEndpointImages:
		return 5
	default:
		return 100
	}
}

func validateProtocolEndpoints(protocol string, endpoints []string) error {
	for _, endpoint := range endpoints {
		switch protocol {
		case "openai":
			switch endpoint {
			case ProviderEndpointChatCompletions, ProviderEndpointResponses, ProviderEndpointCompletions, ProviderEndpointEmbeddings, ProviderEndpointImages:
			case ProviderEndpointMessages:
				return fmt.Errorf("endpoint messages is invalid for openai protocol (use chat_completions; conversion serves /v1/messages)")
			default:
				return fmt.Errorf("unknown endpoint %q", endpoint)
			}
		case "anthropic":
			switch endpoint {
			case ProviderEndpointMessages:
			case ProviderEndpointChatCompletions, ProviderEndpointResponses, ProviderEndpointCompletions, ProviderEndpointEmbeddings, ProviderEndpointImages:
				return fmt.Errorf("endpoint %q is invalid for anthropic protocol (use messages; conversion may serve /v1/chat/completions)", endpoint)
			default:
				return fmt.Errorf("unknown endpoint %q", endpoint)
			}
		case "chatgptweb":
			if endpoint != ProviderEndpointChatCompletions && endpoint != ProviderEndpointResponses && endpoint != ProviderEndpointImages {
				return fmt.Errorf("endpoint %q is invalid for chatgptweb protocol", endpoint)
			}
		}
	}
	return nil
}

// ProviderHasDirectEndpoint 判断 provider 是否显式声明了上游原生 endpoint。
// 只检查配置声明,不包含协议转换派生的客户端可服务 path。
func ProviderHasDirectEndpoint(provider Provider, endpoint string) bool {
	endpoint = strings.TrimSpace(endpoint)
	for _, item := range provider.Endpoints {
		if item == endpoint {
			return true
		}
	}
	return false
}

// ProviderTransport is the single protocol/endpoint routing contract shared by
// startup validation and request-time TransportPlan construction.
type ProviderTransport struct {
	UpstreamEndpoint string
	Mode             string
}

// ResolveProviderTransport combines inbound path, upstream protocol and the
// provider's explicitly declared native endpoints. Matrix entries not listed
// here are unsupported; callers must not infer endpoints from protocol alone.
func ResolveProviderTransport(provider Provider, path string) (ProviderTransport, bool) {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	switch path {
	case "/v1/chat/completions":
		if provider.Protocol == "chatgptweb" && ProviderHasDirectEndpoint(provider, ProviderEndpointChatCompletions) {
			return ProviderTransport{UpstreamEndpoint: "chatgptweb", Mode: "native"}, true
		}
		if provider.Protocol == "openai" && ProviderHasDirectEndpoint(provider, ProviderEndpointChatCompletions) {
			return ProviderTransport{UpstreamEndpoint: "/v1/chat/completions", Mode: "native"}, true
		}
		if provider.Protocol == "anthropic" && ProviderHasDirectEndpoint(provider, ProviderEndpointMessages) {
			return ProviderTransport{UpstreamEndpoint: "/v1/messages", Mode: "openai_to_anthropic"}, true
		}
	case "/v1/messages":
		if provider.Protocol == "anthropic" && ProviderHasDirectEndpoint(provider, ProviderEndpointMessages) {
			return ProviderTransport{UpstreamEndpoint: "/v1/messages", Mode: "native"}, true
		}
		if provider.Protocol == "openai" && ProviderHasDirectEndpoint(provider, ProviderEndpointChatCompletions) {
			return ProviderTransport{UpstreamEndpoint: "/v1/chat/completions", Mode: "anthropic_to_openai"}, true
		}
	case "/v1/responses":
		if provider.Protocol == "chatgptweb" && ProviderHasDirectEndpoint(provider, ProviderEndpointResponses) {
			return ProviderTransport{UpstreamEndpoint: "chatgptweb_responses", Mode: "chatgptweb_responses"}, true
		}
		if provider.Protocol == "openai" && ProviderHasDirectEndpoint(provider, ProviderEndpointResponses) {
			return ProviderTransport{UpstreamEndpoint: "/v1/responses", Mode: "native"}, true
		}
		if provider.Protocol == "codexoauth" && ProviderHasDirectEndpoint(provider, ProviderEndpointResponses) {
			return ProviderTransport{UpstreamEndpoint: "codex_oauth_responses", Mode: "codex_oauth_responses"}, true
		}
	case "/v1/search":
		if provider.Protocol == "chatgptweb" && ProviderHasDirectEndpoint(provider, ProviderEndpointChatCompletions) {
			return ProviderTransport{UpstreamEndpoint: "chatgptweb_search", Mode: "native"}, true
		}
	case "/v1/completions":
		if provider.Protocol == "openai" && ProviderHasDirectEndpoint(provider, ProviderEndpointCompletions) {
			return ProviderTransport{UpstreamEndpoint: "/v1/completions", Mode: "native"}, true
		}
	case "/v1/embeddings":
		if provider.Protocol == "openai" && ProviderHasDirectEndpoint(provider, ProviderEndpointEmbeddings) {
			return ProviderTransport{UpstreamEndpoint: "/v1/embeddings", Mode: "native"}, true
		}
	case "/v1/images/generations", "/v1/images/edits":
		if provider.Protocol == "chatgptweb" && ProviderHasDirectEndpoint(provider, ProviderEndpointImages) {
			return ProviderTransport{UpstreamEndpoint: "chatgptweb_images", Mode: "native"}, true
		}
		if provider.Protocol == "openai" && ProviderHasDirectEndpoint(provider, ProviderEndpointImages) {
			return ProviderTransport{UpstreamEndpoint: path, Mode: "native"}, true
		}
	}
	return ProviderTransport{}, false
}

func ProviderSupportsInboundPath(provider Provider, path string) bool {
	_, ok := ResolveProviderTransport(provider, path)
	return ok
}

// ServiceableInboundPaths 返回 provider 当前可服务的入站 path 列表(稳定排序)。
func ServiceableInboundPaths(provider Provider) []string {
	candidates := []string{
		"/v1/chat/completions",
		"/v1/messages",
		"/v1/responses",
		"/v1/completions",
		"/v1/embeddings",
		"/v1/images/generations",
		"/v1/images/edits",
	}
	out := make([]string, 0, len(candidates))
	for _, path := range candidates {
		if ProviderSupportsInboundPath(provider, path) {
			out = append(out, path)
		}
	}
	return out
}

func providersShareServiceablePath(a, b Provider) bool {
	for _, path := range ServiceableInboundPaths(a) {
		if ProviderSupportsInboundPath(b, path) {
			return true
		}
	}
	return false
}

// clientAPIKeyIDPattern: [a-z0-9][a-z0-9._-]{0,63}
var clientAPIKeyIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ReservedClientAPIKeyID 保留给历史 usage 记录，配置中禁止声明。
const ReservedClientAPIKeyID = "default"

const (
	defaultUsageStoreMemoryLimit       = "256MB"
	defaultUsageStoreThreads           = 2
	defaultUsageStoreQueryCacheSeconds = 15
	maxUsageStoreThreads               = 32
	maxUsageStoreQueryCacheSeconds     = 3600
	maxUsageStoreMemoryBytes           = 16 << 30 // 16GiB 上限
	minUsageStoreMemoryBytes           = 32 << 20 // 32MiB 下限
)

func normalizeState(state *StateConfig, configPath string) error {
	if state == nil {
		return fmt.Errorf("state is nil")
	}
	state.Dir = strings.TrimSpace(state.Dir)
	if state.Dir == "" {
		state.Dir = "var"
	}
	if strings.Contains(strings.ToLower(state.Dir), "://") {
		return fmt.Errorf("state.dir must be a local directory path")
	}
	if !filepath.IsAbs(state.Dir) && strings.TrimSpace(configPath) != "" {
		state.Dir = filepath.Join(filepath.Dir(configPath), state.Dir)
	}
	state.Dir = filepath.Clean(state.Dir)
	state.Database = strings.TrimSpace(state.Database)
	if state.Database == "" {
		state.Database = "state.duckdb"
	}
	if filepath.IsAbs(state.Database) || strings.Contains(state.Database, "://") || strings.Contains(filepath.Clean(state.Database), "..") {
		return fmt.Errorf("state.database must be a file beneath state.dir")
	}
	state.Database = filepath.Join(state.Dir, filepath.Clean(state.Database))
	if strings.TrimSpace(state.MemoryLimit) == "" {
		state.MemoryLimit = defaultUsageStoreMemoryLimit
	}
	state.MemoryLimit = strings.TrimSpace(state.MemoryLimit)
	if state.Threads <= 0 {
		state.Threads = defaultUsageStoreThreads
	}
	if state.QueryCacheSeconds < 0 {
		state.QueryCacheSeconds = defaultUsageStoreQueryCacheSeconds
	}
	if state.InteractionRetention <= 0 {
		state.InteractionRetention = 500
	}
	return nil
}

func normalizeClientAPIKeys(cfg *Config) error {
	if cfg.ClientAPIKeys == nil {
		cfg.ClientAPIKeys = map[string]ClientAPIKey{}
		return nil
	}
	normalized := make(map[string]ClientAPIKey, len(cfg.ClientAPIKeys))
	for rawID, entry := range cfg.ClientAPIKeys {
		id := strings.ToLower(strings.TrimSpace(entry.ID))
		if id == "" {
			id = strings.ToLower(strings.TrimSpace(rawID))
		}
		if id == "" {
			return fmt.Errorf("client_api_keys: empty id")
		}
		if id == ReservedClientAPIKeyID {
			return fmt.Errorf("client_api_keys: %q is a reserved id", ReservedClientAPIKeyID)
		}
		if !clientAPIKeyIDPattern.MatchString(id) {
			return fmt.Errorf("client_api_keys: invalid id %q (must match [a-z0-9][a-z0-9._-]{0,63})", id)
		}
		if existing, ok := normalized[id]; ok {
			return fmt.Errorf("client_api_keys: duplicate id after case fold: %q and %q", existing.ID, rawID)
		}
		entry.ID = id
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		entry.APIKeyHash = strings.ToLower(strings.TrimSpace(entry.APIKeyHash))
		normalized[id] = entry
	}
	cfg.ClientAPIKeys = normalized
	return nil
}

func validateClientAPIKeys(cfg Config) error {
	// 用摘要做唯一性检查;错误信息不得包含密钥明文。
	seenDigests := make(map[string]string, len(cfg.ClientAPIKeys))
	for id, entry := range cfg.ClientAPIKeys {
		if id == ReservedClientAPIKeyID {
			return fmt.Errorf("client_api_keys: %q is a reserved id", ReservedClientAPIKeyID)
		}
		if !clientAPIKeyIDPattern.MatchString(id) {
			return fmt.Errorf("client_api_keys: invalid id %q", id)
		}
		if entry.APIKey != "" && entry.APIKeyHash != "" {
			return fmt.Errorf("client_api_keys.%s: api_key and api_key_hash are mutually exclusive", id)
		}
		if entry.Enabled && entry.APIKey == "" && entry.APIKeyHash == "" {
			return fmt.Errorf("client_api_keys.%s: api_key or api_key_hash is required when enabled", id)
		}
		if entry.APIKey == "" && entry.APIKeyHash == "" {
			continue
		}
		digest := entry.APIKeyHash
		if entry.APIKey != "" {
			sum := sha256.Sum256([]byte(entry.APIKey))
			digest = "sha256:" + hex.EncodeToString(sum[:])
		} else if !isClientAPIKeyHash(digest) {
			return fmt.Errorf("client_api_keys.%s: invalid api_key_hash", id)
		}
		if prev, ok := seenDigests[digest]; ok {
			if entry.APIKey != "" && cfg.ClientAPIKeys[prev].APIKey != "" {
				return fmt.Errorf("client_api_keys: duplicate api_key shared by %q and %q", prev, id)
			}
			return fmt.Errorf("client_api_keys: duplicate credential shared by %q and %q", prev, id)
		}
		seenDigests[digest] = id
	}
	return nil
}

func isClientAPIKeyHash(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func validateState(state StateConfig) error {
	if _, err := parseMemoryLimitBytes(state.MemoryLimit); err != nil {
		return fmt.Errorf("state.memory_limit: %w", err)
	}
	if state.Threads < 1 {
		return fmt.Errorf("state.threads must be >= 1")
	}
	maxThreads := runtime.NumCPU()
	if maxThreads < 1 {
		maxThreads = 1
	}
	if maxThreads > maxUsageStoreThreads {
		maxThreads = maxUsageStoreThreads
	}
	if state.Threads > maxThreads {
		return fmt.Errorf("state.threads %d exceeds limit %d", state.Threads, maxThreads)
	}
	if state.QueryCacheSeconds < 0 || state.QueryCacheSeconds > maxUsageStoreQueryCacheSeconds {
		return fmt.Errorf("state.query_cache_seconds must be between 0 and %d", maxUsageStoreQueryCacheSeconds)
	}
	return nil
}

func normalizeTemporaryChat(tc *TemporaryChatConfig) {
	if tc == nil {
		return
	}
	unset := !tc.Enabled && tc.RetentionDays == 0 && tc.MaxConversations == 0 &&
		tc.MaxMessagesPerConversation == 0 && tc.MaxMessageBytes == 0 && tc.TurnTimeoutSeconds == 0
	if tc.RetentionDays == 0 {
		tc.RetentionDays = 30
	}
	if tc.MaxConversations == 0 {
		tc.MaxConversations = 2000
	}
	if tc.MaxMessagesPerConversation == 0 {
		tc.MaxMessagesPerConversation = 200
	}
	if tc.MaxMessageBytes == 0 {
		tc.MaxMessageBytes = 262144
	}
	if tc.TurnTimeoutSeconds == 0 {
		tc.TurnTimeoutSeconds = 300
	}
	// Design default is enabled:true when the nested block is omitted entirely.
	if unset {
		tc.Enabled = true
	}
}

func validateChatGPTWeb(web ChatGPTWebConfig) error {
	if web.Priority < MinProviderPriority || web.Priority > MaxProviderPriority {
		return fmt.Errorf("chatgpt_web.priority must be in [%d,%d]", MinProviderPriority, MaxProviderPriority)
	}
	if web.RefreshAccountIntervalMinute < 0 {
		return fmt.Errorf("chatgpt_web.refresh_account_interval_minute must be >= 0")
	}
	tc := web.TemporaryChat
	if tc.RetentionDays <= 0 {
		return fmt.Errorf("chatgpt_web.temporary_chat.retention_days must be > 0")
	}
	if tc.MaxConversations <= 0 {
		return fmt.Errorf("chatgpt_web.temporary_chat.max_conversations must be > 0")
	}
	if tc.MaxMessagesPerConversation <= 0 {
		return fmt.Errorf("chatgpt_web.temporary_chat.max_messages_per_conversation must be > 0")
	}
	if tc.MaxMessageBytes <= 0 {
		return fmt.Errorf("chatgpt_web.temporary_chat.max_message_bytes must be > 0")
	}
	if tc.TurnTimeoutSeconds <= 0 {
		return fmt.Errorf("chatgpt_web.temporary_chat.turn_timeout_seconds must be > 0")
	}
	return nil
}

func validateCodexOAuth(codex CodexOAuthConfig) error {
	if codex.Priority < MinProviderPriority || codex.Priority > MaxProviderPriority {
		return fmt.Errorf("codex_oauth.priority must be in [%d,%d]", MinProviderPriority, MaxProviderPriority)
	}
	if codex.RefreshAccountIntervalMinute < 0 {
		return fmt.Errorf("codex_oauth.refresh_account_interval_minute must be >= 0")
	}
	return nil
}

// parseMemoryLimitBytes 解析形如 256MB / 1GB / 512MiB 的内存上限,并施加上下限。
// 只接受受控后缀,禁止透传任意 SQL 字符串。
func parseMemoryLimitBytes(raw string) (int64, error) {
	s := strings.TrimSpace(strings.ToUpper(raw))
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	type unit struct {
		suffix string
		mult   int64
	}
	units := []unit{
		{"GIB", 1 << 30},
		{"GB", 1000 * 1000 * 1000},
		{"MIB", 1 << 20},
		{"MB", 1000 * 1000},
		{"KIB", 1 << 10},
		{"KB", 1000},
		{"B", 1},
	}
	var mult int64 = 1
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			s = strings.TrimSpace(s[:len(s)-len(u.suffix)])
			mult = u.mult
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid memory_limit %q", raw)
	}
	bytes := n * mult
	if bytes < minUsageStoreMemoryBytes {
		return 0, fmt.Errorf("memory_limit too small (min 32MB)")
	}
	if bytes > maxUsageStoreMemoryBytes {
		return 0, fmt.Errorf("memory_limit too large (max 16GB)")
	}
	return bytes, nil
}

// ClientAPIKeyEntries 返回稳定排序的客户端 Key 条目,供 clientauth 索引构建。
func ClientAPIKeyEntries(cfg Config) []ClientAPIKey {
	if len(cfg.ClientAPIKeys) == 0 {
		return nil
	}
	ids := make([]string, 0, len(cfg.ClientAPIKeys))
	for id := range cfg.ClientAPIKeys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ClientAPIKey, 0, len(ids))
	for _, id := range ids {
		out = append(out, cfg.ClientAPIKeys[id])
	}
	return out
}

// UsageStoreMemoryLimitSQL 返回可安全用于 SET memory_limit 的字面量(已校验)。
func UsageStoreMemoryLimitSQL(us UsageStoreConfig) (string, error) {
	bytes, err := parseMemoryLimitBytes(us.MemoryLimit)
	if err != nil {
		return "", err
	}
	// DuckDB 接受 '256MB' 形式;统一用 MB 整数避免浮点。
	mb := bytes / (1000 * 1000)
	if mb < 1 {
		mb = 1
	}
	return fmt.Sprintf("%dMB", mb), nil
}
