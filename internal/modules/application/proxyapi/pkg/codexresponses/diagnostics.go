package codexresponses

import "encoding/json"

// Diagnostics contains only bounded diagnostic hints, never routing inputs.
type Diagnostics struct {
	RequestID        string
	RequestKind      string
	CompactionReason string
	CompactionPhase  string
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
	switch value.Compaction.Reason {
	case "comp_hash_changed", "context_limit", "model_downshift", "user_requested":
		result.CompactionReason = value.Compaction.Reason
	}
	switch value.Compaction.Phase {
	case "pre_turn", "post_turn", "mid_turn":
		result.CompactionPhase = value.Compaction.Phase
	}
	return result
}
