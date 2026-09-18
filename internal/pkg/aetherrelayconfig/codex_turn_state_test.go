package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadCodexTurnStateConfig(t *testing.T, section string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "server:\n  listen_addr: 127.0.0.1:18080\n"
	if section != "" {
		body += "codex_oauth:\n" + section
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// CP-HDR-023: an omitted switch enables the fill and an omitted value keeps the
// built-in fallback.
func TestCodexTurnStateFallbackDefaultsToEnabled(t *testing.T) {
	cfg := loadCodexTurnStateConfig(t, "")
	if !cfg.CodexOAuth.EffectiveTurnStateFallback() {
		t.Fatal("CP-HDR-023 turn state fallback must default to enabled")
	}
	if cfg.CodexOAuth.EffectiveDefaultTurnState() != DefaultCodexTurnState {
		t.Fatal("CP-HDR-023 default turn state must be the built-in value")
	}
	if !ValidCodexTurnState(DefaultCodexTurnState) {
		t.Fatal("CP-HDR-023 built-in default must be a valid turn state")
	}
	// The zero-value configuration used by focused tests behaves the same.
	var zero CodexOAuthConfig
	if !zero.EffectiveTurnStateFallback() || zero.EffectiveDefaultTurnState() != DefaultCodexTurnState {
		t.Fatal("CP-HDR-023 zero-value configuration must match the loaded defaults")
	}
}

// CP-HDR-023: both keys are hot reloadable and the configured value replaces the
// built-in fallback without changing the switch.
func TestCodexTurnStateOverridesAreLoaded(t *testing.T) {
	cfg := loadCodexTurnStateConfig(t, "  turn_state_fallback: false\n  default_turn_state: state-configured\n")
	if cfg.CodexOAuth.EffectiveTurnStateFallback() {
		t.Fatal("CP-HDR-023 explicit false must disable the fallback")
	}
	if cfg.CodexOAuth.EffectiveDefaultTurnState() != "state-configured" {
		t.Fatalf("CP-HDR-023 configured default=%q", cfg.CodexOAuth.EffectiveDefaultTurnState())
	}
	// An explicitly empty value means "unset", not "send nothing".
	empty := loadCodexTurnStateConfig(t, "  default_turn_state: \"\"\n")
	if empty.CodexOAuth.EffectiveDefaultTurnState() != DefaultCodexTurnState {
		t.Fatal("CP-HDR-023 empty default must keep the built-in value")
	}
}

// CP-HDR-023: the fallback reaches the upstream as an opaque header, so the
// configured value is validated at load time.
func TestCodexTurnStateOverridesAreValidated(t *testing.T) {
	for name, section := range map[string]string{
		"oversized":  "  default_turn_state: \"" + strings.Repeat("x", MaxCodexTurnStateBytes+1) + "\"\n",
		"control":    "  default_turn_state: \"a\tb\"\n",
		"bad switch": "  turn_state_fallback: maybe\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		body := "server:\n  listen_addr: 127.0.0.1:18080\ncodex_oauth:\n" + section
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("CP-HDR-023 %s configuration was accepted", name)
		}
	}
}

// CP-HDR-023: the same switch and value are reachable from the environment.
func TestCodexTurnStateEnvironmentOverrides(t *testing.T) {
	t.Setenv("AETHERRELAY_CODEX_OAUTH_TURN_STATE_FALLBACK", "false")
	t.Setenv("AETHERRELAY_CODEX_OAUTH_DEFAULT_TURN_STATE", "state-from-env")
	cfg := loadCodexTurnStateConfig(t, "")
	if cfg.CodexOAuth.EffectiveTurnStateFallback() {
		t.Fatal("CP-HDR-023 environment must disable the fallback")
	}
	if cfg.CodexOAuth.EffectiveDefaultTurnState() != "state-from-env" {
		t.Fatalf("CP-HDR-023 environment default=%q", cfg.CodexOAuth.EffectiveDefaultTurnState())
	}
	t.Setenv("AETHERRELAY_CODEX_OAUTH_TURN_STATE_FALLBACK", "not-a-bool")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  listen_addr: 127.0.0.1:18080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("CP-HDR-023 invalid environment switch was accepted")
	}
}

// CP-HDR-022: 强制默认开关默认关闭；打开但没有有效默认值必须在加载期失败，
// 而不是静默失效。
func TestCodexTurnStateForceDefaultSwitch(t *testing.T) {
	if loadCodexTurnStateConfig(t, "").CodexOAuth.EffectiveTurnStateForceDefault() {
		t.Fatal("CP-HDR-022 force switch must default to off")
	}
	cfg := loadCodexTurnStateConfig(t, "  turn_state_force_default: true\n  default_turn_state: state-configured\n")
	if !cfg.CodexOAuth.EffectiveTurnStateForceDefault() {
		t.Fatal("CP-HDR-022 force switch was not loaded")
	}
	// 两个开关各自保留原值：force 优先由解析层实现，不在配置层改语义。
	both := loadCodexTurnStateConfig(t, "  turn_state_force_default: true\n  turn_state_fallback: false\n  default_turn_state: state-configured\n")
	if !both.CodexOAuth.EffectiveTurnStateForceDefault() || both.CodexOAuth.EffectiveTurnStateFallback() {
		t.Fatalf("CP-HDR-022 switches=%+v", both.CodexOAuth)
	}
	for name, section := range map[string]string{
		"missing default": "  turn_state_force_default: true\n",
		"empty default":   "  turn_state_force_default: true\n  default_turn_state: \"\"\n",
		"bad switch":      "  turn_state_force_default: maybe\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		body := "server:\n  listen_addr: 127.0.0.1:18080\ncodex_oauth:\n" + section
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("CP-HDR-022 %s was accepted", name)
		}
	}
}

// CP-HDR-022: 开关与默认值都能从环境注入，环境值同样参与加载期校验。
func TestCodexTurnStateForceDefaultEnvironment(t *testing.T) {
	t.Setenv("AETHERRELAY_CODEX_OAUTH_TURN_STATE_FORCE_DEFAULT", "true")
	t.Setenv("AETHERRELAY_CODEX_OAUTH_DEFAULT_TURN_STATE", "state-from-env")
	cfg := loadCodexTurnStateConfig(t, "")
	if !cfg.CodexOAuth.EffectiveTurnStateForceDefault() {
		t.Fatal("CP-HDR-022 environment must enable the force switch")
	}
	if cfg.CodexOAuth.EffectiveDefaultTurnState() != "state-from-env" {
		t.Fatalf("CP-HDR-022 environment default=%q", cfg.CodexOAuth.EffectiveDefaultTurnState())
	}
	t.Setenv("AETHERRELAY_CODEX_OAUTH_TURN_STATE_FORCE_DEFAULT", "not-a-bool")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  listen_addr: 127.0.0.1:18080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("CP-HDR-022 invalid environment switch was accepted")
	}
}
