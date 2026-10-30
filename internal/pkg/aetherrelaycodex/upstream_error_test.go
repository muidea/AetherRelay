package aetherrelaycodex

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestUpstreamErrorBoundsRetainExistingErrorPolicy(t *testing.T) {
	raw := UpstreamError{UpstreamErrorType: strings.Repeat("t", 100), UpstreamErrorCode: strings.Repeat("c", 120), UpstreamErrorParam: "input[0].text", UpstreamErrorMessage: strings.Repeat("中", 180) + "\nend"}
	got := raw.Bounded()
	if len(got.UpstreamErrorType) != 64 || len(got.UpstreamErrorCode) != 96 || got.UpstreamErrorParam != raw.UpstreamErrorParam || len(got.UpstreamErrorMessage) > 512 || !utf8.ValidString(got.UpstreamErrorMessage) {
		t.Fatalf("invalid bounds: %+v", got)
	}
	if got := (UpstreamError{UpstreamErrorCode: "bad\ncode", UpstreamErrorMessage: "line1\nline2"}).Bounded(); got.UpstreamErrorCode != "" || got.UpstreamErrorMessage != "line1 line2" {
		t.Fatal(got)
	}
	for _, marker := range []string{"Bearer secret", "access_token=secret", "Cookie: secret", "sk-secret"} {
		if got := (UpstreamError{UpstreamErrorMessage: marker}).Bounded(); got.UpstreamErrorMessage != "" {
			t.Fatal("changed existing sensitive error policy", got)
		}
	}
}
