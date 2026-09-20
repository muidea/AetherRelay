package biz

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	aetherrelaycodex "aetherrelay/internal/pkg/aetherrelaycodex"
)

const (
	defaultCodexBetaFeatures = "remote_compaction_v2"
	maxCodexTurnStateBytes   = 16 << 10
	maxClientUserAgentBytes  = 256
	maxClientOriginatorBytes = 64
)

// codexClientIdentityValue applies the CP-HDR-003/004 boundary at the Block
// boundary: an empty result means "use the versioned profile". Oversized or
// control-character values are rejected rather than forwarded.
func codexClientIdentityValue(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > limit {
		return ""
	}
	if strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return ""
	}
	return value
}

// requestUserAgent and requestOriginator resolve one atomic identity pair. If
// either client field fails the boundary, both fields use the fallback profile.
func (p codexRequestProfile) requestUserAgent() string {
	if userAgent, _, ok := p.requestClientIdentity(); ok {
		return userAgent
	}
	return currentIdentity.UserAgent
}

func (p codexRequestProfile) requestOriginator() string {
	if _, originator, ok := p.requestClientIdentity(); ok {
		return originator
	}
	return currentIdentity.Originator
}

func (p codexRequestProfile) requestClientIdentity() (string, string, bool) {
	userAgent := codexClientIdentityValue(p.clientIdentity.UserAgent, maxClientUserAgentBytes)
	originator := codexClientIdentityValue(p.clientIdentity.Originator, maxClientOriginatorBytes)
	return userAgent, originator, userAgent != "" && originator != ""
}

type codexRequestProfile struct {
	sessionHash   string
	betaFeatures  string
	responsesLite bool
	turnState     string
	fingerprint   events.CodexFingerprint
	// archiveUnredacted is CP-OBS-009: it decides whether the archived attempt
	// observation keeps credential headers verbatim. The zero value redacts.
	archiveUnredacted  bool
	archiveFullContent bool
	// clientIdentity is CP-HDR-003/004: the downstream client's bounded identity.
	// Any empty or rejected field makes the whole pair use the fallback profile.
	clientIdentity events.ClientIdentity
	// turnMetadata is the CP-HDR-011 client projection: turn level values and
	// attributes only. Session identity is never taken from it.
	turnMetadata events.TurnMetadata
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
	session, thread, window := profile.sessionIdentity()
	applyCodexSessionHeaders(headers, session, thread, window)
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

// applyCodexTurnMetadata rebuilds X-Codex-Turn-Metadata for one attempt
// (CP-HDR-011). The identity fields are read back from the headers this attempt
// actually sends, so header, metadata and body can never disagree; the turn level
// fields and attributes come from the client projection, with the fingerprint
// snapshot as the turn level fallback.
func applyCodexTurnMetadata(headers headerSetter, profile codexRequestProfile) {
	getter, ok := headers.(headerGetter)
	if !ok {
		return
	}
	metadata := map[string]any{}
	if installation := strings.TrimSpace(getter.Get("X-Codex-Installation-Id")); installation != "" {
		metadata["installation_id"] = installation
	}
	if session := strings.TrimSpace(getter.Get("Session-Id")); session != "" {
		metadata["session_id"] = session
	}
	if thread := strings.TrimSpace(getter.Get("Thread-Id")); thread != "" {
		metadata["thread_id"] = thread
	}
	if window := strings.TrimSpace(getter.Get("X-Codex-Window-Id")); window != "" {
		metadata["window_id"] = window
	}
	applyCodexTurnFields(metadata, profile)
	if len(metadata) == 0 {
		return
	}
	encoded, err := encodeCodexJSON(metadata)
	if err != nil {
		return
	}
	headers.Set("X-Codex-Turn-Metadata", string(encoded))
}

// decodeTurnMetadataAttributes keeps the bounded attribute object the inbound
// adapter already validated. It stays a best-effort decode: anything unreadable is
// simply not forwarded.
func decodeTurnMetadataAttributes(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var attributes map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&attributes) != nil {
		return nil
	}
	return attributes
}

// sessionIdentity resolves the attempt's own session identity. It is the single
// source for the header, metadata and body carriers. CP-HDR-010 keeps the session
// part proxy owned while the window number stays whatever the client declared, so
// window_id and the window_number attribute never contradict each other.
func (p codexRequestProfile) sessionIdentity() (session, thread, window string) {
	number := p.turnMetadata.WindowNumber
	if number < 0 || number > aetherrelaycodex.CodexWindowNumberMax {
		number = 0
	}
	session = strings.TrimSpace(p.sessionHash)
	thread = session
	window = aetherrelaycodex.WindowID(session, number)
	if mode := normalizedCodexFingerprintMode(p.fingerprint.Mode); mode != "" && strings.TrimSpace(p.fingerprint.SessionID) != "" {
		session = strings.TrimSpace(p.fingerprint.SessionID)
		thread = strings.TrimSpace(p.fingerprint.ThreadID)
		window = strings.TrimSpace(p.fingerprint.WindowID)
	}
	if thread == "" {
		thread = session
	}
	if window == "" && thread != "" {
		window = aetherrelaycodex.WindowID(thread, number)
	}
	return session, thread, window
}

