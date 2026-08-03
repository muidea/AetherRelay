package admin

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"ai-proxy/internal/pkg/aiproxyconfig"

	"go.yaml.in/yaml/v4"
)

type builtinProviderInput struct {
	Enabled  *bool `json:"enabled"`
	Priority *int  `json:"priority"`
}

// updateBuiltinProvider changes only the route-level policy of a synthetic
// account-pool Provider. It deliberately cannot alter credentials, discovery,
// lifecycle settings, protocol, capability, or fallback behavior.
func (h *Handler) updateBuiltinProvider(w http.ResponseWriter, r *http.Request, rel string) {
	id := strings.ToLower(strings.Trim(strings.TrimPrefix(rel, "/api/builtin-providers/"), "/"))
	section, err := builtinProviderSection(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	var input builtinProviderInput
	if !decodeAdminJSON(w, r, &input) {
		return
	}
	if input.Enabled == nil && input.Priority == nil {
		writeError(w, http.StatusBadRequest, "enabled or priority is required")
		return
	}
	if input.Priority != nil && (*input.Priority < config.MinProviderPriority || *input.Priority > config.MaxProviderPriority) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("priority must be in [%d,%d]", config.MinProviderPriority, config.MaxProviderPriority))
		return
	}
	if input.Enabled != nil && *input.Enabled && !builtinProviderRuntimeEnabled(id, h.runtime.ConfigSnapshot()) {
		writeError(w, http.StatusConflict, "account-pool runtime is disabled; set its enabled setting and restart ai-proxy before enabling provider routing")
		return
	}

	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	rewrite, err := prepareConfigRewrite(h.configPath, h.adminBasePath(), func(root *yaml.Node) error {
		node := mappingValue(root, section)
		if node == nil {
			node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setMappingValue(root, section, node)
		}
		if node.Kind != yaml.MappingNode {
			return fmt.Errorf("%s must be a mapping", section)
		}
		if input.Enabled != nil {
			setMappingValue(node, "provider_enabled", scalar(fmt.Sprintf("%t", *input.Enabled), "!!bool"))
		}
		if input.Priority != nil {
			setMappingValue(node, "priority", scalar(fmt.Sprintf("%d", *input.Priority), "!!int"))
		}
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

	cfg := h.runtime.ConfigSnapshot()
	view := h.builtinChatGPTProviderView(cfg)
	if id == effectivecatalog.CodexOAuthProviderID {
		view = h.builtinCodexProviderView(cfg)
	}
	writeJSON(w, http.StatusOK, view)
}

func builtinProviderRuntimeEnabled(id string, cfg config.Config) bool {
	switch id {
	case effectivecatalog.BuiltinProviderID:
		return cfg.ChatGPTWeb.Enabled
	case effectivecatalog.CodexOAuthProviderID:
		return cfg.CodexOAuth.Enabled
	default:
		return false
	}
}

func builtinProviderSection(id string) (string, error) {
	switch id {
	case effectivecatalog.BuiltinProviderID:
		return "chatgpt_web", nil
	case effectivecatalog.CodexOAuthProviderID:
		return "codex_oauth", nil
	default:
		return "", errors.New("unknown builtin provider")
	}
}
