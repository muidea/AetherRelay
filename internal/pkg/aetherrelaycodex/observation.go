package aetherrelaycodex

// ObservationFeature identifies known ancillary events by their exact type.
// Their open-ended payloads are preserved by native transports and archives,
// but cannot establish generated output, response completion or token usage.
// An empty result means the event must follow normal business validation.
func ObservationFeature(eventType string) string {
	switch eventType {
	case "response.metadata", "codex.response.metadata":
		return "response_metadata"
	case "codex.rate_limits":
		return "codex_rate_limits"
	case "responsesapi.websocket_timing":
		return "websocket_timing"
	default:
		return ""
	}
}
