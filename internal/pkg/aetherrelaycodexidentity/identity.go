// Package codexidentity owns the immutable outbound identity shared by every
// Codex credential and inference surface.
package codexidentity

// Profile is the versioned, non-secret identity of the verified Codex client.
// Callers receive it by value so no runtime component can mutate the shared
// authority.
type Profile struct {
	ClientVersion string
	UserAgent     string
	Originator    string
	WebsocketBeta string
}

var current = Profile{
	ClientVersion: "0.147.0",
	UserAgent:     "codex-tui/0.147.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.147.0)",
	Originator:    "codex-tui",
	WebsocketBeta: "responses_websockets=2026-02-06",
}

// Current returns the single verified identity profile by value.
func Current() Profile { return current }
