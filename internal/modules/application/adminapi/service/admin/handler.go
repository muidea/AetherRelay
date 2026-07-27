package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	taskevents "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	imgevents "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/events"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetricsport"
	"ai-proxy/internal/pkg/aiproxyusage"
	"ai-proxy/internal/services/probe"
	adminweb "ai-proxy/web"

	"go.yaml.in/yaml/v4"
)

const maxRequestBodyBytes = 1 << 20

// RuntimeConfig 由代理处理器实现，用于读取和原子切换当前运行配置。
type RuntimeConfig interface {
	ConfigSnapshot() config.Config
	UpdateConfig(config.Config) error
}

type ChatGPTRuntime interface {
	ListChatGPTAccounts(context.Context) ([]accevents.AccountView, error)
	AddChatGPTAccounts(context.Context, []string, string) (accevents.AddResult, error)
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
}

// chatGPTAvailability is intentionally optional to keep isolated HTTP tests
// focused on the transport contract. The production Admin runtime implements
// it and reports whether ChatGPT Web was enabled at process start.
type chatGPTAvailability interface {
	ChatGPTWebEnabled() bool
}

type Handler struct {
	configPath      string
	runtime         RuntimeConfig
	usageStore      usage.Store
	metricsRegistry metricsport.Port
	auth            *authState
	chatGPT         ChatGPTRuntime
	updateMu        sync.Mutex
}

type providerView struct {
	Name                 string               `json:"name"`
	Protocol             string               `json:"protocol"`
	BaseURL              string               `json:"base_url"`
	Models               []string             `json:"models"`
	EndpointCapabilities []string             `json:"endpoint_capabilities"`
	AllowUnauthenticated bool                 `json:"allow_unauthenticated"`
	Enabled              bool                 `json:"enabled"`
	APIKeyConfigured     bool                 `json:"api_key_configured"`
	Availability         providerAvailability `json:"availability"`
	// Source is a display-only classification derived from builtin + base_url
	// (builtin / official / third_party). It is never read from or written to YAML.
	Source string `json:"source"`
	// Builtin marks the non-persistent chatgptweb provider. Admin must not
	// offer edit/delete/toggle/probe for builtin rows.
	Builtin           bool     `json:"builtin,omitempty"`
	ModelCount        int      `json:"model_count,omitempty"`
	AvailableAccounts int      `json:"available_accounts,omitempty"`
	ConflictCount     int      `json:"conflict_count,omitempty"`
	ConflictModels    []string `json:"conflict_models,omitempty"`
	UpdatedAt         string   `json:"updated_at,omitempty"`
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
}

type providerAvailability struct {
	Status              string `json:"status"`
	Successes           int64  `json:"successes"`
	Failures            int64  `json:"failures"`
	ConsecutiveFailures int64  `json:"consecutive_failures"`
	LastSuccessAt       string `json:"last_success_at,omitempty"`
	LastFailureAt       string `json:"last_failure_at,omitempty"`
	LastStatus          int    `json:"last_status,omitempty"`
	LastOutcome         string `json:"last_outcome,omitempty"`
}

type providerInput struct {
	Name                 string   `json:"name"`
	Protocol             string   `json:"protocol"`
	BaseURL              string   `json:"base_url"`
	APIKey               string   `json:"api_key"`
	ClearAPIKey          bool     `json:"clear_api_key"`
	Models               []string `json:"models"`
	EndpointCapabilities []string `json:"endpoint_capabilities"`
	AllowUnauthenticated bool     `json:"allow_unauthenticated"`
	Enabled              bool     `json:"enabled"`
}

type updateRequest struct {
	Providers []providerInput `json:"providers"`
}

func NewHandler(configPath string, runtime RuntimeConfig) *Handler {
	h := &Handler{configPath: configPath, runtime: runtime}
	if runtime != nil {
		h.auth = newAuthState(runtime.ConfigSnapshot().AdminAuth)
	} else {
		h.auth = newAuthState(config.AdminAuthConfig{BasePath: config.DefaultAdminBasePath, SessionTTLSeconds: config.DefaultAdminSessionTTLSeconds})
	}
	return h
}

