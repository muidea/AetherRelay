// Package assets supplies the verified, embedded upstream reference bank.
package assets

import (
	"aetherrelay/internal/pkg/modeltrace"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const Commit = "6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2"
const BankHash = "f267d0513aa06981d024ec5394746fceddf44e4e8a75fa6bf8304255f4b32d6d"
const CoreHash = "83fa5bd611e18f8339122582335123c8ea168ed242298bb31f4e363abeeb6e4a"
const ParserHash = "f35e72b5fe82ce5037980d890fbdcfb3979ee31357f195f5fafc1e099a02c5d6"
const GuardVersion = "0.1.7"
const ProvenanceHash = "c606a533e2f3420c94dd1733b7df119c3470b7ffffc89a7c76aa3d6dfd6c16ec"
const LicenseHash = "238afb57e498990742f84b9ce6167b4e256ac06104a9938ce953bc41c14bea51"

// Only reference data and its provenance/license are linked into consumers.
//
//go:embed vendor/modeltrace/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/unified_bank.json vendor/modeltrace/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/provenance.json vendor/modeltrace/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/LICENSE
var reference embed.FS

// Load constructs an immutable Engine without accessing the working directory.
func Load() (*modeltrace.Engine, error) {
	base := "vendor/modeltrace/" + Commit + "/"
	required := map[string]string{"unified_bank.json": BankHash, "provenance.json": ProvenanceHash, "LICENSE": LicenseHash}
	var bank []byte
	for name, want := range required {
		data, err := reference.ReadFile(base + name)
		if err != nil {
			return nil, fmt.Errorf("%w: embedded asset missing", modeltrace.ErrAssetInvalid)
		}
		if referenceDigest(data) != want {
			return nil, fmt.Errorf("%w: embedded asset digest mismatch", modeltrace.ErrAssetInvalid)
		}
		if name == "unified_bank.json" {
			bank = data
		}
		if name == "provenance.json" {
			if err := validateProvenance(data); err != nil {
				return nil, err
			}
		}
	}
	return modeltrace.New(bank)
}

func validateProvenance(data []byte) error {
	var p struct {
		Bank  string `json:"bankSha256"`
		Core  string `json:"scorerSha256"`
		Count int    `json:"modelCount"`
	}
	if json.Unmarshal(data, &p) != nil || p.Bank != BankHash || p.Core != CoreHash || p.Count != 18 {
		return fmt.Errorf("%w: embedded provenance mismatch", modeltrace.ErrAssetInvalid)
	}
	return nil
}

func referenceDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
