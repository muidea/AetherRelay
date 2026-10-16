package aetherrelaycodex

import "testing"

func TestObservationFeaturesAreExact(t *testing.T) {
	for typ, want := range map[string]string{
		"response.metadata": "response_metadata", "codex.response.metadata": "response_metadata",
		"codex.rate_limits": "codex_rate_limits", "responsesapi.websocket_timing": "websocket_timing",
		"responsesapi.future_business": "", "response.metadata.delta": "", "response.completed": "",
		"response.output_text.delta": "", "response.failed": "", "": "",
	} {
		if got := ObservationFeature(typ); got != want {
			t.Fatalf("%q: got %q want %q", typ, got, want)
		}
		if want != "" && MeaningfulOutputEvent([]byte(`{"type":"`+typ+`","delta":"observational","usage":{"output_tokens":99}}`)) {
			t.Fatalf("%q established output evidence", typ)
		}
	}
}
