package proxy

import (
	"testing"

	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
)

func TestModelSupportedEndpointsUsesTransportMatrix(t *testing.T) {
	snap := effectivecatalog.Snapshot{Candidates: map[string][]effectivecatalog.Candidate{
		"claude": {
			{ModelID: "claude", RouteOwner: "anthropic", SupportedEndpoints: []string{"/v1/chat/completions", "/v1/messages"}},
			{ModelID: "claude", RouteOwner: "openai", SupportedEndpoints: []string{"/v1/responses"}},
		},
	}}
	got := modelSupportedEndpoints(snap, "claude")
	want := []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"}
	if len(got) != len(want) {
		t.Fatalf("supported endpoints=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("supported endpoints=%v, want %v", got, want)
		}
	}
}

func TestModelSupportedEndpointsIncludesChatGPTWebSearchAndImages(t *testing.T) {
	snap := effectivecatalog.Snapshot{Candidates: map[string][]effectivecatalog.Candidate{
		"gpt": {{ModelID: "gpt", RouteOwner: effectivecatalog.BuiltinProviderID, Builtin: true, SupportedEndpoints: []string{"/v1/chat/completions", "/v1/responses", "/v1/search", "/v1/images/generations", "/v1/images/edits"}}},
	}}
	got := modelSupportedEndpoints(snap, "gpt")
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/search", "/v1/images/generations", "/v1/images/edits"} {
		found := false
		for _, item := range got {
			if item == path {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("supported endpoints=%v missing %s", got, path)
		}
	}
}
