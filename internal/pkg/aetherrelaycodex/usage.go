package aetherrelaycodex

// UsageLimitGroup is an upstream quota group's status, independent of any
// primary/secondary window's utilization. Its freshness belongs to the full
// usage observation and cannot be renewed by partial response headers.
type UsageLimitGroup struct {
	ID         string `json:"id"`
	Scope      string `json:"scope"`
	Model      string `json:"model,omitempty"`
	Source     string `json:"source,omitempty"`
	ObservedAt string `json:"observed_at,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	// ExhaustedWindow* and ResetAt preserve the explanation from the same full
	// response. Partial headers may replace a child, but cannot rewrite this fact.
	ExhaustedWindowID      string `json:"exhausted_window_id,omitempty"`
	ExhaustedWindowSeconds int    `json:"exhausted_window_seconds,omitempty"`
	ResetAt                string `json:"reset_at,omitempty"`
	Allowed                bool   `json:"allowed"`
	AllowedKnown           bool   `json:"allowed_known"`
	LimitReached           bool   `json:"limit_reached"`
	LimitReachedKnown      bool   `json:"limit_reached_known"`
}
