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

func TestObservedProfilePromotionUsesVerifiedClientPairs(t *testing.T) {
	oldUA := "codex-tui/0.154.0 (Ubuntu 24.4.0; x86_64) WindowsTerminal (codex-tui; 0.154.0)"
	newUA := "codex-tui/0.155.0 (Ubuntu 24.4.0; x86_64) gnome-terminal (codex-tui; 0.155.0)"
	oldProfile, ok := ParseObserved(oldUA, "codex-tui")
	if !ok {
		t.Fatal("verified 0.154.0 profile was rejected")
	}
	newProfile, ok := ParseObserved(newUA, "codex-tui")
	if !ok {
		t.Fatal("verified 0.155.0 profile was rejected")
	}
	if selected, promoted := PromoteObserved(oldProfile, newProfile); !promoted || selected.UserAgent != newUA {
		t.Fatalf("newer observed profile was not promoted: %+v promoted=%v", selected, promoted)
	}
	if selected, promoted := PromoteObserved(newProfile, oldProfile); promoted || selected.UserAgent != newUA {
		t.Fatalf("observed profile downgraded: %+v promoted=%v", selected, promoted)
	}
	equalVersionDifferentPlatform := "codex-tui/0.155.0 (Ubuntu 24.4.0; x86_64) WindowsTerminal (codex-tui; 0.155.0)"
	equalProfile, ok := ParseObserved(equalVersionDifferentPlatform, "codex-tui")
	if !ok {
		t.Fatal("equal-version alternate platform was rejected")
	}
	if selected, promoted := PromoteObserved(newProfile, equalProfile); promoted || selected.UserAgent != newUA {
		t.Fatalf("equal version changed the persisted platform: %+v promoted=%v", selected, promoted)
	}
	execUA := "codex_exec/0.153.4 (Ubuntu 24.4.0; x86_64) WindowsTerminal (codex_exec; 0.153.4)"
	execProfile, ok := ParseObserved(execUA, "codex_exec")
	if !ok {
		t.Fatal("verified codex_exec profile was rejected")
	}
	if selected, promoted := PromoteObserved(execProfile, oldProfile); !promoted || selected.UserAgent != oldUA {
		t.Fatalf("preferred codex-tui family was not promoted: %+v promoted=%v", selected, promoted)
	}
	if selected, promoted := PromoteObserved(oldProfile, execProfile); promoted || selected.UserAgent != oldUA {
		t.Fatalf("lower-priority codex_exec family replaced codex-tui: %+v promoted=%v", selected, promoted)
	}
}

func TestObservedProfileRejectsUnverifiedOrInconsistentCandidates(t *testing.T) {
	for _, candidate := range []struct {
		ObservedProfile
		Reason ObservationReason
	}{
		{ObservedProfile: ObservedProfile{UserAgent: "unknown/0.155.0 (unknown; 0.155.0)", Originator: "unknown"}, Reason: ObservationUnsupportedFamily},
		{ObservedProfile: ObservedProfile{UserAgent: "codex-tui/0.155.0 (Ubuntu; x86_64) terminal (codex-tui; 0.155.0)", Originator: "codex_exec"}, Reason: ObservationFamilyMismatch},
		{ObservedProfile: ObservedProfile{UserAgent: "codex-tui/0.153.4 (Ubuntu; x86_64) terminal (codex-tui; 0.153.4)", Originator: "codex-tui"}, Reason: ObservationUnsupportedVersion},
		{ObservedProfile: ObservedProfile{UserAgent: "codex_exec/0.154.0 (Ubuntu; x86_64) terminal (codex_exec; 0.154.0)", Originator: "codex_exec"}, Reason: ObservationUnsupportedVersion},
		{ObservedProfile: ObservedProfile{UserAgent: "codex-tui/0.156.0 (Ubuntu; x86_64) terminal (codex-tui; 0.156.0)", Originator: "codex-tui"}, Reason: ObservationUnsupportedVersion},
		{ObservedProfile: ObservedProfile{UserAgent: "codex-tui/0.155.0\r\nInjected: true (codex-tui; 0.155.0)", Originator: "codex-tui"}, Reason: ObservationInvalidControl},
	} {
		if _, ok := ParseObserved(candidate.UserAgent, candidate.Originator); ok {
			t.Fatalf("unverified profile accepted: %+v", candidate)
		}
		if _, reason := ClassifyObserved(candidate.UserAgent, candidate.Originator); reason != candidate.Reason {
			t.Fatalf("candidate=%+v reason=%q want=%q", candidate.ObservedProfile, reason, candidate.Reason)
		}
	}
}

func TestObservedProfileClassifiesMissingAtomicFields(t *testing.T) {
	for _, testCase := range []struct {
		UserAgent  string
		Originator string
		Reason     ObservationReason
	}{
		{Reason: ObservationAbsent},
		{Originator: "codex-tui", Reason: ObservationMissingUserAgent},
		{UserAgent: "codex-tui/0.155.0", Reason: ObservationMissingOriginator},
	} {
		if _, reason := ClassifyObserved(testCase.UserAgent, testCase.Originator); reason != testCase.Reason {
			t.Fatalf("identity=(%q,%q) reason=%q want=%q", testCase.UserAgent, testCase.Originator, reason, testCase.Reason)
		}
	}
	if ValidObservationReason(ObservationReason("raw-client-value")) {
		t.Fatal("unbounded observation reason was accepted")
	}
}
