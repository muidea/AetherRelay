package codexresponses

import "encoding/json"

// Diagnostics contains only bounded diagnostic hints, never routing inputs.
type Diagnostics struct {
	RequestID            string
	RequestKind          string
	CompactionReason     string
	CompactionPhase      string
	ClientIdentityReason string
}

// ParseDiagnostics projects enums only; metadata may contain private prompts,
// identities, paths or arbitrary extension values that must not enter logs.
func ParseDiagnostics(metadata string) Diagnostics {
	var result Diagnostics
	if len(metadata) > 16<<10 {
		return result
	}
	var value struct {
		Kind       string `json:"request_kind"`
		Compaction struct {
			Reason string `json:"reason"`
			Phase  string `json:"phase"`
		} `json:"compaction"`
	}
	if json.Unmarshal([]byte(metadata), &value) != nil {
		return result
	}
	switch value.Kind {
	case "turn", "compaction":
		result.RequestKind = value.Kind
	}
	if ValidCompactionReason(value.Compaction.Reason) {
		result.CompactionReason = value.Compaction.Reason
	}
	if ValidCompactionPhase(value.Compaction.Phase) {
		result.CompactionPhase = value.Compaction.Phase
	}
	return result
}

// ValidCompactionReason and ValidCompactionPhase are the shared bounded enums
// used by both diagnostics and the CP-HDR-011 metadata projection.
func ValidCompactionReason(value string) bool {
	switch value {
	case "comp_hash_changed", "context_limit", "model_downshift", "user_requested":
		return true
	default:
		return false
	}
}

func ValidCompactionPhase(value string) bool {
	switch value {
	case "pre_turn", "post_turn", "mid_turn":
		return true
	default:
		return false
	}
}
