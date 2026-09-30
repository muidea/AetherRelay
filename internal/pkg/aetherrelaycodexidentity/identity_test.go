package codexidentity

import (
	"fmt"
	"testing"
)

// CP-HDR-025: valid codex-tui versions have no static upper bound.
func TestObservedProfileAutomaticallyPromotesNewerVersions(t *testing.T) {
	for _, family := range []string{"codex-tui"} {
		t.Run(family, func(t *testing.T) {
			var selected ObservedProfile
			for _, version := range []string{"0.154.0", "0.155.0", "0.159.2", "0.159.3", "0.160.0", "1.0.0"} {
				ua := fmt.Sprintf("%s/%s (Ubuntu; x86_64) terminal (%s; %s)", family, version, family, version)
				candidate, reason := ClassifyObserved(ua, family)
				if reason != ObservationVerified {
					t.Fatalf("version %s rejected: %s", version, reason)
				}
				var promoted bool
				selected, promoted = PromoteObserved(selected, candidate)
				if !promoted || selected.Version != version || selected.UserAgent != ua {
					t.Fatalf("version %s not promoted atomically: %+v", version, selected)
				}
			}
			for _, version := range []string{"0.159.2", "1.0.0"} {
				ua := fmt.Sprintf("%s/%s (Windows; x86_64) alternate (%s; %s)", family, version, family, version)
				candidate, _ := ParseObserved(ua, family)
				if next, promoted := PromoteObserved(selected, candidate); promoted || next != selected {
					t.Fatalf("version %s changed stable profile: %+v", version, next)
				}
			}
		})
	}
}

// CP-HDR-025: automatic promotion still validates the exact version format.
func TestObservedProfileRejectsMalformedNewerVersions(t *testing.T) {
	for _, version := range []string{"0.159", "0.159.2.1", "0.159.2-beta", "0.0159.2", "0.+159.2", "0.10001.0"} {
		ua := fmt.Sprintf("codex-tui/%s (Ubuntu; x86_64) terminal (codex-tui; %s)", version, version)
		if _, reason := ClassifyObserved(ua, "codex-tui"); reason != ObservationInvalidFormat {
			t.Fatalf("version %s classified as %s", version, reason)
		}
	}
}

// CP-HDR-025: tool families and typed hints cannot bypass the atomic pair.
func TestOtherToolVersionsCannotPolluteObservedProfile(t *testing.T) {
	current, ok := ParseObserved("codex-tui/0.159.2 (Ubuntu; x86_64) terminal (codex-tui; 0.159.2)", "codex-tui")
	if !ok {
		t.Fatal("valid Codex profile rejected")
	}
	for _, tool := range []string{"codex_exec", "claude-cli", "opencode", "curl", "python-requests", "codex-tui-other", "codex_exec-other"} {
		ua := fmt.Sprintf("%s/99.0.0 (Ubuntu; x86_64) terminal (%s; 99.0.0)", tool, tool)
		for _, originator := range []string{tool, "codex-tui"} {
			if _, reason := ClassifyObserved(ua, originator); reason != ObservationUnsupportedFamily {
				t.Fatalf("other tool %s classified as %s", tool, reason)
			}
			candidate := ObservedProfile{UserAgent: ua, Originator: originator, Family: "codex-tui", Version: "99.0.0"}
			for _, existing := range []ObservedProfile{{}, current} {
				if next, promoted := PromoteObserved(existing, candidate); promoted || next != existing {
					t.Fatalf("other tool %s polluted profile: %+v", tool, next)
				}
			}
		}
	}
}

// CP-CLIENT-003/CP-HDR-003..004: the default profile is the current locally
// observed Codex CLI request identity.
func TestCurrentProfileUsesObservedCodexTUIUA(t *testing.T) {
	profile := Current()
	if profile.ClientVersion != "0.155.0" || profile.UserAgent != "codex-tui/0.155.0 (Ubuntu 24.4.0; x86_64) gnome-terminal (codex-tui; 0.155.0)" || profile.Originator != "codex-tui" {
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
}

func TestObservedProfileRejectsUnverifiedOrInconsistentCandidates(t *testing.T) {
	for _, candidate := range []struct {
		ObservedProfile
		Reason ObservationReason
	}{
		{ObservedProfile: ObservedProfile{UserAgent: "unknown/0.155.0 (unknown; 0.155.0)", Originator: "unknown"}, Reason: ObservationUnsupportedFamily},
		{ObservedProfile: ObservedProfile{UserAgent: "codex-tui/0.155.0 (Ubuntu; x86_64) terminal (codex-tui; 0.155.0)", Originator: "codex_exec"}, Reason: ObservationFamilyMismatch},
		{ObservedProfile: ObservedProfile{UserAgent: "codex-tui/0.153.4 (Ubuntu; x86_64) terminal (codex-tui; 0.153.4)", Originator: "codex-tui"}, Reason: ObservationUnsupportedVersion},
		{ObservedProfile: ObservedProfile{UserAgent: "codex_exec/0.153.3 (Ubuntu; x86_64) terminal (codex_exec; 0.153.3)", Originator: "codex_exec"}, Reason: ObservationUnsupportedFamily},
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
