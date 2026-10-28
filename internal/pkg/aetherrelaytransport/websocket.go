package aetherrelaytransport

// WebsocketDiagnostics is observation only. It contains no payload, remote
// close text, or raw error strings, and must never drive account/replay policy.
// LastMessageBytes describes the last successfully read message, not a rejected
// oversized message whose size the websocket reader does not expose.
type WebsocketDiagnostics struct {
	WebsocketFailureStage        string `json:"websocket_failure_stage,omitempty"`
	WebsocketErrorType           string `json:"websocket_error_type,omitempty"`
	WebsocketCloseCode           int    `json:"websocket_close_code,omitempty"`
	WebsocketCloseReceived       bool   `json:"websocket_close_received,omitempty"`
	WebsocketMessageLimitBytes   int64  `json:"websocket_message_limit_bytes,omitempty"`
	WebsocketLastMessageBytes    int64  `json:"websocket_last_message_bytes,omitempty"`
	WebsocketLastMessageObserved bool   `json:"websocket_last_message_observed,omitempty"`
	WebsocketJSONErrorOffset     int64  `json:"websocket_json_error_offset,omitempty"`
}

func (d WebsocketDiagnostics) Safe() WebsocketDiagnostics {
	switch d.WebsocketFailureStage {
	case "", "read", "decode", "write":
	default:
		d.WebsocketFailureStage = "unknown"
	}
	switch d.WebsocketErrorType {
	case "", "close", "message_limit", "json_syntax", "json_type", "json_invalid", "network", "timeout", "canceled", "read_error", "write_error":
	default:
		d.WebsocketErrorType = "unknown"
	}
	if d.WebsocketCloseCode < 1000 || d.WebsocketCloseCode > 4999 {
		d.WebsocketCloseCode = 0
	}
	// 1006 and 1015 are synthesized locally; neither can appear on the wire.
	if d.WebsocketCloseCode == 0 || d.WebsocketCloseCode == 1006 || d.WebsocketCloseCode == 1015 {
		d.WebsocketCloseReceived = false
	}
	if d.WebsocketMessageLimitBytes < 0 {
		d.WebsocketMessageLimitBytes = 0
	}
	if !d.WebsocketLastMessageObserved || d.WebsocketLastMessageBytes < 0 {
		d.WebsocketLastMessageBytes = 0
	}
	if d.WebsocketJSONErrorOffset < 0 {
		d.WebsocketJSONErrorOffset = 0
	}
	return d
}

// Merge freezes the first failure. Cancel/Close cleanup and late progress must
// not replace its stage, reason, or message measurement with a secondary error.
func (d WebsocketDiagnostics) Merge(next WebsocketDiagnostics) WebsocketDiagnostics {
	d, next = d.Safe(), next.Safe()
	if d.WebsocketFailureStage != "" {
		return d
	}
	if next.WebsocketMessageLimitBytes == 0 {
		next.WebsocketMessageLimitBytes = d.WebsocketMessageLimitBytes
	}
	if !next.WebsocketLastMessageObserved {
		next.WebsocketLastMessageObserved, next.WebsocketLastMessageBytes = d.WebsocketLastMessageObserved, d.WebsocketLastMessageBytes
	}
	return next
}

func (d WebsocketDiagnostics) LogAttrs() []any {
	d = d.Safe()
	if d.WebsocketFailureStage == "" {
		return nil
	}
	return []any{
		"websocket_failure_stage", d.WebsocketFailureStage, "websocket_error_type", d.WebsocketErrorType,
		"websocket_close_code", d.WebsocketCloseCode, "websocket_close_received", d.WebsocketCloseReceived,
		"websocket_message_limit_bytes", d.WebsocketMessageLimitBytes,
		"websocket_last_message_observed", d.WebsocketLastMessageObserved, "websocket_last_message_bytes", d.WebsocketLastMessageBytes,
		"websocket_json_error_offset", d.WebsocketJSONErrorOffset,
	}
}
