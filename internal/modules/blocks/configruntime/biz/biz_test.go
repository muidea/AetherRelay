package biz

import (
	"strings"
	"testing"

	config "ai-proxy/internal/pkg/aiproxyconfig"
)

func TestValidateHotReloadRejectsLifecycleChanges(t *testing.T) {
	current := config.Config{ChatGPTWeb: config.ChatGPTWebConfig{Enabled: false}, CodexOAuth: config.CodexOAuthConfig{Enabled: false}}
	next := current
	next.CodexOAuth.Enabled = true
	if err := validateHotReload(current, next); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("Codex enable transition error = %v", err)
	}
	next = current
	next.ChatGPTWeb.Enabled = true
	if err := validateHotReload(current, next); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("ChatGPT enable transition error = %v", err)
	}
}

func TestValidateHotReloadAllowsCodexModelCatalogChange(t *testing.T) {
	current := config.Config{CodexOAuth: config.CodexOAuthConfig{Enabled: true, Models: []string{"gpt-5.2"}}}
	next := current
	next.CodexOAuth.Models = []string{"gpt-5.2", "gpt-5.2-codex"}
	if err := validateHotReload(current, next); err != nil {
		t.Fatalf("model list should be hot-reloadable: %v", err)
	}
}
