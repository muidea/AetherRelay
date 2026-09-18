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
	aetherrelaycodex "aetherrelay/internal/pkg/aetherrelaycodex"
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

func resolveCodexFingerprint(fingerprintSeed, mode, sessionHash, logicalThreadHash string, metadata codexresponses.TurnMetadata) upevents.CodexFingerprint {
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
	if mode != accevents.FingerprintModeScoped {
		return upevents.CodexFingerprint{}
	}
	fingerprint := upevents.CodexFingerprint{
		Mode:           mode,
		InstallationID: stableCodexUUID("aetherrelay:codex-installation:v2\x00" + fingerprintSeed),
	}
	conversation := strings.TrimSpace(sessionHash)
	if conversation == "" {
		return upevents.CodexFingerprint{}
	}
	fingerprint.SessionID = stableCodexUUID("aetherrelay:codex-scoped-session:v1\x00" + fingerprintSeed + "\x00" + conversation)
	if logicalThread := strings.TrimSpace(logicalThreadHash); logicalThread != "" {
		fingerprint.ThreadID = stableCodexUUID("aetherrelay:codex-scoped-thread:v1\x00" + fingerprintSeed + "\x00" + conversation + "\x00" + logicalThread)
	} else {
		fingerprint.ThreadID = fingerprint.SessionID
	}
	// CP-HDR-010: the session part is proxy owned (account seed under fingerprint
	// convergence), the window number stays the client's.
	fingerprint.WindowID = aetherrelaycodex.WindowID(fingerprint.ThreadID, metadata.WindowNumber)
	fingerprint.TurnID = strings.TrimSpace(metadata.TurnID)
	fingerprint.TurnStartedAtUnixMS = metadata.TurnStartedAtMS
	return fingerprint
}

func codexFingerprintForTurn(fingerprint upevents.CodexFingerprint) upevents.CodexFingerprint {
	if fingerprint.Mode == accevents.FingerprintModeScoped {
		fingerprint.TurnID = newCodexTurnID()
		fingerprint.TurnStartedAtUnixMS = time.Now().UnixMilli()
	}
	return fingerprint
}

// freezeCodexTurnMetadata completes the account-independent turn envelope once.
// Every failover attempt then receives the same fallback turn identity and time.
func freezeCodexTurnMetadata(metadata *codexresponses.TurnMetadata) {
	if metadata == nil {
		return
	}
	if strings.TrimSpace(metadata.TurnID) == "" {
		metadata.TurnID = newCodexTurnID()
	}
	if metadata.TurnStartedAtMS <= 0 {
		metadata.TurnStartedAtMS = time.Now().UnixMilli()
	}
}

func stableCodexUUID(seed string) string {
	return aetherrelaycodex.StableUUID(seed)
}

func newCodexTurnID() string {
	value, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return value.String()
}

// toUpstreamTurnMetadata maps the inbound CP-HDR-011 projection onto the Block
// contract. Session identity fields are intentionally absent on both sides; the
// window number travels because CP-HDR-010 keeps the client declared window while
// the session part stays proxy owned.
func toUpstreamTurnMetadata(metadata codexresponses.TurnMetadata) upevents.TurnMetadata {
	return upevents.TurnMetadata{
		TurnID:          metadata.TurnID,
		RootTurnID:      metadata.RootTurnID,
		TurnStartedAtMS: metadata.TurnStartedAtMS,
		WindowNumber:    metadata.WindowNumber,
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
// owned by codexupstream. Both off and scoped retain the declared downstream
// conversation dimension even though scoped replaces its wire representation.
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
// without any observation falls back to the configured default. A withheld value
// reports TurnStateSourceStripped so diagnostics can tell it apart from a client
// that declared nothing at all.
func (s *Proxy) resolveCodexSessionTurnState(accountID string, fingerprint upevents.CodexFingerprint, sessionScope, provided string) (string, codexresponses.TurnStateSource) {
	// CP-HDR-022 force switch: the operator wants a known good state on the wire,
	// so the configured default replaces both the client value and the session
	// record. The forced value is not remembered and the upstream observation is
	// still recorded, so turning the switch off restores the ordinary chain.
	if s.config.CodexOAuth.EffectiveTurnStateForceDefault() {
		if forced := s.config.CodexOAuth.EffectiveDefaultTurnState(); forced != "" {
			return forced, codexresponses.TurnStateSourceForced
		}
		return "", codexresponses.TurnStateSourceAbsent
	}
	provided = strings.TrimSpace(provided)
	if provided != "" {
		guarded := s.guardCodexTurnState(provided, accountID)
		if guarded == "" {
			return "", codexresponses.TurnStateSourceStripped
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
			return "", codexresponses.TurnStateSourceStripped
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