func applyCodexSessionHeaders(headers headerSetter, session, thread, window string) {
	session = strings.TrimSpace(session)
	if headers == nil || session == "" {
		return
	}
	thread = strings.TrimSpace(thread)
	if thread == "" {
		thread = session
	}
	headers.Set("Session-Id", session)
	headers.Set("Thread-Id", thread)
	headers.Set("X-Client-Request-Id", thread)
	if window = strings.TrimSpace(window); window != "" {
		headers.Set("X-Codex-Window-Id", window)
	}
}

func applyCodexFingerprintHeaders(headers headerSetter, fingerprint events.CodexFingerprint) {
	mode := normalizedCodexFingerprintMode(fingerprint.Mode)
	if headers == nil || mode == "" {
		return
	}
	if fingerprint.InstallationID != "" {
		headers.Set("X-Codex-Installation-Id", fingerprint.InstallationID)
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

// applyCodexRequestBody implements the body half of CP-HDR-011: the upstream
// client_metadata always carries the attempt's own identity plus the client's
// bounded turn projection, regardless of fingerprint convergence.
func applyCodexRequestBody(body []byte, profile codexRequestProfile) ([]byte, error) {
	fingerprint := profile.fingerprint
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode Codex fingerprint body: %w", err)
	}
	// CP-HDR-011: the flat projection only carries the known Codex keys, and every
	// value is a string — the upstream rejects anything else with invalid_type
	// (live: client_metadata.auto_review_enabled expected a string). Typed values,
	// including all client attributes, live inside the embedded turn metadata JSON.
	metadata := map[string]any{}
	session, thread, window := profile.sessionIdentity()
	if fingerprint.InstallationID != "" {
		metadata["x-codex-installation-id"] = fingerprint.InstallationID
	}
	if session != "" {
		metadata["session_id"] = session
	}
	if thread != "" {
		metadata["thread_id"] = thread
	}
	if window != "" {
		metadata["x-codex-window-id"] = window
	}
	turnID := strings.TrimSpace(profile.turnMetadata.TurnID)
	if turnID == "" {
		turnID = strings.TrimSpace(fingerprint.TurnID)
	}
	rootTurnID := strings.TrimSpace(profile.turnMetadata.RootTurnID)
	if rootTurnID == "" {
		rootTurnID = turnID
	}
	if turnID != "" {
		metadata["turn_id"] = turnID
	}
	if rootTurnID != "" {
		metadata["root_turn_id"] = rootTurnID
	}
	embedded := map[string]any{}
	for key, value := range metadata {
		embedded[key] = value
	}
	applyCodexTurnFields(embedded, profile)
	if profile.responsesLite {
		metadata["ws_request_header_x_openai_internal_codex_responses_lite"] = "true"
	}
	if len(embedded) > 0 {
		if nested := encodedCodexTurnMetadata(embedded); nested != "" {
			metadata["x-codex-turn-metadata"] = nested
		}
	}
	if len(metadata) == 0 {
		// Nothing to declare: never emit an empty client_metadata envelope.
		return body, nil
	}
	rawMetadata, err := encodeCodexJSON(metadata)
	if err != nil {
		return nil, err
	}
	envelope["client_metadata"] = rawMetadata
	return encodeCodexJSON(envelope)
}

// applyCodexTurnFields fills the non-identity part of a turn metadata object: the
// turn level values (client first, fingerprint fallback) and the client's bounded
// scalar attributes. Shared by the flat header and the embedded body copy so both
// carriers describe the same turn.
func applyCodexTurnFields(metadata map[string]any, profile codexRequestProfile) {
	turnID := strings.TrimSpace(profile.turnMetadata.TurnID)
	if turnID == "" {
		turnID = strings.TrimSpace(profile.fingerprint.TurnID)
	}
	if turnID != "" {
		metadata["turn_id"] = turnID
	}
	if rootTurnID := strings.TrimSpace(profile.turnMetadata.RootTurnID); rootTurnID != "" {
		metadata["root_turn_id"] = rootTurnID
	} else if turnID != "" {
		metadata["root_turn_id"] = turnID
	}
	startedAt := profile.turnMetadata.TurnStartedAtMS
	if startedAt <= 0 {
		startedAt = profile.fingerprint.TurnStartedAtUnixMS
	}
	if startedAt > 0 {
		metadata["turn_started_at_unix_ms"] = startedAt
	}
	for key, value := range decodeTurnMetadataAttributes(profile.turnMetadata.Attributes) {
		metadata[key] = value
	}
}

func encodedCodexTurnMetadata(metadata map[string]any) string {
	encoded, err := encodeCodexJSON(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// encodeCodexJSON is the final upstream encoder for normalized Codex bodies and
// embedded metadata. Go's json.Marshal escapes HTML-sensitive characters even
// though they are ordinary JSON string content; the native client leaves them
// verbatim, so every reconstruction at this boundary must disable that escape.
func encodeCodexJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func normalizedCodexFingerprintMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "scoped":
		return "scoped"
	default:
		return ""
	}
}
