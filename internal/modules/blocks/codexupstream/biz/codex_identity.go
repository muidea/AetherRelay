package biz

import (
	"encoding/json"
	"fmt"
	"strings"

	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
)

const (
	defaultCodexBetaFeatures = "remote_compaction_v2"
	maxCodexTurnStateBytes   = 16 << 10
)

type codexRequestProfile struct {
	sessionHash   string
	betaFeatures  string
	responsesLite bool
	turnState     string
	fingerprint   events.CodexFingerprint
}

func resolvedCodexBetaFeatures(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultCodexBetaFeatures
	}
	return value
}

func ensureCodexBetaFeature(value, feature string) string {
	value = resolvedCodexBetaFeatures(value)
	if codexBetaFeaturePresent(value, feature) {
		return value
	}
	return value + "," + feature
}

func codexBetaFeaturePresent(value, expected string) bool {
	for _, token := range strings.Split(value, ",") {
		if strings.TrimSpace(token) == expected {
			return true
		}
	}
	return false
}

func applyCodexFeatureHeaders(headers headerSetter, betaFeatures string, responsesLite bool) {
	if headers == nil {
		return
	}
	if betaFeatures = resolvedCodexBetaFeatures(betaFeatures); betaFeatures != "" {
		headers.Set("X-Codex-Beta-Features", betaFeatures)
	}
	if responsesLite {
		headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	}
}

type headerSetter interface{ Set(string, string) }

func applyCodexRequestIdentity(headers headerSetter, profile codexRequestProfile) {
	if headers == nil {
		return
	}
	applyCodexSessionHeaders(headers, profile.sessionHash)
	applyCodexFingerprintHeaders(headers, profile.fingerprint)
	if turnState := normalizedCodexTurnState(profile.turnState); turnState != "" {
		headers.Set("X-Codex-Turn-State", turnState)
	}
}

func normalizedCodexTurnState(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > maxCodexTurnStateBytes || strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return ""
	}
	return value
}

func applyCodexSessionHeaders(headers headerSetter, sessionHash string) {
	sessionHash = strings.TrimSpace(sessionHash)
	if headers == nil || sessionHash == "" {
		return
	}
	headers.Set("Session-Id", sessionHash)
	headers.Set("Thread-Id", sessionHash)
	headers.Set("X-Client-Request-Id", sessionHash)
	headers.Set("X-Codex-Window-Id", sessionHash+":0")
}

func applyCodexFingerprintHeaders(headers headerSetter, fingerprint events.CodexFingerprint) {
	mode := normalizedCodexFingerprintMode(fingerprint.Mode)
	if headers == nil || mode == "" {
		return
	}
	if fingerprint.InstallationID != "" {
		headers.Set("X-Codex-Installation-Id", fingerprint.InstallationID)
	}
	if mode == "device" {
		return
	}
	if fingerprint.SessionID != "" {
		headers.Set("Session-Id", fingerprint.SessionID)
		headers.Set("Session_Id", fingerprint.SessionID)
	}
	if fingerprint.ThreadID != "" {
		headers.Set("Thread-Id", fingerprint.ThreadID)
		headers.Set("X-Client-Request-Id", fingerprint.ThreadID)
	}
	if fingerprint.WindowID != "" {
		headers.Set("X-Codex-Window-Id", fingerprint.WindowID)
	}
}

func applyCodexFingerprintBody(body []byte, fingerprint events.CodexFingerprint) ([]byte, error) {
	mode := normalizedCodexFingerprintMode(fingerprint.Mode)
	if mode == "" {
		return body, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode Codex fingerprint body: %w", err)
	}
	metadata := map[string]any{}
	if raw := envelope["client_metadata"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return nil, fmt.Errorf("decode Codex client_metadata: %w", err)
		}
	}
	if fingerprint.InstallationID != "" {
		metadata["x-codex-installation-id"] = fingerprint.InstallationID
	}
	if mode != "device" {
		metadata["session_id"] = fingerprint.SessionID
		metadata["thread_id"] = fingerprint.ThreadID
		metadata["turn_id"] = fingerprint.TurnID
		metadata["x-codex-window-id"] = fingerprint.WindowID
	}
	rawMetadata, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	envelope["client_metadata"] = rawMetadata
	return json.Marshal(envelope)
}

func normalizedCodexFingerprintMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "device":
		return "device"
	case "session":
		return "session"
	case "full":
		return "full"
	default:
		return ""
	}
}
