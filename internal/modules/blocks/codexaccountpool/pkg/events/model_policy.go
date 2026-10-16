package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ModelBlacklist distinguishes an omitted policy (nil) from an explicit clear
// (non-nil empty slice). JSON null is rejected, including on credential imports.
type ModelBlacklist []string

// MarshalJSON emits an explicit empty policy in complete bundle exports.
func (m ModelBlacklist) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]string(m))
}

func (m *ModelBlacklist) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("blocked_models must be an array, not null")
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("blocked_models must be an array of model IDs: %w", err)
	}
	clean, err := NormalizeBlockedModels(values)
	if err == nil {
		*m = clean
	}
	return err
}

func NormalizeBlockedModels(values []string) (ModelBlacklist, error) {
	if values == nil {
		return nil, nil
	}
	if len(values) > 256 {
		return nil, fmt.Errorf("blocked_models must contain at most 256 model IDs")
	}
	seen := make(map[string]bool, len(values))
	clean := make(ModelBlacklist, 0, len(values))
	for _, value := range values {
		model := strings.TrimSpace(value)
		if model == "" || len(model) > 256 {
			return nil, fmt.Errorf("blocked_models IDs must contain 1 to 256 bytes")
		}
		for _, r := range model {
			if r < 0x20 || r == 0x7f {
				return nil, fmt.Errorf("blocked_models IDs must not contain control characters")
			}
		}
		if !seen[model] {
			seen[model] = true
			clean = append(clean, model)
		}
	}
	sort.Strings(clean)
	return clean, nil
}
