package antibot

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildLegacyRequirementsToken(t *testing.T) {
	config := []any{"screen", "timestamp", 1, 2}
	token, err := BuildLegacyRequirementsToken(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "gAAAAAC") {
		t.Fatalf("unexpected prefix: %q", token)
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(token, "gAAAAAC"))
	if err != nil {
		t.Fatal(err)
	}
	var got []any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(config) || got[0] != "screen" || got[2] != float64(1) {
		t.Fatalf("unexpected round trip: %#v", got)
	}
}

func TestBuildProofToken(t *testing.T) {
	seed := RequirementsSeed{
		Seed:       "proof-seed",
		Difficulty: "ff", // the first candidate must pass, making the test deterministic.
		Config: []any{
			1920, "time", 4294705152, 1, "agent", "sdk", "build", "en-US", "accept", 0.5, "navigator", "document", "window",
		},
	}
	token, err := BuildProofToken(seed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(token), "gAAAAAB") {
		t.Fatalf("unexpected prefix: %q", token)
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(string(token), "gAAAAAB"))
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `[1920,"time",4294705152,0,"agent","sdk","build","en-US","accept",0,"navigator","document","window"]` {
		t.Fatalf("unexpected candidate: %s", payload)
	}
}

func TestBuildProofTokenRejectsInvalidInput(t *testing.T) {
	if _, err := BuildProofToken(RequirementsSeed{Seed: "s", Difficulty: "zz", Config: make([]any, 11)}); err == nil {
		t.Fatal("expected invalid difficulty error")
	}
}

func TestSolveTurnstileToken(t *testing.T) {
	// The compact program mirrors the upstream bytecode shape: store a value,
	// base64-encode it, decode it again, then invoke func_3 through func_7.
	program, err := json.Marshal([][]any{
		{2, 30, "hello"},
		{19, 31, 30},
		{18, 32, 31},
		{7, 3, 32},
	})
	if err != nil {
		t.Fatal(err)
	}
	dx := base64.StdEncoding.EncodeToString([]byte(xorString(string(program), "key")))
	token, err := SolveTurnstileToken(dx, "key")
	if err != nil {
		t.Fatal(err)
	}
	if token != "aGVsbG8=" {
		t.Fatalf("token = %q, want base64(hello)", token)
	}
}

func TestSolveTurnstileTokenRejectsMalformedProgram(t *testing.T) {
	if _, err := SolveTurnstileToken("not-base64", "key"); err == nil {
		t.Fatal("expected malformed program error")
	}
}
