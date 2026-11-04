package aetherrelaycompletion

import "unicode/utf8"

// Search records the terminal evidence observed in a Web conversation document.
// A stable snapshot or a poll timeout is never terminal evidence.
type Search struct {
	Completed  bool   `json:"completed"`
	Truncated  bool   `json:"truncated"`
	Reason     Reason `json:"reason,omitempty"`
	Terminal   string `json:"terminal,omitempty"`
	TextBytes  int    `json:"text_bytes"`
	TextSHA256 string `json:"text_sha256,omitempty"`
}

func (s Search) matches(text string) bool {
	return utf8.ValidString(text) && len(text) > 0 && len(text) <= MaxTextBytes && s.TextBytes == len(text) && s.TextSHA256 == Hash(text)
}

func (s Search) Valid(text string) bool {
	return s.Completed && !s.Truncated && s.Reason == Stop && (s.Terminal == "stop" || s.Terminal == "completed" || s.Terminal == "finished_successfully") && s.matches(text)
}

func (s Search) Incomplete(text string) bool {
	return !s.Completed && s.Truncated && s.Reason == ModelLength && (s.Terminal == "max_tokens" || s.Terminal == "length") && s.matches(text)
}
