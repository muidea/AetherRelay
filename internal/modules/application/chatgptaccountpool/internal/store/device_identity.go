package store

import "github.com/google/uuid"

// A missing/empty legacy value is initialized once by saveEncryptedLocked.
// A malformed value is preserved for diagnosis, never silently rotated.
func deviceIdentityError(acc *Account) string {
	if raw, present := acc.Extra["web_device_id"]; present {
		if _, ok := raw.(string); !ok {
			return "invalid_device_identity"
		}
	}
	if acc.DeviceID == "" {
		return ""
	}
	id, err := uuid.Parse(acc.DeviceID)
	if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || id.String() != acc.DeviceID {
		return "invalid_device_identity"
	}
	return ""
}

// Keep persisted operator state separate from the derived execution block so
// a credential/status update cannot bypass a malformed device identity.
func accountStatus(acc *Account) string {
	if acc.Status != StatusDisabled && deviceIdentityError(acc) != "" {
		return StatusAbnormal
	}
	return acc.Status
}
