package admin

import (
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

type systemInfoResponse struct {
	Service       systemServiceInfo  `json:"service"`
	Runtime       systemRuntimeInfo  `json:"runtime"`
	AccessMethods []systemAccessInfo `json:"access_methods"`
	Endpoints     []systemEndpoint   `json:"endpoints"`
}

type systemServiceInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	GoVersion   string `json:"go_version"`
	Revision    string `json:"revision,omitempty"`
	BuildTime   string `json:"build_time,omitempty"`
	VCSModified *bool  `json:"vcs_modified,omitempty"`
}

type systemRuntimeInfo struct {
	StartedAt     string `json:"started_at"`
	ServerTime    string `json:"server_time"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

type systemAccessInfo struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Protocol       string   `json:"protocol"`
	Authentication string   `json:"authentication"`
	Endpoints      []string `json:"endpoints"`
	Description    string   `json:"description"`
}

type systemEndpoint struct {
	Method         string `json:"method"`
	Path           string `json:"path"`
	Protocol       string `json:"protocol"`
	Authentication string `json:"authentication"`
	RemoteAccess   string `json:"remote_access"`
	Description    string `json:"description"`
}

func (h *Handler) systemInfo(w http.ResponseWriter) {
	now := time.Now().UTC()
	startedAt := h.startedAt
	version := "dev"
	if source, ok := h.runtime.(systemMetadataRuntime); ok {
		if value := strings.TrimSpace(source.SystemVersion()); value != "" {
			version = value
		}
		if value := source.SystemStartedAt(); !value.IsZero() {
			startedAt = value.UTC()
		}
	}
	if startedAt.IsZero() || startedAt.After(now) {
		startedAt = now
	}
	service := systemServiceInfo{Name: "AetherRelay", Version: version, GoVersion: runtime.Version()}
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				service.Revision = setting.Value
			case "vcs.time":
				service.BuildTime = setting.Value
			case "vcs.modified":
				modified := setting.Value == "true"
				service.VCSModified = &modified
			}
		}
	}
	metricsAccess := "loopback_only"
	if h.runtime != nil {
		cfg := h.runtime.ConfigSnapshot()
		if cfg.MetricsRemoteAccess {
			metricsAccess = "remote_enabled"
			if len(cfg.MetricsAllowedCIDRs) > 0 {
				metricsAccess = "configured_networks"
			}
		}
	}
	clientAuth := "Authorization: Bearer <client_api_key> or X-API-Key: <client_api_key>"
	response := systemInfoResponse{
		Service: service,
		Runtime: systemRuntimeInfo{StartedAt: startedAt.Format(time.RFC3339), ServerTime: now.Format(time.RFC3339), UptimeSeconds: int64(now.Sub(startedAt).Seconds())},
		AccessMethods: []systemAccessInfo{
			{ID: "openai", Name: "OpenAI-compatible REST", Protocol: "openai", Authentication: clientAuth, Endpoints: []string{"/v1/models", "/v1/chat/completions", "/v1/responses", "/v1/completions", "/v1/embeddings", "/v1/images/generations", "/v1/images/edits"}, Description: "Compatible with OpenAI-style REST clients; model availability is reported by /v1/models."},
			{ID: "anthropic", Name: "Anthropic-compatible Messages", Protocol: "anthropic", Authentication: clientAuth, Endpoints: []string{"/v1/messages"}, Description: "Accepts Anthropic Messages requests and routes directly or through supported protocol conversion."},
			{ID: "search", Name: "Web search REST", Protocol: "AetherRelay", Authentication: clientAuth, Endpoints: []string{"/v1/search"}, Description: "Runs the restricted ChatGPT Web search contract."},
		},
		Endpoints: []systemEndpoint{
			{Method: http.MethodGet, Path: "/healthz", Protocol: "health", Authentication: "none", RemoteAccess: "listener", Description: "Service health check."},
			{Method: http.MethodGet, Path: "/v1/models", Protocol: "openai", Authentication: "client_api_key", RemoteAccess: "listener", Description: "List the effective model catalog."},
			{Method: http.MethodPost, Path: "/v1/chat/completions", Protocol: "openai", Authentication: "client_api_key", RemoteAccess: "listener", Description: "OpenAI Chat Completions."},
			{Method: http.MethodPost, Path: "/v1/responses", Protocol: "openai", Authentication: "client_api_key", RemoteAccess: "listener", Description: "OpenAI Responses."},
			{Method: http.MethodPost, Path: "/v1/messages", Protocol: "anthropic", Authentication: "client_api_key", RemoteAccess: "listener", Description: "Anthropic Messages."},
			{Method: http.MethodPost, Path: "/v1/completions", Protocol: "openai", Authentication: "client_api_key", RemoteAccess: "listener", Description: "Legacy OpenAI Completions."},
			{Method: http.MethodPost, Path: "/v1/embeddings", Protocol: "openai", Authentication: "client_api_key", RemoteAccess: "listener", Description: "OpenAI Embeddings."},
			{Method: http.MethodPost, Path: "/v1/images/generations", Protocol: "openai", Authentication: "client_api_key", RemoteAccess: "listener", Description: "Image generation."},
			{Method: http.MethodPost, Path: "/v1/images/edits", Protocol: "openai", Authentication: "client_api_key", RemoteAccess: "listener", Description: "Image editing."},
			{Method: http.MethodPost, Path: "/v1/search", Protocol: "AetherRelay", Authentication: "client_api_key", RemoteAccess: "listener", Description: "Restricted Web search."},
			{Method: http.MethodGet, Path: "/metrics", Protocol: "prometheus", Authentication: "network_policy", RemoteAccess: metricsAccess, Description: "Prometheus metrics."},
			{Method: http.MethodHead, Path: "/metrics", Protocol: "prometheus", Authentication: "network_policy", RemoteAccess: metricsAccess, Description: "Prometheus metrics headers."},
			{Method: http.MethodGet, Path: "/stats", Protocol: "json", Authentication: "network_policy", RemoteAccess: metricsAccess, Description: "Runtime statistics snapshot."},
			{Method: http.MethodHead, Path: "/stats", Protocol: "json", Authentication: "network_policy", RemoteAccess: metricsAccess, Description: "Runtime statistics headers."},
			{Method: http.MethodGet, Path: "/stats/stream", Protocol: "sse", Authentication: "network_policy", RemoteAccess: metricsAccess, Description: "Runtime statistics stream."},
		},
	}
	writeJSON(w, http.StatusOK, response)
}
