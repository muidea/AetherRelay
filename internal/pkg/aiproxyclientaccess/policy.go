package clientaccess

import (
	"fmt"
	"sort"
	"strings"
)

type Mode string

const (
	ModeAll      Mode = "all"
	ModeSelected Mode = "selected"
)

type Policy struct {
	Mode        Mode
	ProviderIDs []string
}

func All() Policy { return Policy{Mode: ModeAll} }

func Selected(providerIDs []string) (Policy, error) {
	return Normalize(Policy{Mode: ModeSelected, ProviderIDs: providerIDs})
}

func Normalize(policy Policy) (Policy, error) {
	mode := Mode(strings.ToLower(strings.TrimSpace(string(policy.Mode))))
	switch mode {
	case ModeAll:
		if len(policy.ProviderIDs) != 0 {
			return Policy{}, fmt.Errorf("provider_ids must be empty when mode is all")
		}
		return All(), nil
	case ModeSelected:
		seen := make(map[string]struct{}, len(policy.ProviderIDs))
		ids := make([]string, 0, len(policy.ProviderIDs))
		for _, raw := range policy.ProviderIDs {
			id := strings.ToLower(strings.TrimSpace(raw))
			if id == "" || strings.ContainsAny(id, "/\\") {
				return Policy{}, fmt.Errorf("invalid provider id %q", raw)
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return Policy{}, fmt.Errorf("provider_ids is required when mode is selected")
		}
		sort.Strings(ids)
		return Policy{Mode: ModeSelected, ProviderIDs: ids}, nil
	default:
		return Policy{}, fmt.Errorf("invalid provider access mode %q", policy.Mode)
	}
}

func Clone(policy Policy) Policy {
	return Policy{Mode: policy.Mode, ProviderIDs: append([]string(nil), policy.ProviderIDs...)}
}

func (p Policy) Allows(providerID string) bool {
	switch p.Mode {
	case ModeAll:
		return true
	case ModeSelected:
		id := strings.ToLower(strings.TrimSpace(providerID))
		index := sort.SearchStrings(p.ProviderIDs, id)
		return index < len(p.ProviderIDs) && p.ProviderIDs[index] == id
	default:
		return false
	}
}
