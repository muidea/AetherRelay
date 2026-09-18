package biz

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	aetherrelayconfig "aetherrelay/internal/pkg/aetherrelayconfig"
	"github.com/google/uuid"
)

const (
	codexTurnStateTTL        = time.Hour
	codexTurnStateMaxEntries = 4096

	// CP-HDR-022 keeps a session record for the whole process lifetime, so the
	// entry cap alone is not a resource bound. Oversized values are rejected and
	// the table carries a byte budget instead of a TTL.
	codexSessionTurnStateMaxEntries = 4096
	codexSessionTurnStateMaxBytes   = 4 << 20
)

type codexTurnStateOrigin struct {
	accountID string
	expiresAt time.Time
}

func resolveCodexFingerprint(fingerprintSeed, mode, sessionHash string) upevents.CodexFingerprint {
	fingerprintSeed = strings.TrimSpace(fingerprintSeed)
	mode = strings.ToLower(strings.TrimSpace(mode))
	if fingerprintSeed == "" || mode == "" || mode == accevents.FingerprintModeOff {
		return upevents.CodexFingerprint{}
	}
	parsedSeed, err := uuid.Parse(fingerprintSeed)
	if err != nil || parsedSeed == uuid.Nil {
		return upevents.CodexFingerprint{}
	}
	fingerprintSeed = parsedSeed.String()
	if mode != accevents.FingerprintModeDevice && mode != accevents.FingerprintModeSession && mode != accevents.FingerprintModeFull {
		return upevents.CodexFingerprint{}
	}
	fingerprint := upevents.CodexFingerprint{
		Mode:           mode,
		InstallationID: stableCodexUUID("aetherrelay:codex-installation:v2\x00" + fingerprintSeed),
	}
	if mode == accevents.FingerprintModeDevice {
		return fingerprint
	}
	fingerprint.SessionID = stableCodexUUID("aetherrelay:codex-session:v2\x00" + fingerprintSeed)
	if mode == accevents.FingerprintModeFull {
		fingerprint.ThreadID = fingerprint.SessionID
	} else if strings.TrimSpace(sessionHash) == "" {
		fingerprint.ThreadID = fingerprint.SessionID
	} else {
		fingerprint.ThreadID = stableCodexUUID("aetherrelay:codex-thread:v2\x00" + fingerprintSeed + "\x00" + strings.TrimSpace(sessionHash))
	}
	if fingerprint.ThreadID == "" {
		fingerprint.ThreadID = fingerprint.SessionID
	}
	fingerprint.WindowID = fingerprint.ThreadID + ":0"
	fingerprint.TurnID = newCodexTurnID()
	fingerprint.TurnStartedAtUnixMS = time.Now().UnixMilli()
	return fingerprint
}

func codexFingerprintForTurn(fingerprint upevents.CodexFingerprint) upevents.CodexFingerprint {
	if fingerprint.Mode == accevents.FingerprintModeSession || fingerprint.Mode == accevents.FingerprintModeFull {
		fingerprint.TurnID = newCodexTurnID()
		fingerprint.TurnStartedAtUnixMS = time.Now().UnixMilli()
	}
	return fingerprint
}

func stableCodexUUID(seed string) string {
	if strings.TrimSpace(seed) == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(seed))
	var value uuid.UUID
	copy(value[:], digest[:16])
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return value.String()
}

func newCodexTurnID() string {
	value, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return value.String()
}

// toUpstreamTurnMetadata maps the inbound CP-HDR-011 projection onto the Block
// contract. Identity fields are intentionally absent on both sides.
func toUpstreamTurnMetadata(metadata codexresponses.TurnMetadata) upevents.TurnMetadata {
	return upevents.TurnMetadata{
		TurnID:          metadata.TurnID,
		RootTurnID:      metadata.RootTurnID,
		TurnStartedAtMS: metadata.TurnStartedAtMS,
		Attributes:      metadata.Attributes,
	}
}

func codexTurnStateKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (s *Proxy) guardCodexTurnState(value, accountID string) string {
	key := codexTurnStateKey(value)
	if key == "" || strings.TrimSpace(accountID) == "" {
		return ""
	}
	now := time.Now()
	s.mu.Lock()
	origin, found := s.codexTurnStates[key]
	if found && now.After(origin.expiresAt) {
		delete(s.codexTurnStates, key)
		found = false
	}
	s.mu.Unlock()
	if found && origin.accountID != accountID {
		return ""
	}
	return strings.TrimSpace(value)
}

// codexTurnStateScope is the CP-HDR-022 record unit: the minting account plus the
// downstream session identity tuple. sessionScope is the digest of a conversation
// the client explicitly declared, so a client that declares none gets no record
// unit instead of sharing one with unrelated requests. The tuple keeps the
// fingerprint inputs rather than the derived outbound Session-Id, which makes the
// key exact under every fingerprint mode without mirroring the header rewrite
// owned by codexupstream: off/device degenerate to the upstream Session-Id, while
// session/full stay per downstream session because the upstream collapses those
// sessions onto one account level Session-Id (and still separates them by
// Thread-Id).
type codexTurnStateScope struct {
	accountID    string
	mode         string
	sessionID    string
	sessionScope string
}

type codexSessionTurnState struct {
	state     string
	updatedAt time.Time
}

func codexTurnStateScopeFor(accountID string, fingerprint upevents.CodexFingerprint, sessionScope string) (codexTurnStateScope, bool) {
	accountID = strings.TrimSpace(accountID)
	sessionScope = strings.TrimSpace(sessionScope)
	// CP-HDR-022: without an account or a declared downstream conversation there
	// is no record unit. The service layer leaves the scope empty when the client
	// declared no conversation identity, so unrelated stateless requests can
	// never share one bucket.
	if accountID == "" || sessionScope == "" {
		return codexTurnStateScope{}, false
	}
	return codexTurnStateScope{
		accountID:    accountID,
		mode:         strings.ToLower(strings.TrimSpace(fingerprint.Mode)),
		sessionID:    strings.TrimSpace(fingerprint.SessionID),
		sessionScope: sessionScope,
	}, true
}

// resolveCodexSessionTurnState returns the value to send for one attempt and its
// bounded provenance. A client supplied value wins and is remembered; a value
// that CP-HDR-020 stripped is never replaced, because CP-HDR-022 fills only what
// the client omitted. Otherwise the session record is replayed, and a session
// without any observation falls back to the configured default.
func (s *Proxy) resolveCodexSessionTurnState(accountID string, fingerprint upevents.CodexFingerprint, sessionScope, provided string) (string, codexresponses.TurnStateSource) {
	provided = strings.TrimSpace(provided)
	if provided != "" {
		guarded := s.guardCodexTurnState(provided, accountID)
		if guarded == "" {
			return "", codexresponses.TurnStateSourceAbsent
		}
		if scope, ok := codexTurnStateScopeFor(accountID, fingerprint, sessionScope); ok {
			s.noteCodexSessionTurnState(scope, guarded)
		}
		return guarded, codexresponses.TurnStateSourceClient
	}
	if !s.config.CodexOAuth.EffectiveTurnStateFallback() {
		return "", codexresponses.TurnStateSourceAbsent
	}
	if scope, ok := codexTurnStateScopeFor(accountID, fingerprint, sessionScope); ok {
		if state := s.sessionCodexTurnState(scope); state != "" {
			// A record can outlive the account identity it was minted for, so the
			// replay passes the same CP-HDR-020 guard as a client supplied value.
			if guarded := s.guardCodexTurnState(state, accountID); guarded != "" {
				return guarded, codexresponses.TurnStateSourceSession
			}
			return "", codexresponses.TurnStateSourceAbsent
		}
	}
	// An empty effective default means "no built-in value and none configured":
	// the attempt then sends no turn state at all, which is not a fallback.
	if fallback := s.config.CodexOAuth.EffectiveDefaultTurnState(); fallback != "" {
		return fallback, codexresponses.TurnStateSourceDefault
	}
	return "", codexresponses.TurnStateSourceAbsent
}

func (s *Proxy) sessionCodexTurnState(scope codexTurnStateScope) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.codexSessionTurnStates[scope].state
}

