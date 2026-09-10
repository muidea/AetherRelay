package aetherrelaycredential

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func testKey(fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, keyBytes))
}

func TestCodecRoundTripAndBinding(t *testing.T) {
	codec, err := New(testKey(7))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := codec.Seal("providers", "openai", []byte(`{"api_key":"secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "secret") {
		t.Fatalf("encrypted envelope leaked plaintext: %s", sealed)
	}
	plain, err := codec.Open("providers", "openai", sealed)
	if err != nil || string(plain) != `{"api_key":"secret"}` {
		t.Fatalf("open=%q err=%v", plain, err)
	}
	if _, err := codec.Open("providers", "other", sealed); err == nil {
		t.Fatal("credential envelope was not bound to its record identity")
	}
}

func TestCodecRejectsInvalidKeyAndPlaintext(t *testing.T) {
	if _, err := New("short"); err == nil {
		t.Fatal("invalid key accepted")
	}
	codec, err := New(testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Open("scope", "id", []byte("plaintext")); err == nil {
		t.Fatal("plaintext credential accepted")
	}
}

func TestDeriveKeyIsStableAndPurposeBound(t *testing.T) {
	root := testKey(9)
	first, err := DeriveKey(root, "image-url-signing")
	if err != nil {
		t.Fatal(err)
	}
	again, err := DeriveKey(root, "image-url-signing")
	if err != nil || first != again {
		t.Fatalf("stable derivation failed: equal=%t err=%v", first == again, err)
	}
	other, err := DeriveKey(root, "another-purpose")
	if err != nil || first == other {
		t.Fatalf("purpose separation failed: equal=%t err=%v", first == other, err)
	}
	if _, err := DeriveKey(root, " "); err == nil {
		t.Fatal("empty purpose was accepted")
	}
}
