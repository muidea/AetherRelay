package biz

// identityProfile is the single versioned authority for the Codex upstream
// identity. Contract: CP-CLIENT-002..003, CP-HDR-003..006.
type identityProfile struct {
	ClientVersion string
	UserAgent     string
	Originator    string
	WebsocketBeta string
}

var currentIdentity = identityProfile{
	ClientVersion: "0.147.0",
	UserAgent:     "codex-tui/0.147.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.147.0)",
	Originator:    "codex-tui",
	WebsocketBeta: "responses_websockets=2026-02-06",
}
