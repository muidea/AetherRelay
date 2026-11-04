// Package aetherrelaycompletion carries immutable text completion evidence.
// It contains no transport handles, credentials or user content.
package aetherrelaycompletion

import (
	"crypto/sha256"
	"fmt"
	"unicode/utf8"
)

const MaxTextBytes = 256 << 10

type Reason string

const (
	Stop          Reason = "stop"
	ModelLength   Reason = "text_model_length"
	EOF           Reason = "text_incomplete"
	ReadError     Reason = "text_read_error"
	Canceled      Reason = "text_canceled"
	EmitError     Reason = "text_emit_error"
	Protocol      Reason = "text_protocol_error"
	Snapshot      Reason = "text_snapshot_error"
	InvalidResult Reason = "text_invalid_result"
	SSELimit      Reason = "text_sse_limit"
	LineLimit     Reason = "text_line_limit"
	EventLimit    Reason = "text_event_limit"
	ContentLimit  Reason = "text_content_limit"
	OutputLimit   Reason = "text_output_limit"
)

// Text is a value snapshot, not a task lifecycle flag. The zero value is unknown
// and must never authorize publishing a successful completion.
type Text struct {
	ResponseStarted bool   `json:"response_started"`
	Completed       bool   `json:"completed"`
	Reason          Reason `json:"reason,omitempty"`
	Terminal        string `json:"terminal,omitempty"`
	SSESHA256       string `json:"sse_sha256,omitempty"`
	SSEBytes        int64  `json:"sse_bytes"`
	Events          int64  `json:"events"`
	TextBytes       int    `json:"text_bytes"`
	// OutputBytes counts accepted append-only deltas; failed delivery does not
	// prove that these bytes reached the caller.
	OutputBytes int    `json:"output_bytes"`
	TextSHA256  string `json:"text_sha256,omitempty"`
	Limit       int64  `json:"limit,omitempty"`
}

func Hash(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }

// Valid verifies evidence against the actual returned or reconstructed text.
func (t Text) Valid(text string) bool {
	return t.Completed && t.Reason == Stop && t.Terminal == "done" && utf8.ValidString(text) && len(text) > 0 && len(text) <= MaxTextBytes && t.TextBytes == len(text) && t.OutputBytes == len(text) && t.TextSHA256 == Hash(text)
}

func (t Text) FinishReason() string {
	if t.Completed && t.Reason == Stop && t.Terminal == "done" {
		return "stop"
	}
	return ""
}

func (r Reason) Budget() bool {
	switch r {
	case SSELimit, LineLimit, EventLimit, ContentLimit, OutputLimit:
		return true
	}
	return false
}

// FailureClass separates local protection from upstream/account health.
func (r Reason) FailureClass() string {
	if r.Budget() {
		return "text_budget"
	}
	switch r {
	case Protocol, Snapshot, InvalidResult:
		return "text_protocol"
	case ModelLength:
		return "text_model_length"
	case Canceled:
		return "cancelled"
	default:
		return "text_incomplete"
	}
}
