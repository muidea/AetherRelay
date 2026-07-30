package admin

import (
	"errors"
	"net/http"
	"strings"

	"ai-proxy/internal/pkg/aiproxyconfig"

	"go.yaml.in/yaml/v4"
)

var errInvalidServerConfig = errors.New("server must be a mapping")

type adminPreferencesView struct {
	DefaultLanguage string `json:"default_language"`
	Writable        bool   `json:"writable"`
	HotReload       bool   `json:"hot_reload"`
}

type updateAdminPreferencesRequest struct {
	DefaultLanguage string `json:"default_language"`
}

func (h *Handler) getAdminPreferences(w http.ResponseWriter) {
	cfg := h.runtime.ConfigSnapshot()
	writeJSON(w, http.StatusOK, adminPreferencesView{
		DefaultLanguage: configuredAdminLanguage(cfg),
		Writable:        strings.TrimSpace(h.configPath) != "",
		HotReload:       true,
	})
}

func (h *Handler) updateAdminPreferences(w http.ResponseWriter, r *http.Request) {
	var input updateAdminPreferencesRequest
	if !decodeAdminJSON(w, r, &input) {
		return
	}
	language := strings.TrimSpace(input.DefaultLanguage)
	if language != "zh-CN" && language != "en-US" {
		writeError(w, http.StatusBadRequest, "default_language must be one of zh-CN, en-US")
		return
	}

	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	rewrite, err := prepareConfigRewrite(h.configPath, h.adminBasePath(), func(root *yaml.Node) error {
		server := mappingValue(root, "server")
		if server == nil {
			server = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setMappingValue(root, "server", server)
		}
		if server.Kind != yaml.MappingNode {
			return errInvalidServerConfig
		}
		setMappingValue(server, "admin_default_language", scalar(language, "!!str"))
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.activateAndCommitConfig(rewrite); err != nil {
		writeError(w, http.StatusInternalServerError, "activate config: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, adminPreferencesView{DefaultLanguage: language, Writable: true, HotReload: true})
}

func configuredAdminLanguage(cfg config.Config) string {
	if language := strings.TrimSpace(cfg.AdminAuth.DefaultLanguage); language != "" {
		return language
	}
	return config.DefaultAdminLanguage
}
