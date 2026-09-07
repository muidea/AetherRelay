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
	ClientVersion: "0.153.4",
	UserAgent:     "codex_exec/0.153.4 (Ubuntu 24.4.0; x86_64) WindowsTerminal (codex_exec; 0.153.4)",
	Originator:    "codex_exec",
	WebsocketBeta: "responses_websockets=2026-02-06",
}

// Current returns the single verified identity profile by value.
func Current() Profile { return current }