// noteCodexSessionTurnState remembers an observed value for one record unit. The
// configured default is never recorded, so a session that only ever fell back
// keeps falling back instead of pinning a synthetic value.
func (s *Proxy) noteCodexSessionTurnState(scope codexTurnStateScope, state string) {
	state = strings.TrimSpace(state)
	if state == "" || !aetherrelayconfig.ValidCodexTurnState(state) {
		return
	}
	if fallback := s.config.CodexOAuth.EffectiveDefaultTurnState(); fallback != "" && state == fallback {
		return
	}
	s.mu.Lock()
	if s.codexSessionTurnStates == nil {
		s.codexSessionTurnStates = map[codexTurnStateScope]codexSessionTurnState{}
	}
	// The map key covers the session identity, so a rewrite always replaces the
	// previous value of the same unit.
	if previous, found := s.codexSessionTurnStates[scope]; found {
		s.codexSessionTurnStateBytes -= len(previous.state)
	}
	s.codexSessionTurnStates[scope] = codexSessionTurnState{state: state, updatedAt: time.Now()}
	s.codexSessionTurnStateBytes += len(state)
	s.pruneCodexSessionTurnStates()
	s.mu.Unlock()
}

// pruneCodexSessionTurnStates enforces both budgets at once. Caller holds s.mu.
// Eviction is batched and ordered by observation time so a saturated table does
// not scan on every insert and the active sessions survive.
func (s *Proxy) pruneCodexSessionTurnStates() {
	if len(s.codexSessionTurnStates) <= codexSessionTurnStateMaxEntries && s.codexSessionTurnStateBytes <= codexSessionTurnStateMaxBytes {
		return
	}
	scopes := make([]codexTurnStateScope, 0, len(s.codexSessionTurnStates))
	for scope := range s.codexSessionTurnStates {
		scopes = append(scopes, scope)
	}
	sort.Slice(scopes, func(i, j int) bool {
		return s.codexSessionTurnStates[scopes[i]].updatedAt.Before(s.codexSessionTurnStates[scopes[j]].updatedAt)
	})
	batch := len(scopes) / 8
	if batch < 1 {
		batch = 1
	}
	for index, scope := range scopes {
		if index >= batch && len(s.codexSessionTurnStates) <= codexSessionTurnStateMaxEntries && s.codexSessionTurnStateBytes <= codexSessionTurnStateMaxBytes {
			break
		}
		s.codexSessionTurnStateBytes -= len(s.codexSessionTurnStates[scope].state)
		delete(s.codexSessionTurnStates, scope)
	}
	if s.codexSessionTurnStateBytes < 0 {
		s.codexSessionTurnStateBytes = 0
	}
}

// noteCodexTurnState records one upstream observation. The provenance table keeps
// only the state hash, while the CP-HDR-022 session table keeps the opaque value
// for same-session replay.
func (s *Proxy) noteCodexTurnState(accountID string, fingerprint upevents.CodexFingerprint, sessionScope string, headers []upevents.Header) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return
	}
	state := ""
	for _, header := range headers {
		if strings.EqualFold(strings.TrimSpace(header.Name), "X-Codex-Turn-State") {
			state = strings.TrimSpace(header.Value)
			break
		}
	}
	key := codexTurnStateKey(state)
	if key == "" {
		return
	}
	if scope, ok := codexTurnStateScopeFor(accountID, fingerprint, sessionScope); ok {
		s.noteCodexSessionTurnState(scope, state)
	}
	now := time.Now()
	s.mu.Lock()
	if s.codexTurnStates == nil {
		s.codexTurnStates = map[string]codexTurnStateOrigin{}
	}
	s.codexTurnStates[key] = codexTurnStateOrigin{accountID: accountID, expiresAt: now.Add(codexTurnStateTTL)}
	if len(s.codexTurnStates) > codexTurnStateMaxEntries {
		for candidate, origin := range s.codexTurnStates {
			if now.After(origin.expiresAt) {
				delete(s.codexTurnStates, candidate)
			}
		}
		for candidate := range s.codexTurnStates {
			if len(s.codexTurnStates) <= codexTurnStateMaxEntries {
				break
			}
			if candidate != key {
				delete(s.codexTurnStates, candidate)
			}
		}
	}
	s.mu.Unlock()
}
