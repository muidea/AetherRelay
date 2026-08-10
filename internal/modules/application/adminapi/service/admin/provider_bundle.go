package admin

import (
	"net/http"
	"strings"
	"time"

	config "ai-proxy/internal/pkg/aiproxyconfig"
)

const (
	providerBundleFormat        = "ai-proxy.provider-bundle"
	providerBundleSchemaVersion = 1
)

type providerBundle struct {
	Format        string               `json:"format"`
	SchemaVersion int                  `json:"schema_version"`
	ExportedAt    string               `json:"exported_at,omitempty"`
	Providers     []providerBundleItem `json:"providers"`
	Mode          string               `json:"mode,omitempty"`
}

type providerBundleItem struct {
	Name             string   `json:"name"`
	Protocol         string   `json:"protocol"`
	BaseURL          string   `json:"base_url"`
	APIKey           *string  `json:"api_key,omitempty"`
	APIKeyConfigured bool     `json:"api_key_configured,omitempty"`
	Models           []string `json:"models,omitempty"`
	Endpoints        []string `json:"endpoints,omitempty"`
	Priority         int      `json:"priority,omitempty"`
	Fallback         bool     `json:"fallback,omitempty"`
	Enabled          bool     `json:"enabled"`
}

type providerBundleItemResult struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	Status string `json:"status"`
	APIKey string `json:"api_key,omitempty"`
	Error  string `json:"error,omitempty"`
}

type providerBundleResult struct {
	Format        string                     `json:"format"`
	SchemaVersion int                        `json:"schema_version"`
	Added         int                        `json:"added"`
	Updated       int                        `json:"updated"`
	Skipped       int                        `json:"skipped"`
	Failed        int                        `json:"failed"`
	Items         []providerBundleItemResult `json:"items"`
}

func (h *Handler) exportProviderBundle(w http.ResponseWriter, r *http.Request) {
	includeSecrets := false
	if r.Body != nil {
		var input struct {
			IncludeAPIKeys bool `json:"include_api_keys"`
		}
		if !decodeAdminBody(w, r, &input) {
			return
		}
		includeSecrets = input.IncludeAPIKeys
	}
	current := h.runtime.ConfigSnapshot()
	items := make([]providerBundleItem, 0, len(current.Providers))
	for name, provider := range current.Providers {
		if name == "chatgptweb" || name == "codexoauth" || provider.Protocol == "chatgptweb" || provider.Protocol == "codexoauth" {
			continue
		}
		item := providerBundleItem{Name: name, Protocol: provider.Protocol, BaseURL: provider.BaseURL, Models: append([]string(nil), provider.Models...), Endpoints: append([]string(nil), provider.Endpoints...), Priority: provider.Priority, Fallback: provider.Fallback, Enabled: !provider.Disabled, APIKeyConfigured: strings.TrimSpace(provider.APIKey) != ""}
		if includeSecrets && item.APIKeyConfigured {
			value := provider.APIKey
			item.APIKey = &value
		}
		items = append(items, item)
	}
	exportedAt := time.Now().UTC().Truncate(time.Second)
	profile := bundleExportProfileSafe
	if includeSecrets {
		profile = bundleExportProfileComplete
	}
	payload := providerBundle{Format: providerBundleFormat, SchemaVersion: providerBundleSchemaVersion, ExportedAt: exportedAt.Format(time.RFC3339), Providers: items}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", bundleExportContentDisposition(bundleExportArtifactProvider, providerBundleSchemaVersion, profile, exportedAt))
	writeJSON(w, http.StatusOK, payload)
}

