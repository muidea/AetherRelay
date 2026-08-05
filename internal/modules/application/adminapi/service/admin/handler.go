package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ai-proxy/internal/modules/application/adminapi/pkg/codexmanagement"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	taskevents "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
	tempevents "ai-proxy/internal/modules/application/chatgpttemporarychat/pkg/events"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	imgevents "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/events"
	codexevents "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/events"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetricsport"
	"ai-proxy/internal/pkg/aiproxyusage"
	"ai-proxy/internal/services/probe"
	adminweb "ai-proxy/web"

	"go.yaml.in/yaml/v4"
)

const (
	maxRequestBodyBytes   = 1 << 20
	maxAccountImportItems = 1000
)

// RuntimeConfig 由代理处理器实现，用于读取和原子切换当前运行配置。
type RuntimeConfig interface {
	ConfigSnapshot() config.Config
	UpdateConfig(config.Config) error
}

type managedProviderRuntime interface {
	ReplaceProviders(map[string]config.Provider) error
	ProviderStorageAvailable() bool
}

type ChatGPTRuntime interface {
	ListChatGPTAccounts(context.Context) ([]accevents.AccountView, error)
	AddChatGPTAccounts(context.Context, []string, []accevents.ExportItem, string) (accevents.AddResult, error)
	DeleteChatGPTAccounts(context.Context, []string) (accevents.DeleteResult, error)
	UpdateChatGPTAccount(context.Context, accevents.UpdateByIDCommand) (accevents.UpdateResult, error)
	ExportChatGPTAccounts(context.Context, []string) (accevents.ExportResult, error)
	RefreshChatGPTAccountsByID(context.Context, []string) (accevents.RefreshResult, error)
	ChatGPTAccountRefreshProgress(context.Context, string) (accevents.RefreshProgress, error)
	StartChatGPTOAuth(context.Context, string) (accevents.OAuthStartResult, error)
	FinishChatGPTOAuth(context.Context, string, string) (accevents.OAuthFinishResult, error)
	ListChatGPTImages(context.Context, string, string, string) (imgevents.ListResult, error)
	ChatGPTImageStorage(context.Context) (imgevents.StorageStatsResult, error)
	ListChatGPTImageTags(context.Context) (imgevents.ListTagsResult, error)
	SetChatGPTImageTags(context.Context, string, []string) (imgevents.SetTagsResult, error)
	DeleteChatGPTImages(context.Context, []string) (imgevents.DeleteResult, error)
	GetChatGPTImageBytes(context.Context, string) ([]byte, error)
	GetChatGPTImageThumbnail(context.Context, string) ([]byte, error)
	SubmitChatGPTImageGeneration(context.Context, taskevents.SubmitGenerationCommand) (taskevents.SubmitResult, error)
	SubmitChatGPTImageEdit(context.Context, taskevents.SubmitEditCommand) (taskevents.SubmitResult, error)
	ListChatGPTImageTasks(context.Context, string, []string) (taskevents.ListResult, error)
	ResumeChatGPTImageTask(context.Context, string, string, int) (taskevents.ResumePollResult, error)
	RetryChatGPTImageGeneration(context.Context, string, string, string) (taskevents.RetryGenerationResult, error)
	ChatGPTEffectiveCatalog(context.Context) (effectivecatalog.Snapshot, error)
	CreateTemporaryConversation(context.Context, tempevents.CreateConversationCommand) (tempevents.ConversationResult, error)
	ListTemporaryConversations(context.Context, tempevents.ListConversationsCommand) (tempevents.ListConversationsResult, error)
	GetTemporaryConversation(context.Context, tempevents.GetConversationCommand) (tempevents.ConversationDetailResult, error)
	GetTemporaryMessageImage(context.Context, tempevents.GetMessageImageCommand) (tempevents.GetMessageImageResult, error)
	GetTemporaryMessageAttachment(context.Context, tempevents.GetMessageAttachmentCommand) (tempevents.GetMessageAttachmentResult, error)
	StartTemporaryTurn(context.Context, tempevents.StartTurnCommand) (tempevents.StartTurnResult, error)
	PullTemporaryTurn(context.Context, tempevents.PullTurnCommand) (tempevents.PullTurnResult, error)
	CancelTemporaryTurn(context.Context, tempevents.CancelTurnCommand) (tempevents.CancelTurnResult, error)
	DeleteTemporaryConversation(context.Context, tempevents.DeleteConversationCommand) (tempevents.DeleteConversationResult, error)
}

// CodexRuntime is intentionally independent from ChatGPTRuntime: the two
// OAuth domains have different credential, proxy, and session semantics.
type CodexRuntime interface {
	ListCodexAccounts(context.Context) ([]codexevents.AccountView, error)
	ImportCodexAccounts(context.Context, []codexevents.CredentialInput) (codexmanagement.ImportResult, error)
	DeleteCodexAccounts(context.Context, []string) (codexevents.DeleteResult, error)
	UpdateCodexAccount(context.Context, codexevents.UpdateCommand) (codexevents.UpdateResult, error)
	RefreshCodexAccounts(context.Context, []string) (codexmanagement.RefreshResult, error)
	ExportCodexAccounts(context.Context, []string) (codexevents.ExportByIDResult, error)
	StartCodexOAuth(context.Context, string, string) (codexevents.OAuthStartResult, error)
	FinishCodexOAuth(context.Context, string, string) (codexmanagement.OAuthFinishResult, error)
	StartCodexModelDiscovery(context.Context, []string) (proxyevents.CodexDiscoveryProgress, error)
	CodexModelDiscoveryProgress(context.Context, string) (proxyevents.CodexDiscoveryProgress, error)
	StartCodexUsageRefresh(context.Context, []string) (proxyevents.CodexUsageProgress, error)
	CodexUsageRefreshProgress(context.Context, string) (proxyevents.CodexUsageProgress, error)
}

