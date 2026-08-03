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

func TestValidateHotReloadAllowsBuiltinRoutingPolicy(t *testing.T) {
	current := config.Config{
		ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true, ProviderEnabled: true},
		CodexOAuth: config.CodexOAuthConfig{Enabled: true, ProviderEnabled: true},
	}
	next := current
	next.ChatGPTWeb.ProviderEnabled = false
	next.ChatGPTWeb.Priority = 5
	next.CodexOAuth.ProviderEnabled = false
	next.CodexOAuth.Priority = 120
	if err := validateHotReload(current, next); err != nil {
		t.Fatalf("builtin routing policy should hot reload: %v", err)
	}
}
