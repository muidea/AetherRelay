// Package codexidentity owns the immutable fallback identity and the validation
// and promotion rules for account-scoped client-observed identities.
package codexidentity

import (
	"strconv"
	"strings"
)

// Profile is the versioned, non-secret fallback identity of the verified Codex client.
// Callers receive it by value so no runtime component can mutate the shared
// authority.
type Profile struct {
	ClientVersion string
	UserAgent     string
	Originator    string
	WebsocketBeta string
}

var current = Profile{
	ClientVersion: "0.153.4",
	UserAgent:     "codex_exec/0.153.4 (Ubuntu 24.4.0; x86_64) WindowsTerminal (codex_exec; 0.153.4)",
	Originator:    "codex_exec",
	WebsocketBeta: "responses_websockets=2026-02-06",
}

// Current returns the single verified identity profile by value.
func Current() Profile { return current }

const (
	maxObservedUserAgentBytes  = 256
	maxObservedOriginatorBytes = 64
)

// ObservedProfile is an atomic User-Agent/Originator pair observed on one
// downstream request. Raw values are retained so the selected upstream identity
// always came from a real client rather than a synthesized combination.
type ObservedProfile struct {
	UserAgent  string `json:"user_agent"`
	Originator string `json:"originator"`
	Family     string `json:"family"`
	Version    string `json:"version"`
}

// ObservationReason is a bounded diagnostic classification. It never contains
// the raw client headers.
type ObservationReason string

const (
	ObservationVerified           ObservationReason = "verified"
	ObservationAbsent             ObservationReason = "absent"
	ObservationMissingUserAgent   ObservationReason = "missing_user_agent"
	ObservationMissingOriginator  ObservationReason = "missing_originator"
	ObservationInvalidLength      ObservationReason = "invalid_length"
	ObservationInvalidControl     ObservationReason = "invalid_control_character"
	ObservationInvalidFormat      ObservationReason = "invalid_format"
	ObservationUnsupportedFamily  ObservationReason = "unsupported_family"
	ObservationFamilyMismatch     ObservationReason = "family_mismatch"
	ObservationVersionMismatch    ObservationReason = "version_mismatch"
	ObservationUnsupportedVersion ObservationReason = "unsupported_version"
)

func ValidObservationReason(reason ObservationReason) bool {
	switch reason {
	case ObservationVerified, ObservationAbsent, ObservationMissingUserAgent, ObservationMissingOriginator,
		ObservationInvalidLength, ObservationInvalidControl, ObservationInvalidFormat, ObservationUnsupportedFamily,
		ObservationFamilyMismatch, ObservationVersionMismatch, ObservationUnsupportedVersion:
		return true
	default:
		return false
	}
}

type observedVersion struct{ major, minor, patch int }

// ParseObserved accepts only bounded, internally consistent client profiles
// whose family and version have been verified against the current wire
// contract. Unknown clients can still use the proxy but cannot mutate an
// account-scoped identity profile.
func ParseObserved(userAgent, originator string) (ObservedProfile, bool) {
	profile, reason := ClassifyObserved(userAgent, originator)
	return profile, reason == ObservationVerified
}

// ClassifyObserved validates one atomic User-Agent/Originator pair and returns
// only a bounded reason when it cannot become an account-scoped profile.
func ClassifyObserved(userAgent, originator string) (ObservedProfile, ObservationReason) {
	userAgent = strings.TrimSpace(userAgent)
	originator = strings.TrimSpace(originator)
	if userAgent == "" && originator == "" {
		return ObservedProfile{}, ObservationAbsent
	}
	if userAgent == "" {
		return ObservedProfile{}, ObservationMissingUserAgent
	}
	if originator == "" {
		return ObservedProfile{}, ObservationMissingOriginator
	}
	if len(userAgent) > maxObservedUserAgentBytes || len(originator) > maxObservedOriginatorBytes {
		return ObservedProfile{}, ObservationInvalidLength
	}
	if hasControl(userAgent) || hasControl(originator) {
		return ObservedProfile{}, ObservationInvalidControl
	}
	first := userAgent
	if index := strings.IndexByte(first, ' '); index >= 0 {
		first = first[:index]
	}
	separator := strings.LastIndexByte(first, '/')
	if separator <= 0 || separator == len(first)-1 {
		return ObservedProfile{}, ObservationInvalidFormat
	}
	family, versionText := first[:separator], first[separator+1:]
	if familyRank(family) == 0 {
		return ObservedProfile{}, ObservationUnsupportedFamily
	}
	if originator != family {
		return ObservedProfile{}, ObservationFamilyMismatch
	}
	if !strings.Contains(userAgent, "("+family+"; "+versionText+")") {
		return ObservedProfile{}, ObservationVersionMismatch
	}
	version, ok := parseObservedVersion(versionText)
	if !ok {
		return ObservedProfile{}, ObservationInvalidFormat
	}
	minimum, maximum, supportedFamily := familyVersionRange(family)
	if !supportedFamily || compareObservedVersion(version, minimum) < 0 || compareObservedVersion(version, maximum) > 0 {
		return ObservedProfile{}, ObservationUnsupportedVersion
	}
	return ObservedProfile{UserAgent: userAgent, Originator: originator, Family: family, Version: versionText}, ObservationVerified
}

// PromoteObserved returns a new candidate only when it belongs to a preferred
// verified family or has a strictly newer compatible version in the same
// family. Equal versions keep the persisted platform/profile, preventing
// clients on different platforms from making the account identity oscillate.
func PromoteObserved(currentProfile, candidate ObservedProfile) (ObservedProfile, bool) {
	candidate, validCandidate := ParseObserved(candidate.UserAgent, candidate.Originator)
	if !validCandidate {
		return currentProfile, false
	}
	currentProfile, validCurrent := ParseObserved(currentProfile.UserAgent, currentProfile.Originator)
	if !validCurrent {
		return candidate, true
	}
	currentRank, candidateRank := familyRank(currentProfile.Family), familyRank(candidate.Family)
	if candidateRank > currentRank {
		return candidate, true
	}
	if candidateRank < currentRank || candidate.Family != currentProfile.Family {
		return currentProfile, false
	}
	currentVersion, _ := parseObservedVersion(currentProfile.Version)
	candidateVersion, _ := parseObservedVersion(candidate.Version)
	if compareObservedVersion(candidateVersion, currentVersion) > 0 {
		return candidate, true
	}
	return currentProfile, false
}

func familyRank(value string) int {
	switch value {
	case "codex-tui":
		return 2
	case "codex_exec":
		return 1
	default:
		return 0
	}
}

func familyVersionRange(value string) (observedVersion, observedVersion, bool) {
	switch value {
	case "codex-tui":
		return observedVersion{major: 0, minor: 154, patch: 0}, observedVersion{major: 0, minor: 155, patch: 0}, true
	case "codex_exec":
		version := observedVersion{major: 0, minor: 153, patch: 4}
		return version, version, true
	default:
		return observedVersion{}, observedVersion{}, false
	}
}

func parseObservedVersion(value string) (observedVersion, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return observedVersion{}, false
	}
	values := [3]int{}
	for index, part := range parts {
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 0 || parsed > 10000 || strconv.Itoa(parsed) != part {
			return observedVersion{}, false
		}
		values[index] = parsed
	}
	return observedVersion{major: values[0], minor: values[1], patch: values[2]}, true
}

func compareObservedVersion(left, right observedVersion) int {
	if left.major != right.major {
		return left.major - right.major
	}
	if left.minor != right.minor {
		return left.minor - right.minor
	}
	return left.patch - right.patch
}

func hasControl(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f })
}