type featureCatalogRuntime interface {
	FeatureCatalog(context.Context) (proxyevents.FeatureCatalogResult, error)
}

type featureSearchRuntime interface {
	ExecuteFeatureSearch(context.Context, proxyevents.ExecuteFeatureSearchCommand) (proxyevents.ExecuteFeatureSearchResult, error)
}

type featureSearchHistoryRuntime interface {
	ListFeatureSearchHistory(context.Context, proxyevents.ListFeatureSearchHistoryCommand) (proxyevents.ListFeatureSearchHistoryResult, error)
	GetFeatureSearchHistory(context.Context, proxyevents.GetFeatureSearchHistoryCommand) (proxyevents.GetFeatureSearchHistoryResult, error)
}

type systemMetadataRuntime interface {
	SystemVersion() string
	SystemStartedAt() time.Time
}

type Handler struct {
	configPath      string
	runtime         RuntimeConfig
	usageStore      usage.Store
	metricsRegistry metricsport.Port
	auth            *authState
	chatGPT         ChatGPTRuntime
	codex           CodexRuntime
	startedAt       time.Time
	updateMu        sync.Mutex
}

type providerView struct {
	Name                 string               `json:"name"`
	Protocol             string               `json:"protocol"`
	BaseURL              string               `json:"base_url"`
	Models               []string             `json:"models"`
	Endpoints            []string             `json:"endpoints"`
	AllowUnauthenticated bool                 `json:"allow_unauthenticated"`
	Priority             int                  `json:"priority"`
	Fallback             bool                 `json:"fallback"`
	Enabled              bool                 `json:"enabled"`
	APIKeyConfigured     bool                 `json:"api_key_configured"`
	Availability         providerAvailability `json:"availability"`
	// Source is a display-only classification derived from builtin + base_url
	// (builtin / official / third_party). It is never read from or written to YAML.
	Source string `json:"source"`
	// Builtin marks an account-pool provider that is not represented in the
	// static providers mapping. Admin permits only its route enablement and
	// priority policy; protocol, credentials, catalog discovery, and deletion
	// stay managed by the corresponding account pool.
	Builtin           bool     `json:"builtin,omitempty"`
	ModelCount        int      `json:"model_count,omitempty"`
	AvailableAccounts int      `json:"available_accounts,omitempty"`
	ConflictCount     int      `json:"conflict_count,omitempty"`
	ConflictModels    []string `json:"conflict_models,omitempty"`
	UpdatedAt         string   `json:"updated_at,omitempty"`
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
}

type providerAvailability struct {
	Status              string  `json:"status"`
	Successes           int64   `json:"successes"`
	Failures            int64   `json:"failures"`
	ConsecutiveFailures int64   `json:"consecutive_failures"`
	LastSuccessAt       string  `json:"last_success_at,omitempty"`
	LastFailureAt       string  `json:"last_failure_at,omitempty"`
	LastStatus          int     `json:"last_status,omitempty"`
	LastOutcome         string  `json:"last_outcome,omitempty"`
	Score               int     `json:"score"`
	WindowSeconds       int     `json:"window_seconds"`
	SampleCount         int64   `json:"sample_count"`
	SuccessRate         float64 `json:"success_rate"`
	P50MS               float64 `json:"p50_ms"`
	P95MS               float64 `json:"p95_ms"`
	CircuitState        string  `json:"circuit_state,omitempty"`
	CircuitRetryAt      string  `json:"circuit_retry_at,omitempty"`
}

type providerInput struct {
	Name                 string   `json:"name"`
	Protocol             string   `json:"protocol"`
	BaseURL              string   `json:"base_url"`
	APIKey               string   `json:"api_key"`
	ClearAPIKey          bool     `json:"clear_api_key"`
	Models               []string `json:"models"`
	Endpoints            []string `json:"endpoints"`
	AllowUnauthenticated bool     `json:"allow_unauthenticated"`
	Priority             *int     `json:"priority"`
	Fallback             *bool    `json:"fallback"`
	Enabled              bool     `json:"enabled"`
}

type updateRequest struct {
	Providers []providerInput `json:"providers"`
}

func NewHandler(configPath string, runtime RuntimeConfig) *Handler {
	h := &Handler{configPath: configPath, runtime: runtime, startedAt: time.Now().UTC()}
	if runtime != nil {
		h.auth = newAuthState(runtime.ConfigSnapshot().AdminAuth)
	} else {
		h.auth = newAuthState(config.AdminAuthConfig{BasePath: config.DefaultAdminBasePath, SessionTTLSeconds: config.DefaultAdminSessionTTLSeconds})
	}
	return h
}

func (h *Handler) WithChatGPTRuntime(runtime ChatGPTRuntime) *Handler { h.chatGPT = runtime; return h }
func (h *Handler) WithCodexRuntime(runtime CodexRuntime) *Handler     { h.codex = runtime; return h }