func (h *Handler) WithChatGPTRuntime(runtime ChatGPTRuntime) *Handler { h.chatGPT = runtime; return h }

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
	case strings.HasPrefix(rel, "/api/chatgpt/") && !h.chatGPTWebAvailable():
		writeError(w, http.StatusServiceUnavailable, "chatgpt web is not enabled")
	case (rel == "/" || rel == "") && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		h.serveIndex(w, r)
	case rel == "/api/providers" && r.Method == http.MethodGet:
		h.listProviders(w)
	case rel == "/api/providers" && r.Method == http.MethodPut:
		if !h.requireAdminMutation(w, r) {
			return
		}
		h.updateProviders(w, r)
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
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) chatGPTWebAvailable() bool {
	if h.chatGPT == nil {
		return false
	}
	availability, ok := h.chatGPT.(chatGPTAvailability)
	return !ok || availability.ChatGPTWebEnabled()
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
	result, err := probe.Check(r.Context(), h.runtime.ConfigSnapshot(), name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.metricsRegistry != nil {
		h.metricsRegistry.RecordRequestPlan(name, result.Model, result.Capability, result.Status, time.Duration(result.DurationMS)*time.Millisecond, mapProbeOutcome(result.Conclusion), "", result.Protocol, result.UpstreamPath, "probe")
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": name, "status": result.Status, "duration_ms": result.DurationMS, "conclusion": result.Conclusion, "summary": result.Summary})
}

func mapProbeOutcome(conclusion string) string {
	if conclusion == "success" {
		return "success"
	}
	if conclusion == "capability_drift" {
		return "capability_drift"
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

	providers := make([]providerView, 0, len(names)+1)
	for _, name := range names {
		provider := cfg.Providers[name]
		providers = append(providers, providerView{
			Name:                 name,
			Protocol:             provider.Protocol,
			BaseURL:              provider.BaseURL,
			Models:               append([]string(nil), provider.Models...),
			EndpointCapabilities: append([]string(nil), provider.EndpointCapabilities...),
			AllowUnauthenticated: provider.AllowUnauthenticated,
			Enabled:              !provider.Disabled,
			APIKeyConfigured:     strings.TrimSpace(provider.APIKey) != "",
			Availability:         health[name],
			Source:               classifyProviderSource(false, provider.BaseURL),
		})
	}
	if cfg.ChatGPTWeb.Enabled {
		providers = append(providers, h.builtinChatGPTProviderView())
		sort.SliceStable(providers, func(i, j int) bool { return providers[i].Name < providers[j].Name })
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers":  providers,
		"writable":   strings.TrimSpace(h.configPath) != "",
		"hot_reload": true,
	})
}

func (h *Handler) builtinChatGPTProviderView() providerView {
	view := providerView{
		Name:                 effectivecatalog.BuiltinProviderID,
		Protocol:             effectivecatalog.BuiltinProviderID,
		BaseURL:              "(account pool)",
		EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions, config.EndpointCapabilityImages},
		Enabled:              true,
		APIKeyConfigured:     false,
		Source:               ProviderSourceBuiltin,
		Builtin:              true,
		Availability:         providerAvailability{Status: "unknown"},
	}
	if h.chatGPT == nil || !h.chatGPTWebAvailable() {
		view.Availability = providerAvailability{Status: "unavailable"}
		view.UnavailableReason = "chatgpt web is not available"
		return view
	}
	snap, err := h.chatGPT.ChatGPTEffectiveCatalog(context.Background())
	if err != nil {
		view.Availability = providerAvailability{Status: "unavailable"}
		view.UnavailableReason = "catalog snapshot unavailable"
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
	for id, model := range snap.BuiltinModels {
		if model.ConflictWithStatic {
			continue
		}
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
	return view
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
	data, err := h.metricsRegistry.StatsJSON()
	if err != nil {
		return result
	}
	var snapshot struct {
		ProviderHealth map[string]struct {
			Successes           int64  `json:"successes"`
			Failures            int64  `json:"failures"`
			ConsecutiveFailures int64  `json:"consecutive_failures"`
			LastSuccessAt       string `json:"last_success_at"`
			LastFailureAt       string `json:"last_failure_at"`
			LastStatus          int    `json:"last_status"`
			LastOutcome         string `json:"last_outcome"`
		} `json:"provider_health"`
	}
	if json.Unmarshal(data, &snapshot) != nil {
		return result
	}
	for name, value := range snapshot.ProviderHealth {
		status := "unknown"
		switch {
		case value.LastOutcome == "capability_drift":
			status = "capability_drift"
		case value.LastStatus == 401 || value.LastStatus == 403:
			status = "credential_error"
		case value.ConsecutiveFailures >= 3:
			status = "unavailable"
		case value.Failures > 0:
			status = "degraded"
		case value.Successes > 0:
			status = "healthy"
		}
		result[name] = providerAvailability{Status: status, Successes: value.Successes, Failures: value.Failures, ConsecutiveFailures: value.ConsecutiveFailures, LastSuccessAt: value.LastSuccessAt, LastFailureAt: value.LastFailureAt, LastStatus: value.LastStatus, LastOutcome: value.LastOutcome}
	}
	for name, provider := range cfg.Providers {
		if provider.Disabled {
			result[name] = providerAvailability{Status: "disabled"}
		}
	}
	return result
}

func (h *Handler) updateProviders(w http.ResponseWriter, r *http.Request) {
	h.updateMu.Lock()
	defer h.updateMu.Unlock()

	if strings.TrimSpace(h.configPath) == "" {
		writeError(w, http.StatusConflict, "no writable config file is active")
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
		if strings.EqualFold(strings.TrimSpace(item.Name), "chatgptweb") || strings.TrimSpace(item.Protocol) == "chatgptweb" {
			writeError(w, http.StatusBadRequest, "protocol chatgptweb is reserved for the builtin provider")
			return
		}
	}
	if len(request.Providers) == 0 && !h.runtime.ConfigSnapshot().ChatGPTWeb.Enabled {
		writeError(w, http.StatusBadRequest, "at least one provider is required")
		return
	}

	cfg, err := writeProviders(h.configPath, h.adminBasePath(), request.Providers)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.activateConfig(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "activate config: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "message": "provider configuration saved and activated"})
}

func writeProviders(path, expectedAdminBasePath string, providers []providerInput) (config.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config.Config{}, fmt.Errorf("read config: %w", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return config.Config{}, fmt.Errorf("parse config: %w", err)
	}
	root, err := documentRoot(&document)
	if err != nil {
		return config.Config{}, err
	}
	existingSecrets := providerSecrets(root)
	providersNode, err := buildProvidersNode(providers, existingSecrets)
	if err != nil {
		return config.Config{}, err
	}
	setMappingValue(root, "providers", providersNode)

	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return config.Config{}, fmt.Errorf("encode config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return config.Config{}, fmt.Errorf("close config encoder: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return config.Config{}, fmt.Errorf("stat config: %w", err)
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".ai-proxy-config-*.yaml")
	if err != nil {
		return config.Config{}, fmt.Errorf("create temporary config: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		_ = temp.Close()
		return config.Config{}, fmt.Errorf("set temporary config mode: %w", err)
	}
	if _, err := temp.Write(encoded.Bytes()); err != nil {
		_ = temp.Close()
		return config.Config{}, fmt.Errorf("write temporary config: %w", err)
	}
	if err := temp.Close(); err != nil {
		return config.Config{}, fmt.Errorf("close temporary config: %w", err)
	}

	cfg, err := config.Load(tempPath)
	if err != nil {
		return config.Config{}, fmt.Errorf("configuration rejected: %w", err)
	}
	if cfg.AdminAuth.BasePath != expectedAdminBasePath {
		return config.Config{}, errAdminBasePathRestart
	}
	if err := os.Rename(tempPath, path); err != nil {
		return config.Config{}, fmt.Errorf("replace config: %w", err)
	}
	return cfg, nil
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

func providerSecrets(root *yaml.Node) map[string]string {
	providers := mappingValue(root, "providers")
	secrets := map[string]string{}
	if providers == nil || providers.Kind != yaml.MappingNode {
		return secrets
	}
	for i := 0; i+1 < len(providers.Content); i += 2 {
		name := strings.ToLower(strings.TrimSpace(providers.Content[i].Value))
		provider := providers.Content[i+1]
		if secret := mappingValue(provider, "api_key"); secret != nil {
			secrets[name] = secret.Value
		}
	}
	return secrets
}

func buildProvidersNode(inputs []providerInput, existingSecrets map[string]string) (*yaml.Node, error) {
	byName := make(map[string]providerInput, len(inputs))
	for _, input := range inputs {
		name := strings.ToLower(strings.TrimSpace(input.Name))
		if name == "" {
			return nil, errors.New("provider name is required")
		}
		if _, exists := byName[name]; exists {
			return nil, fmt.Errorf("duplicate provider %q", name)
		}
		input.Name = name
		byName[name] = input
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, name := range names {
		input := byName[name]
		secret := strings.TrimSpace(input.APIKey)
		if secret == "" && !input.ClearAPIKey {
			secret = existingSecrets[name]
		}
		provider := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendScalar(provider, "enabled", fmt.Sprintf("%t", input.Enabled), "!!bool")
		appendScalar(provider, "protocol", strings.ToLower(strings.TrimSpace(input.Protocol)), "!!str")
		appendScalar(provider, "base_url", strings.TrimSpace(input.BaseURL), "!!str")
		appendScalar(provider, "api_key", secret, "!!str")
		appendScalar(provider, "endpoint_capabilities", strings.Join(input.EndpointCapabilities, ", "), "!!str")
		appendScalar(provider, "models", strings.Join(input.Models, ", "), "!!str")
		if input.AllowUnauthenticated {
			appendScalar(provider, "allow_unauthenticated", "true", "!!bool")
		}
		node.Content = append(node.Content, mappingKey(name), provider)
	}
	return node, nil
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
