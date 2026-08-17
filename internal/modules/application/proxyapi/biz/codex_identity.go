package biz

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/google/uuid"
)

const (
	codexTurnStateTTL        = time.Hour
	codexTurnStateMaxEntries = 4096
)

type codexTurnStateOrigin struct {
	accountID string
	expiresAt time.Time
}

func resolveCodexFingerprint(accountID, mode, sessionHash string) upevents.CodexFingerprint {
	accountID = strings.TrimSpace(accountID)
	mode = strings.ToLower(strings.TrimSpace(mode))
	if accountID == "" || mode == "" || mode == accevents.FingerprintModeOff {
		return upevents.CodexFingerprint{}
	}
	if mode != accevents.FingerprintModeDevice && mode != accevents.FingerprintModeSession && mode != accevents.FingerprintModeFull {
		return upevents.CodexFingerprint{}
	}
	fingerprint := upevents.CodexFingerprint{
		Mode:           mode,
		InstallationID: stableCodexUUID("aetherrelay:codex-installation:v1\x00" + accountID),
	}
	if mode == accevents.FingerprintModeDevice {
		return fingerprint
	}
	fingerprint.SessionID = stableCodexUUID("aetherrelay:codex-session:v1\x00" + accountID)
	if mode == accevents.FingerprintModeFull {
		fingerprint.ThreadID = fingerprint.SessionID
	} else if strings.TrimSpace(sessionHash) == "" {
		fingerprint.ThreadID = fingerprint.SessionID
	} else {
		fingerprint.ThreadID = stableCodexUUID("aetherrelay:codex-thread:v1\x00" + accountID + "\x00" + strings.TrimSpace(sessionHash))
	}
	if fingerprint.ThreadID == "" {
		fingerprint.ThreadID = fingerprint.SessionID
	}
	fingerprint.WindowID = fingerprint.ThreadID + ":0"
	fingerprint.TurnID = newCodexTurnID()
	return fingerprint
}

func codexFingerprintForTurn(fingerprint upevents.CodexFingerprint) upevents.CodexFingerprint {
	if fingerprint.Mode == accevents.FingerprintModeSession || fingerprint.Mode == accevents.FingerprintModeFull {
		fingerprint.TurnID = newCodexTurnID()
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

func (s *Proxy) noteCodexTurnState(accountID string, headers []upevents.Header) {
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