// WithMetrics 挂接 usage 查询的健康与错误观测；nil-safe，便于单测复用。
func (h *Handler) WithMetrics(source any) *Handler {
	h.metricsRegistry = metricsport.AsPort(source)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	base := h.adminBasePath()
	path := r.URL.Path
	if path != base && !strings.HasPrefix(path, base+"/") {
		http.NotFound(w, r)
		return
	}
	rel := "/"
	if path != base {
		rel = strings.TrimPrefix(path, base)
		if rel == "" {
			rel = "/"
		}
	}

	authOn := h.authEnabled()
	if !authOn {
		if !isLoopbackRequest(r) {
			http.Error(w, "admin access is loopback-only", http.StatusForbidden)
			return
		}
	}

	// 认证端点与登录页(开启认证时无需会话)。
	switch {
	case rel == "/login" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		h.serveLoginPage(w, r)
		return
	case rel == "/api/auth/login" && r.Method == http.MethodPost:
		h.handleLogin(w, r)
		return
	case rel == "/api/auth/session" && r.Method == http.MethodGet:
		h.handleSession(w, r)
		return
	case rel == "/api/auth/logout" && r.Method == http.MethodPost:
		h.handleLogout(w, r)
		return
	}

	// 认证开启时,其余路径需要会话。
	if authOn {
		isAPI := strings.HasPrefix(rel, "/api/")
		isPage := rel == "/" || rel == ""
		if isPage && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			if h.sessionFromRequest(r) == nil {
				if r.Method == http.MethodHead {
					writeAdminAuthError(w, http.StatusUnauthorized, "admin_authentication_required", "admin login is required")
					return
				}
				http.Redirect(w, r, base+"/login", http.StatusSeeOther)
				return
			}
		} else if h.sessionFromRequest(r) == nil {
			if isAPI || strings.HasPrefix(rel, "/api") {
				writeAdminAuthError(w, http.StatusUnauthorized, "admin_authentication_required", "admin login is required")
				return
			}
			http.Redirect(w, r, base+"/login", http.StatusSeeOther)
			return
		}
	}

	switch {
	case rel == "/api/system/info" && r.Method == http.MethodGet:
		h.systemInfo(w)
	case rel == "/api/features/models" && r.Method == http.MethodGet:
		h.featureCatalog(w, r)
	case rel == "/api/features/search/history" && r.Method == http.MethodGet:
		h.listFeatureSearchHistory(w, r)
	case strings.HasPrefix(rel, "/api/features/search/history/") && r.Method == http.MethodGet:
		h.getFeatureSearchHistory(w, r, rel)
	case rel == "/api/features/search" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.executeFeatureSearch(w, r)
		}
	case (rel == "/" || rel == "") && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		h.serveIndex(w, r)
	case rel == "/api/providers" && r.Method == http.MethodGet:
		h.listProviders(w)
	case rel == "/api/providers" && r.Method == http.MethodPut:
		if !h.requireAdminMutation(w, r) {
			return
		}
		h.updateProviders(w, r)
	case strings.HasPrefix(rel, "/api/builtin-providers/") && r.Method == http.MethodPatch:
		if !h.requireAdminMutation(w, r) {
			return
		}
		h.updateBuiltinProvider(w, r, rel)
	case rel == "/api/admin/preferences" && r.Method == http.MethodGet:
		h.getAdminPreferences(w)
	case rel == "/api/admin/preferences" && r.Method == http.MethodPut:
		if !h.requireAdminMutation(w, r) {
			return
		}
		h.updateAdminPreferences(w, r)
	case strings.HasPrefix(rel, "/api/providers/") && strings.HasSuffix(rel, "/probe") && r.Method == http.MethodPost:
		if !h.requireAdminMutation(w, r) {
			return
		}
		h.probeProvider(w, r, rel)
	case rel == "/api/client-api-keys" && r.Method == http.MethodGet:
		h.listClientAPIKeys(w)
	case rel == "/api/client-api-keys" && r.Method == http.MethodPost:
		h.createClientAPIKey(w, r)
	case strings.HasPrefix(rel, "/api/client-api-keys/"):
		h.clientAPIKeyAction(w, r, rel)
	case strings.HasPrefix(rel, "/api/usage/"):
		h.usageAPI(w, r, rel)
	case rel == "/api/codex/accounts" && r.Method == http.MethodGet:
		h.listCodexAccounts(w, r)
	case rel == "/api/codex/accounts" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.importCodexAccounts(w, r)
		}
	case rel == "/api/codex/accounts" && r.Method == http.MethodDelete:
		if h.requireAdminMutation(w, r) {
			h.deleteCodexAccounts(w, r)
		}
	case strings.HasPrefix(rel, "/api/codex/accounts/") && r.Method == http.MethodPatch:
		if h.requireAdminMutation(w, r) {
			h.updateCodexAccount(w, r, rel)
		}
	case rel == "/api/codex/accounts/refresh" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.refreshCodexAccounts(w, r)
		}
	case rel == "/api/codex/accounts/export" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.exportCodexAccounts(w, r)
		}
	case rel == "/api/codex/accounts/discovery" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.startCodexModelDiscovery(w, r)
		}
	case strings.HasPrefix(rel, "/api/codex/accounts/discovery/progress/") && r.Method == http.MethodGet:
		h.codexModelDiscoveryProgress(w, r, rel)
	case rel == "/api/codex/accounts/usage" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.startCodexUsageRefresh(w, r)
		}
	case strings.HasPrefix(rel, "/api/codex/accounts/usage/progress/") && r.Method == http.MethodGet:
		h.codexUsageRefreshProgress(w, r, rel)
	case rel == "/api/codex/accounts/oauth/start" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.startCodexOAuth(w, r)
		}
	case rel == "/api/codex/accounts/oauth/finish" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.finishCodexOAuth(w, r)
		}
	case rel == "/api/chatgpt/accounts" && r.Method == http.MethodGet:
		h.listChatGPTAccounts(w, r)
	case rel == "/api/chatgpt/accounts" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.addChatGPTAccounts(w, r)
		}
	case rel == "/api/chatgpt/accounts" && r.Method == http.MethodDelete:
		if h.requireAdminMutation(w, r) {
			h.deleteChatGPTAccounts(w, r)
		}
	case strings.HasPrefix(rel, "/api/chatgpt/accounts/") && r.Method == http.MethodPatch:
		if h.requireAdminMutation(w, r) {
			h.updateChatGPTAccount(w, r, rel)
		}
	case rel == "/api/chatgpt/accounts/export" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.exportChatGPTAccounts(w, r)
		}
	case rel == "/api/chatgpt/accounts/refresh" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.refreshChatGPTAccounts(w, r)
		}
	case strings.HasPrefix(rel, "/api/chatgpt/accounts/refresh/progress/") && r.Method == http.MethodGet:
		h.chatGPTAccountRefreshProgress(w, r, rel)
	case rel == "/api/chatgpt/accounts/oauth/start" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.startChatGPTOAuth(w, r)
		}
	case rel == "/api/chatgpt/accounts/oauth/finish" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.finishChatGPTOAuth(w, r)
		}
	case rel == "/api/chatgpt/images" && r.Method == http.MethodGet:
		h.listChatGPTImages(w, r)
	case rel == "/api/chatgpt/images/content" && r.Method == http.MethodGet:
		h.serveChatGPTImageContent(w, r)
	case rel == "/api/chatgpt/images/storage" && r.Method == http.MethodGet:
		h.chatGPTImageStorage(w, r)
	case rel == "/api/chatgpt/images/tags" && r.Method == http.MethodGet:
		h.listChatGPTImageTags(w, r)
	case rel == "/api/chatgpt/images/tags" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.setChatGPTImageTags(w, r)
		}
	case rel == "/api/chatgpt/images/delete" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.deleteChatGPTImages(w, r)
		}
	case rel == "/api/chatgpt/image-tasks" && r.Method == http.MethodGet:
		h.listChatGPTImageTasks(w, r)
	case rel == "/api/chatgpt/image-tasks/generations" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.submitChatGPTImageGeneration(w, r)
		}
	case rel == "/api/chatgpt/image-tasks/edits" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.submitChatGPTImageEdit(w, r)
		}
	case strings.HasPrefix(rel, "/api/chatgpt/image-tasks/") && strings.HasSuffix(rel, "/resume-poll") && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.resumeChatGPTImageTask(w, r, rel)
		}
	case strings.HasPrefix(rel, "/api/chatgpt/image-tasks/") && strings.HasSuffix(rel, "/retry-generation") && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.retryChatGPTImageGeneration(w, r, rel)
		}
	case rel == "/api/chatgpt/temporary-conversations" && r.Method == http.MethodGet:
		h.listTemporaryConversations(w, r)
	case rel == "/api/chatgpt/temporary-conversations" && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.createTemporaryConversation(w, r)
		}
	case strings.HasPrefix(rel, "/api/chatgpt/temporary-conversations/") && strings.HasSuffix(rel, "/cancel") && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.cancelTemporaryTurn(w, r, rel)
		}
	case strings.HasPrefix(rel, "/api/chatgpt/temporary-conversations/") && strings.Contains(rel, "/turns/") && strings.HasSuffix(rel, "/events") && r.Method == http.MethodGet:
		h.pullTemporaryTurn(w, r, rel)
	case strings.HasPrefix(rel, "/api/chatgpt/temporary-conversations/") && strings.HasSuffix(rel, "/turns") && r.Method == http.MethodPost:
		if h.requireAdminMutation(w, r) {
			h.startTemporaryTurn(w, r, rel)
		}
	case strings.HasPrefix(rel, "/api/chatgpt/temporary-conversations/") && strings.Contains(rel, "/messages/") && strings.Contains(rel, "/images/") && r.Method == http.MethodGet:
		h.getTemporaryMessageImage(w, r, rel)
	case strings.HasPrefix(rel, "/api/chatgpt/temporary-conversations/") && strings.Contains(rel, "/messages/") && strings.Contains(rel, "/attachments/") && r.Method == http.MethodGet:
		h.getTemporaryMessageAttachment(w, r, rel)
	case strings.HasPrefix(rel, "/api/chatgpt/temporary-conversations/") && r.Method == http.MethodGet:
		h.getTemporaryConversation(w, r, rel)
	case strings.HasPrefix(rel, "/api/chatgpt/temporary-conversations/") && r.Method == http.MethodDelete:
		if h.requireAdminMutation(w, r) {
			h.deleteTemporaryConversation(w, r, rel)
		}
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) featureCatalog(w http.ResponseWriter, r *http.Request) {
	runtime, ok := h.chatGPT.(featureCatalogRuntime)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "feature catalog is unavailable")
		return
	}
	result, err := runtime.FeatureCatalog(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) listChatGPTImages(w http.ResponseWriter, r *http.Request) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "chatgpt image store is unavailable")
		return
	}
	// BaseURL is unused for public serving; rewrite below to Admin-authenticated content URLs.
	out, err := h.chatGPT.ListChatGPTImages(r.Context(), "", strings.TrimSpace(r.URL.Query().Get("start_date")), strings.TrimSpace(r.URL.Query().Get("end_date")))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	rewriteChatGPTImageURLs(h.adminBasePath(), &out)
	writeJSON(w, http.StatusOK, out)
}

