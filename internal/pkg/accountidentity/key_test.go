package accountidentity

import "testing"

func TestKeyPrefersAccountIDAndNormalizesEmailFallback(t *testing.T) {
	if Key("account-1", "first@example.com") != Key(" account-1 ", "second@example.com") {
		t.Fatal("account ID must be the authoritative identity input")
	}
	if Key("", " User@Example.com ") != Key("", "user@example.com") {
		t.Fatal("email fallback must be normalized")
	}
	if Key("", "") != "" {
		t.Fatal("empty identity inputs must stay empty")
	}
}
