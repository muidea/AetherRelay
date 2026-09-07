package codexidentity

import "testing"

// CP-CLIENT-003/CP-HDR-003..004: the default profile is the current locally
// observed Codex CLI request identity.
func TestCurrentProfileUsesObservedCodexExecUA(t *testing.T) {
	profile := Current()
	if profile.ClientVersion != "0.153.4" || profile.UserAgent != "codex_exec/0.153.4 (Ubuntu 24.4.0; x86_64) WindowsTerminal (codex_exec; 0.153.4)" || profile.Originator != "codex_exec" {
		t.Fatalf("profile=%+v", profile)
	}
}