// serveChatGPTImageContent is a minimal Admin-authenticated, path-validated image
// reader. It does not expose a general /files/** surface.
func (h *Handler) serveChatGPTImageContent(w http.ResponseWriter, r *http.Request) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "chatgpt image store is unavailable")
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	// Reject traversal and absolute paths before store; store also applies safeRel.
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || filepath.IsAbs(path) || strings.Contains(clean, "\\") {
		writeError(w, http.StatusBadRequest, "invalid image path")
		return
	}
	thumb := r.URL.Query().Get("thumb") == "1" || strings.EqualFold(r.URL.Query().Get("variant"), "thumbnail")
	var (
		payload []byte
		err     error
	)
	if thumb {
		payload, err = h.chatGPT.GetChatGPTImageThumbnail(r.Context(), clean)
	} else {
		payload, err = h.chatGPT.GetChatGPTImageBytes(r.Context(), clean)
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	if len(payload) == 0 {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	contentType := http.DetectContentType(payload)
	if thumb {
		contentType = "image/png"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func rewriteChatGPTImageURLs(adminBase string, out *imgevents.ListResult) {
	if out == nil {
		return
	}
	adminBase = strings.TrimRight(adminBase, "/")
	if adminBase == "" {
		adminBase = "/admin"
	}
	for i := range out.Items {
		path := strings.TrimSpace(out.Items[i].Path)
		if path == "" {
			continue
		}
		out.Items[i].URL = adminBase + "/api/chatgpt/images/content?path=" + url.QueryEscape(path)
		out.Items[i].ThumbnailURL = adminBase + "/api/chatgpt/images/content?path=" + url.QueryEscape(path) + "&thumb=1"
	}
}
func (h *Handler) chatGPTImageStorage(w http.ResponseWriter, r *http.Request) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "chatgpt image store is unavailable")
		return
	}
	out, err := h.chatGPT.ChatGPTImageStorage(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (h *Handler) listChatGPTImageTags(w http.ResponseWriter, r *http.Request) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "chatgpt image store is unavailable")
		return
	}
	out, err := h.chatGPT.ListChatGPTImageTags(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (h *Handler) setChatGPTImageTags(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string   `json:"path"`
		Tags []string `json:"tags"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(&body) != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	out, err := h.chatGPT.SetChatGPTImageTags(r.Context(), body.Path, body.Tags)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (h *Handler) deleteChatGPTImages(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths []string `json:"paths"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(&body) != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	out, err := h.chatGPT.DeleteChatGPTImages(r.Context(), body.Paths)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func adminImageBaseURL(r *http.Request) string {
	if r == nil || r.Host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (h *Handler) listChatGPTAccounts(w http.ResponseWriter, r *http.Request) {
	if h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "chatgpt account pool is unavailable")
		return
	}
	items, err := h.chatGPT.ListChatGPTAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	for index := range items {
		items[index].AccessToken = redactAccountToken(items[index].AccessToken)
		items[index].Proxy = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func redactAccountToken(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return ""
	}
	return value[:4] + "..." + value[len(value)-4:]
}

func (h *Handler) probeProvider(w http.ResponseWriter, r *http.Request, rel string) {
	name := strings.TrimSuffix(strings.TrimPrefix(rel, "/api/providers/"), "/probe")
	name = strings.ToLower(strings.Trim(strings.TrimSpace(name), "/"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "provider name is required")
		return
	}
	if isBuiltinProviderID(name) {
		h.probeBuiltinProvider(w, name)
		return
	}
	result, err := probe.Check(r.Context(), h.runtime.ConfigSnapshot(), name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.metricsRegistry != nil {
		h.metricsRegistry.RecordRequestPlan(name, result.Model, result.Endpoint, result.Status, time.Duration(result.DurationMS)*time.Millisecond, mapProbeOutcome(result.Conclusion), "", result.Protocol, result.UpstreamPath, "probe")
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": name, "status": result.Status, "duration_ms": result.DurationMS, "conclusion": result.Conclusion, "summary": result.Summary})
}

// probeBuiltinProvider intentionally reads the account-pool catalog projection
// instead of sending an artificial model request. An artificial request could
// consume quota, start a ChatGPT Web conversation, or contaminate the rolling
// health window with an operator action. The response shares the static probe
// shape so the Provider page can use one consistent Check action.
func (h *Handler) probeBuiltinProvider(w http.ResponseWriter, id string) {
	cfg := h.runtime.ConfigSnapshot()
	view := h.builtinChatGPTProviderView(cfg)
	if id == effectivecatalog.CodexOAuthProviderID {
		view = h.builtinCodexProviderView(cfg)
	}
	conclusion, status, summary := builtinProviderProbeResult(view)
	writeJSON(w, http.StatusOK, map[string]any{
		"provider":    id,
		"status":      status,
		"duration_ms": 0,
		"conclusion":  conclusion,
		"summary":     summary,
	})
}

func isBuiltinProviderID(id string) bool {
	return id == effectivecatalog.BuiltinProviderID || id == effectivecatalog.CodexOAuthProviderID
}

func builtinProviderProbeResult(view providerView) (conclusion string, status int, summary string) {
	availability := strings.TrimSpace(view.Availability.Status)
	switch availability {
	case "healthy":
		conclusion, status = "success", http.StatusOK
	case "degraded":
		conclusion, status = "degraded", http.StatusPartialContent
	case "disabled":
		conclusion, status = "disabled", http.StatusServiceUnavailable
	case "unavailable":
		conclusion, status = "unavailable", http.StatusServiceUnavailable
	case "credential_error", "unhealthy":
		conclusion, status = "unhealthy", http.StatusBadGateway
	case "endpoint_drift":
		conclusion, status = "endpoint_drift", http.StatusConflict
	default:
		conclusion, status = "unknown", http.StatusAccepted
	}

	parts := []string{fmt.Sprintf("catalog projection: %s", availability)}
	if reason := strings.TrimSpace(view.UnavailableReason); reason != "" {
		parts = append(parts, reason)
	} else {
		parts = append(parts, fmt.Sprintf("%d routable account(s), %d discovered model(s)", view.AvailableAccounts, view.ModelCount))
	}
	if view.UpdatedAt != "" {
		parts = append(parts, "last discovery "+view.UpdatedAt)
	}
	return conclusion, status, strings.Join(parts, "; ")
}

func mapProbeOutcome(conclusion string) string {
	if conclusion == "success" {
		return "success"
	}
	if conclusion == "endpoint_drift" {
		return "endpoint_drift"
	}
	return "upstream_failed"
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(injectAdminBasePath(adminweb.AdminIndexHTML, h.adminBasePath()))
}

// injectAdminBasePath 在 HTML 开头注入安全的 basePath JSON 字面量。
func injectAdminBasePath(html []byte, basePath string) []byte {
	bp, err := json.Marshal(basePath)
	if err != nil {
		bp = []byte(`"/admin"`)
	}
	injection := []byte("<script>window.__AI_PROXY_ADMIN_BASE_PATH__=" + string(bp) + ";</script>")
	// 插在 <head> 后,保证脚本尽早可用。
	lower := strings.ToLower(string(html))
	idx := strings.Index(lower, "<head>")
	if idx < 0 {
		return append(injection, html...)
	}
	idx += len("<head>")
	out := make([]byte, 0, len(html)+len(injection))
	out = append(out, html[:idx]...)
	out = append(out, injection...)
	out = append(out, html[idx:]...)
	return out
}

func (h *Handler) listProviders(w http.ResponseWriter) {
	cfg := h.runtime.ConfigSnapshot()
	health := h.providerHealth(cfg)
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	providers := make([]providerView, 0, len(names)+2)
	for _, name := range names {
		provider := cfg.Providers[name]
		providers = append(providers, providerView{
			Name:                 name,
			Protocol:             provider.Protocol,
			BaseURL:              provider.BaseURL,
			Models:               append([]string(nil), provider.Models...),
			Endpoints:            append([]string(nil), provider.Endpoints...),
			AllowUnauthenticated: provider.AllowUnauthenticated,
			Priority:             config.EffectiveProviderPriority(provider),
			Fallback:             config.EffectiveProviderFallback(provider),
			Enabled:              !provider.Disabled,
			APIKeyConfigured:     strings.TrimSpace(provider.APIKey) != "",
			Availability:         health[name],
			Source:               classifyProviderSource(false, provider.BaseURL),
		})
	}
	// Builtin rows remain visible while disabled so operators can re-enable
	// routing or adjust priority without hand-editing YAML.
	providers = append(providers,
		h.builtinChatGPTProviderView(cfg),
		h.builtinCodexProviderView(cfg),
	)
	sort.SliceStable(providers, func(i, j int) bool { return providers[i].Name < providers[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{
		"providers":         providers,
		"provider_writable": h.providerStorageAvailable(),
		"config_writable":   strings.TrimSpace(h.configPath) != "",
		"hot_reload":        true,
	})
}

func (h *Handler) providerStorageAvailable() bool {
	runtime, ok := h.runtime.(managedProviderRuntime)
	return ok && runtime.ProviderStorageAvailable()
}

func (h *Handler) builtinCodexProviderView(cfg config.Config) providerView {
	view := providerView{Name: effectivecatalog.CodexOAuthProviderID, Protocol: effectivecatalog.CodexOAuthProviderID, BaseURL: "(Codex OAuth account pool)", Endpoints: []string{config.ProviderEndpointResponses}, Priority: config.EffectiveCodexOAuthProviderPriority(cfg.CodexOAuth), Fallback: true, Enabled: config.EffectiveCodexOAuthProviderEnabled(cfg.CodexOAuth), Source: ProviderSourceBuiltin, Builtin: true, Availability: providerAvailability{Status: "unknown"}}
	if h.chatGPT == nil {
		view.Availability = providerAvailability{Status: "unavailable"}
		view.UnavailableReason = "effective catalog is unavailable"
		if !view.Enabled {
			view.Availability.Status = "disabled"
			view.UnavailableReason = codexBuiltinDisabledReason(cfg)
		}
		h.applyObservedHealth(&view)
		return view
	}
	snap, err := h.chatGPT.ChatGPTEffectiveCatalog(context.Background())
	if err != nil {
		view.Availability = providerAvailability{Status: "unavailable"}
		view.UnavailableReason = "catalog snapshot unavailable"
		if !view.Enabled {
			view.Availability.Status = "disabled"
			view.UnavailableReason = codexBuiltinDisabledReason(cfg)
		}
		h.applyObservedHealth(&view)
		return view
	}
	bp := snap.CodexOAuthProvider
	view.ModelCount, view.AvailableAccounts, view.ConflictCount = bp.ModelCount, bp.AvailableAccounts, bp.ConflictCount
	view.ConflictModels, view.UpdatedAt, view.UnavailableReason = append([]string(nil), bp.ConflictModels...), bp.UpdatedAt, bp.UnavailableReason
	for id := range snap.CodexOAuthModels {
		view.Models = append(view.Models, id)
	}
	sort.Strings(view.Models)
	switch bp.Status {
	case effectivecatalog.StatusReady:
		view.Availability = providerAvailability{Status: "healthy"}
	case effectivecatalog.StatusDegraded:
		view.Availability = providerAvailability{Status: "degraded"}
	case effectivecatalog.StatusEmpty:
		view.Availability = providerAvailability{Status: "unavailable"}
	default:
		view.Availability = providerAvailability{Status: "unknown"}
	}
	if !view.Enabled {
		view.Availability.Status = "disabled"
		view.UnavailableReason = codexBuiltinDisabledReason(cfg)
	}
	h.applyObservedHealth(&view)
	return view
}

func (h *Handler) builtinChatGPTProviderView(cfg config.Config) providerView {
	view := providerView{
		Name:             effectivecatalog.BuiltinProviderID,
		Protocol:         effectivecatalog.BuiltinProviderID,
		BaseURL:          "(account pool)",
		Endpoints:        []string{config.ProviderEndpointChatCompletions, config.ProviderEndpointResponses, config.ProviderEndpointImages},
		Priority:         config.EffectiveChatGPTWebProviderPriority(cfg.ChatGPTWeb),
		Fallback:         false,
		Enabled:          config.EffectiveChatGPTWebProviderEnabled(cfg.ChatGPTWeb),
		APIKeyConfigured: false,
		Source:           ProviderSourceBuiltin,
		Builtin:          true,
		Availability:     providerAvailability{Status: "unknown"},
	}
	if h.chatGPT == nil {
		view.Availability = providerAvailability{Status: "unavailable"}
		view.UnavailableReason = "chatgpt web is not available"
		if !view.Enabled {
			view.Availability.Status = "disabled"
			view.UnavailableReason = chatGPTBuiltinDisabledReason(cfg)
		}
		h.applyObservedHealth(&view)
		return view
	}
	snap, err := h.chatGPT.ChatGPTEffectiveCatalog(context.Background())
	if err != nil {
		view.Availability = providerAvailability{Status: "unavailable"}
		view.UnavailableReason = "catalog snapshot unavailable"
		if !view.Enabled {
			view.Availability.Status = "disabled"
			view.UnavailableReason = chatGPTBuiltinDisabledReason(cfg)
		}
		h.applyObservedHealth(&view)
		return view
	}
	bp := snap.BuiltinProvider
	view.ModelCount = bp.ModelCount
	view.AvailableAccounts = bp.AvailableAccounts
	view.ConflictCount = bp.ConflictCount
	view.ConflictModels = append([]string(nil), bp.ConflictModels...)
	view.UpdatedAt = bp.UpdatedAt
	view.UnavailableReason = bp.UnavailableReason
	models := make([]string, 0, len(snap.BuiltinModels))
	for id := range snap.BuiltinModels {
		models = append(models, id)
	}
	sort.Strings(models)
	view.Models = models
	switch bp.Status {
	case effectivecatalog.StatusReady:
		view.Availability = providerAvailability{Status: "healthy"}
	case effectivecatalog.StatusDegraded:
		view.Availability = providerAvailability{Status: "degraded"}
	case effectivecatalog.StatusDiscovering:
		view.Availability = providerAvailability{Status: "unknown"}
	case effectivecatalog.StatusEmpty:
		view.Availability = providerAvailability{Status: "unavailable"}
	default:
		view.Availability = providerAvailability{Status: "unavailable"}
	}
	if !view.Enabled {
		view.Availability.Status = "disabled"
		view.UnavailableReason = chatGPTBuiltinDisabledReason(cfg)
	}
	h.applyObservedHealth(&view)
	return view
}

func chatGPTBuiltinDisabledReason(config.Config) string {
	return "chatgpt web provider routing is disabled"
}

func codexBuiltinDisabledReason(config.Config) string {
	return "Codex OAuth provider routing is disabled"
}

func (h *Handler) applyObservedHealth(view *providerView) {
	if view == nil || h.metricsRegistry == nil {
		return
	}
	value, ok := h.metricsRegistry.ProviderHealthSnapshot()[view.Name]
	if !ok {
		return
	}
	availability := providerAvailability{
		Status: value.Status, Successes: value.Successes, Failures: value.Failures, ConsecutiveFailures: value.ConsecutiveFailures,
		LastSuccessAt: formatOptionalTime(value.LastSuccessAt), LastFailureAt: formatOptionalTime(value.LastFailureAt), LastStatus: value.LastStatus, LastOutcome: value.LastOutcome,
		Score: value.Score, WindowSeconds: value.WindowSeconds, SampleCount: value.SampleCount, SuccessRate: value.SuccessRate,
		P50MS: value.P50MS, P95MS: value.P95MS, CircuitState: value.CircuitState, CircuitRetryAt: formatOptionalTime(value.CircuitRetryAt),
	}
	// Account-pool disabled/empty state is authoritative: an old successful
	// request sample must not make an intentionally disabled or unroutable
	// builtin provider look usable. Metrics still supply the diagnostic detail.
	if view.Availability.Status == "disabled" || view.Availability.Status == "unavailable" {
		mergeProviderAvailabilityDetails(&view.Availability, availability)
		return
	}
	if availability.Status != "" && availability.Status != "unknown" {
		view.Availability = availability
		return
	}
	// Preserve catalog/credential state while still surfacing live counters.
	mergeProviderAvailabilityDetails(&view.Availability, availability)
}

func mergeProviderAvailabilityDetails(target *providerAvailability, observed providerAvailability) {
	if target == nil {
		return
	}
	target.Successes = observed.Successes
	target.Failures = observed.Failures
	target.ConsecutiveFailures = observed.ConsecutiveFailures
	target.LastSuccessAt = observed.LastSuccessAt
	target.LastFailureAt = observed.LastFailureAt
	target.LastStatus = observed.LastStatus
	target.LastOutcome = observed.LastOutcome
	target.Score = observed.Score
	target.WindowSeconds = observed.WindowSeconds
	target.SampleCount = observed.SampleCount
	target.SuccessRate = observed.SuccessRate
	target.P50MS = observed.P50MS
	target.P95MS = observed.P95MS
	target.CircuitState = observed.CircuitState
	target.CircuitRetryAt = observed.CircuitRetryAt
}

func (h *Handler) providerHealth(cfg config.Config) map[string]providerAvailability {
	result := map[string]providerAvailability{}
	for name, provider := range cfg.Providers {
		status := "unknown"
		if provider.Disabled {
			status = "disabled"
		}
		result[name] = providerAvailability{Status: status}
	}
	if h.metricsRegistry == nil {
		return result
	}
	for name, value := range h.metricsRegistry.ProviderHealthSnapshot() {
		result[name] = providerAvailability{
			Status: value.Status, Successes: value.Successes, Failures: value.Failures, ConsecutiveFailures: value.ConsecutiveFailures,
			LastSuccessAt: formatOptionalTime(value.LastSuccessAt), LastFailureAt: formatOptionalTime(value.LastFailureAt), LastStatus: value.LastStatus, LastOutcome: value.LastOutcome,
			Score: value.Score, WindowSeconds: value.WindowSeconds, SampleCount: value.SampleCount, SuccessRate: value.SuccessRate,
			P50MS: value.P50MS, P95MS: value.P95MS, CircuitState: value.CircuitState, CircuitRetryAt: formatOptionalTime(value.CircuitRetryAt),
		}
	}
	for name, provider := range cfg.Providers {
		if provider.Disabled {
			result[name] = providerAvailability{Status: "disabled"}
		}
	}
	return result
}

func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func (h *Handler) updateProviders(w http.ResponseWriter, r *http.Request) {
	h.updateMu.Lock()
	defer h.updateMu.Unlock()

	providerRuntime, ok := h.runtime.(managedProviderRuntime)
	if !ok || !providerRuntime.ProviderStorageAvailable() {
		writeError(w, http.StatusConflict, "managed Provider storage is unavailable; configure AI_PROXY_CREDENTIAL_KEY")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var request updateRequest
	if err := dec.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if err := ensureJSONEOF(dec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	for _, item := range request.Providers {
		if strings.EqualFold(strings.TrimSpace(item.Name), "chatgptweb") || strings.EqualFold(strings.TrimSpace(item.Name), "codexoauth") || strings.TrimSpace(item.Protocol) == "chatgptweb" || strings.TrimSpace(item.Protocol) == "codexoauth" {
			writeError(w, http.StatusBadRequest, "builtin provider protocols are reserved")
			return
		}
	}
	current := h.runtime.ConfigSnapshot()

	providers, err := buildManagedProviders(request.Providers, current.Providers)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := providerRuntime.ReplaceProviders(providers); err != nil {
		writeError(w, http.StatusInternalServerError, "replace providers: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "message": "provider configuration saved and activated"})
}

func buildManagedProviders(inputs []providerInput, existing map[string]config.Provider) (map[string]config.Provider, error) {
	providers := make(map[string]config.Provider, len(inputs))
	for _, input := range inputs {
		name := strings.ToLower(strings.TrimSpace(input.Name))
		if name == "" {
			return nil, errors.New("provider name is required")
		}
		if _, duplicate := providers[name]; duplicate {
			return nil, fmt.Errorf("duplicate provider %q", name)
		}
		apiKey := strings.TrimSpace(input.APIKey)
		if apiKey == "" && !input.ClearAPIKey {
			apiKey = existing[name].APIKey
		}
		priority := config.DefaultProviderPriority
		if input.Priority != nil {
			priority = *input.Priority
		}
		fallback := true
		if input.Fallback != nil {
			fallback = *input.Fallback
		}
		provider := config.Provider{Name: name, Protocol: strings.ToLower(strings.TrimSpace(input.Protocol)), BaseURL: strings.TrimSpace(input.BaseURL), APIKey: apiKey, Models: append([]string(nil), input.Models...), Endpoints: append([]string(nil), input.Endpoints...), AllowUnauthenticated: input.AllowUnauthenticated, Disabled: !input.Enabled}
		config.ConfigureProviderPolicy(&provider, priority, fallback)
		providers[name] = provider
	}
	return providers, nil
}

func documentRoot(document *yaml.Node) (*yaml.Node, error) {
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return nil, errors.New("config must contain one YAML document")
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("config root must be a mapping")
	}
	return root, nil
}

func appendScalar(mapping *yaml.Node, key, value, tag string) {
	mapping.Content = append(mapping.Content, mappingKey(key), scalar(value, tag))
}

func mappingKey(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func scalar(value, tag string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
	if tag == "!!str" {
		node.Style = yaml.DoubleQuotedStyle
	}
	return node
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func setMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, mappingKey(key), value)
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
