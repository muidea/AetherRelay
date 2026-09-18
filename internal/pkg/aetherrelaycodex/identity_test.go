package aetherrelaycodex

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// CP-HDR-007..010: 代理铸造的会话身份必须与客户端原生 UUID 同形，且可确定复现。
func TestStableUUIDIsDeterministicAndShaped(t *testing.T) {
	first := StableUUID("aetherrelay:codex-session:v3\x00key\x00model\x00session")
	parsed, err := uuid.Parse(first)
	if err != nil || len(first) != 36 || parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 {
		t.Fatalf("CP-HDR-007 uuid=%q err=%v", first, err)
	}
	if repeated := StableUUID("aetherrelay:codex-session:v3\x00key\x00model\x00session"); repeated != first {
		t.Fatalf("CP-HDR-007 unstable uuid: %q vs %q", first, repeated)
	}
	if other := StableUUID("aetherrelay:codex-session:v3\x00key\x00model\x00other"); other == first {
		t.Fatalf("CP-HDR-007 namespaces collided: %q", first)
	}
	if StableUUID("  ") != "" {
		t.Fatal("CP-HDR-007 empty seed produced a value")
	}
}

// CP-HDR-010: window_id 的会话段由代理决定，号段取客户端声明；越界值不猜测。
func TestWindowIDKeepsSessionAndClientNumber(t *testing.T) {
	if got := WindowID("session-hash", 37); got != "session-hash:37" {
		t.Fatalf("CP-HDR-010 window id=%q", got)
	}
	for _, number := range []int64{-1, CodexWindowNumberMax + 1} {
		if got := WindowID("session-hash", number); got != "session-hash:0" {
			t.Fatalf("CP-HDR-010 out of range %d produced %q", number, got)
		}
	}
	if WindowID("  ", 3) != "" {
		t.Fatal("CP-HDR-010 empty session produced a window id")
	}
}

// CP-HDR-010: 只有可解析的十进制号段才算声明。
func TestParseWindowIDAcceptsOnlyNumericTail(t *testing.T) {
	for _, value := range []string{"01a080cb-abf8-7900-97a7-7af78ed32b94:37", "session:0"} {
		if _, ok := ParseWindowID(value); !ok {
			t.Fatalf("CP-HDR-010 rejected %q", value)
		}
	}
	if number, ok := ParseWindowID("01a080cb-abf8-7900-97a7-7af78ed32b94:37"); !ok || number != 37 {
		t.Fatalf("CP-HDR-010 parsed number=%d ok=%v", number, ok)
	}
	for _, value := range []string{"", "session", "session:", "session:next", "session:-1", "session:1.5", strings.Repeat("9", 30)} {
		if _, ok := ParseWindowID(value); ok {
			t.Fatalf("CP-HDR-010 accepted %q", value)
		}
	}
}