func (h *Handler) importProviderBundle(w http.ResponseWriter, r *http.Request) {
	providerRuntime, ok := h.runtime.(managedProviderRuntime)
	if !ok || !providerRuntime.ProviderStorageAvailable() {
		writeError(w, http.StatusConflict, "managed Provider storage is unavailable; configure AI_PROXY_CREDENTIAL_KEY")
		return
	}
	var payload providerBundle
	if !decodeAdminBody(w, r, &payload) {
		return
	}
	if payload.Format != providerBundleFormat || payload.SchemaVersion != providerBundleSchemaVersion {
		writeError(w, http.StatusBadRequest, "unsupported provider bundle format or schema_version")
		return
	}
	if len(payload.Providers) == 0 || len(payload.Providers) > maxAccountImportItems {
		writeError(w, http.StatusBadRequest, "providers must contain 1 to 1000 entries")
		return
	}
	mode := strings.ToLower(strings.TrimSpace(payload.Mode))
	if mode == "" {
		mode = "merge"
	}
	if mode != "merge" && mode != "replace" && mode != "skip" {
		writeError(w, http.StatusBadRequest, "mode must be merge, replace, or skip")
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	current := h.runtime.ConfigSnapshot()
	next := make(map[string]config.Provider, len(current.Providers))
	for name, provider := range current.Providers {
		next[name] = provider
	}
	result := providerBundleResult{Format: "ai-proxy.provider-bundle-result", SchemaVersion: providerBundleSchemaVersion, Items: make([]providerBundleItemResult, 0, len(payload.Providers))}
	seen := make(map[string]struct{}, len(payload.Providers))
	for _, item := range payload.Providers {
		name := strings.ToLower(strings.TrimSpace(item.Name))
		itemResult := providerBundleItemResult{Name: name}
		if name == "" {
			itemResult.Status, itemResult.Action, itemResult.Error = "error", "rejected", "provider name is required"
			result.Failed++
			result.Items = append(result.Items, itemResult)
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			itemResult.Status, itemResult.Action, itemResult.Error = "error", "rejected", "duplicate provider name"
			result.Failed++
			result.Items = append(result.Items, itemResult)
			continue
		}
		seen[name] = struct{}{}
		if name == "chatgptweb" || name == "codexoauth" || item.Protocol == "chatgptweb" || item.Protocol == "codexoauth" {
			itemResult.Status, itemResult.Action, itemResult.Error = "error", "rejected", "builtin providers are not importable"
			result.Failed++
			result.Items = append(result.Items, itemResult)
			continue
		}
		old, exists := next[name]
		if exists && mode == "skip" {
			itemResult.Status, itemResult.Action = "skipped", "skipped"
			result.Skipped++
			result.Items = append(result.Items, itemResult)
			continue
		}
		if strings.TrimSpace(item.Protocol) == "" || strings.TrimSpace(item.BaseURL) == "" {
			itemResult.Status, itemResult.Action, itemResult.Error = "error", "rejected", "protocol and base_url are required"
			result.Failed++
			result.Items = append(result.Items, itemResult)
			continue
		}
		provider := config.Provider{Name: name, Protocol: strings.ToLower(strings.TrimSpace(item.Protocol)), BaseURL: strings.TrimSpace(item.BaseURL), Models: append([]string(nil), item.Models...), Endpoints: append([]string(nil), item.Endpoints...), Priority: item.Priority, Fallback: item.Fallback, Disabled: !item.Enabled}
		if item.APIKey != nil {
			provider.APIKey = strings.TrimSpace(*item.APIKey)
			if provider.APIKey == "" {
				itemResult.Status, itemResult.Action, itemResult.Error = "error", "rejected", "api_key is required"
				result.Failed++
				result.Items = append(result.Items, itemResult)
				continue
			}
			itemResult.APIKey = "replaced"
		} else if exists {
			provider.APIKey = old.APIKey
			if strings.TrimSpace(provider.APIKey) == "" {
				itemResult.Status, itemResult.Action, itemResult.Error = "error", "rejected", "api_key is required"
				result.Failed++
				result.Items = append(result.Items, itemResult)
				continue
			}
			itemResult.APIKey = "preserved"
		} else {
			itemResult.Status, itemResult.Action, itemResult.Error = "error", "rejected", "api_key is required for new providers"
			result.Failed++
			result.Items = append(result.Items, itemResult)
			continue
		}
		next[name] = provider
		if exists {
			result.Updated++
			itemResult.Action = "updated"
		} else {
			result.Added++
			itemResult.Action = "added"
		}
		itemResult.Status = "ok"
		result.Items = append(result.Items, itemResult)
	}
	if mode == "replace" {
		for name := range next {
			if name == "chatgptweb" || name == "codexoauth" {
				continue
			}
			if _, included := seen[name]; !included {
				delete(next, name)
			}
		}
	}
	if result.Failed > 0 {
		writeJSON(w, http.StatusBadRequest, result)
		return
	}
	if _, err := config.ReplaceProviders(current, next); err != nil {
		writeError(w, http.StatusBadRequest, "invalid provider bundle: "+err.Error())
		return
	}
	if err := providerRuntime.ReplaceProviders(next); err != nil {
		writeError(w, http.StatusInternalServerError, "activate provider bundle: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}
