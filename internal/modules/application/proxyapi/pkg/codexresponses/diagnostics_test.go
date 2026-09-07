package codexresponses

import (
	"strings"
	"testing"
)

func TestDiagnosticsWhitelist(t *testing.T) {
	// CP-OBS-006: no private metadata values or invented UI model.
	got := ParseDiagnostics(`{"request_kind":"compaction","session_id":"secret","model":"private","compaction":{"reason":"comp_hash_changed","phase":"pre_turn"}}`)
	if got.RequestKind != "compaction" || got.CompactionReason != "comp_hash_changed" || got.CompactionPhase != "pre_turn" || got.RequestID != "" {
		t.Fatalf("%+v", got)
	}
	for _, raw := range []string{`{"request_kind":"secret","compaction":{"reason":"secret","phase":"secret"}}`, `{"request_kind":17}`, strings.Repeat("x", 20000), `{`} {
		if got := ParseDiagnostics(raw); got != (Diagnostics{}) {
			t.Fatalf("unsafe diagnostic: %+v", got)
		}
	}
}
