package aetherrelaycodex

import (
	"crypto/sha256"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// CodexWindowNumberMax bounds a client declared window number. The value is a
// small per session counter upstream, so anything beyond this range is treated as
// undeclared instead of being forwarded.
const CodexWindowNumberMax = 1 << 31

// StableUUID renders a deterministic UUID shaped identifier from seed. CP-HDR-007
// ..010 require the proxy owned session identity to look like the client's own
// UUIDs so the upstream sees the same value shape as a native Codex client; the
// seed namespaces the derivation, and 122 bits of the sha256 digest survive the
// version and variant bits.
func StableUUID(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(seed))
	var value uuid.UUID
	copy(value[:], digest[:16])
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return value.String()
}

// WindowID renders the CP-HDR-010 window identity <session>:<number>. The session
// part stays proxy owned; the number is the client declared window number when
// there is one, otherwise the session starts at window 0.
func WindowID(sessionID string, number int64) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	if number < 0 || number > CodexWindowNumberMax {
		number = 0
	}
	return sessionID + ":" + strconv.FormatInt(number, 10)
}

// ParseWindowID extracts the bounded window number from a client declared window
// id such as "<uuid>:37". It reports false when the value carries no usable
// numeric tail, so callers keep their own fallback instead of pinning a bogus 0.
func ParseWindowID(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	index := strings.LastIndex(value, ":")
	if index < 0 || index == len(value)-1 {
		return 0, false
	}
	number, err := strconv.ParseInt(strings.TrimSpace(value[index+1:]), 10, 64)
	if err != nil || number < 0 || number > CodexWindowNumberMax {
		return 0, false
	}
	return number, true
}
