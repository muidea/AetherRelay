package store

import (
	"strings"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

func modelBlocked(item *account, model string) bool {
	if item == nil {
		return false
	}
	model = strings.TrimSpace(model)
	for _, blocked := range item.BlockedModels {
		if blocked == model {
			return true
		}
	}
	return false
}

// CheckModelPolicy authorizes the next attempt/turn of an existing lease. Quota
// and concurrency were admitted with the lease and are not charged again.
func (s *Store) CheckModelPolicy(id, model string) events.CheckModelPolicyResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[strings.TrimSpace(id)]
	switch {
	case item == nil:
		return events.CheckModelPolicyResult{Reason: "account_missing"}
	case item.Status == events.StatusDisabled:
		return events.CheckModelPolicyResult{Reason: "account_disabled"}
	case modelBlocked(item, model):
		return events.CheckModelPolicyResult{Reason: "model_blocked"}
	default:
		return events.CheckModelPolicyResult{Allowed: true}
	}
}
