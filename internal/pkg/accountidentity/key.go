// Package accountidentity provides an owner-neutral opaque identity key used
// to join credential-slot projections without exposing upstream account IDs.
package accountidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Key returns a stable, non-reversible local projection. Upstream account ID
// is preferred; normalized email is only a fallback for incomplete imports.
func Key(accountID, email string) string {
	value := strings.TrimSpace(accountID)
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(email))
	}
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return "acct_" + hex.EncodeToString(sum[:12])
}
