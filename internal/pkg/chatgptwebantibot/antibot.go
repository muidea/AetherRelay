// Package antibot holds pure algorithms for ChatGPT Web anti-bot challenges:
// PoW, sentinel tokens, and turnstile solving.
//
// The ChatGPT requirements-token PoW port lives here.  It is intentionally
// independent of HTTP clients and framework lifecycle code.
package antibot

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/crypto/sha3"
)

const maxProofAttempts = 500_000

// ProofToken is the gAAAAAB... proof-of-work token used by chat-requirements.
type ProofToken string

// RequirementsSeed holds inputs for building chat requirements tokens.
type RequirementsSeed struct {
	Seed       string
	Difficulty string
	// Config is the browser fingerprint config array used by the PoW solver.
	Config []any
}

// BuildLegacyRequirementsToken builds the prepare-phase p token from a browser
// fingerprint configuration. The caller owns construction of that config,
// because its random browser fields are part of the upstream client boundary.
func BuildLegacyRequirementsToken(config []any) (string, error) {
	if len(config) == 0 {
		return "", errors.New("requirements config is empty")
	}
	payload, err := marshalConfig(config)
	if err != nil {
		return "", fmt.Errorf("marshal requirements config: %w", err)
	}
	return "gAAAAAC" + base64.StdEncoding.EncodeToString(payload), nil
}

// BuildProofToken solves the PoW challenge.
func BuildProofToken(seed RequirementsSeed) (ProofToken, error) {
	if seed.Seed == "" {
		return "", errors.New("proof seed is empty")
	}
	if len(seed.Config) < 11 {
		return "", errors.New("proof config must contain at least 11 fields")
	}
	target, err := hex.DecodeString(seed.Difficulty)
	if err != nil || len(target) == 0 {
		return "", fmt.Errorf("invalid proof difficulty %q", seed.Difficulty)
	}

	// The upstream JavaScript replaces config[3] with the attempt counter and
	// config[9] with half that counter. Build the three stable JSON fragments
	// once, exactly as Python's utils/pow.py does, then hash every candidate.
	first, err := marshalConfig(seed.Config[:3])
	if err != nil {
		return "", fmt.Errorf("marshal proof config prefix: %w", err)
	}
	middle, err := marshalConfig(seed.Config[4:9])
	if err != nil {
		return "", fmt.Errorf("marshal proof config middle: %w", err)
	}
	last, err := marshalConfig(seed.Config[10:])
	if err != nil {
		return "", fmt.Errorf("marshal proof config suffix: %w", err)
	}
	static1 := append(append([]byte{}, first[:len(first)-1]...), ',')
	static2 := append([]byte{','}, middle[1:len(middle)-1]...)
	static2 = append(static2, ',')
	static3 := append([]byte{','}, last[1:]...)

	for attempt := 0; attempt < maxProofAttempts; attempt++ {
		candidate := make([]byte, 0, len(static1)+len(static2)+len(static3)+32)
		candidate = append(candidate, static1...)
		candidate = append(candidate, fmt.Appendf(nil, "%d", attempt)...)
		candidate = append(candidate, static2...)
		candidate = append(candidate, fmt.Appendf(nil, "%d", attempt>>1)...)
		candidate = append(candidate, static3...)
		encoded := base64.StdEncoding.EncodeToString(candidate)
		digest := sha3.Sum512(append([]byte(seed.Seed), encoded...))
		if lessOrEqual(digest[:len(target)], target) {
			return ProofToken("gAAAAAB" + encoded), nil
		}
	}
	return "", fmt.Errorf("failed to solve proof token: difficulty=%s", seed.Difficulty)
}

func marshalConfig(config []any) ([]byte, error) {
	return json.Marshal(config)
}

func lessOrEqual(got, target []byte) bool {
	for i := range target {
		if got[i] < target[i] {
			return true
		}
		if got[i] > target[i] {
			return false
		}
	}
	return true
}
