package store

import (
	"math"
	"strings"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

// unavailableResult is called under the store lock after selection failed.
// CP-FAIL-019: derive retry hints from the same exact-model admission facts,
// without exposing account identities or changing any cooldown.
func (s *Store) unavailableResult(model string, excluded map[string]struct{}, transport string, now time.Time) events.AcquireResult {
	result := events.AcquireResult{UnavailableReason: "no_eligible_account"}
	var earliest time.Time
	for _, item := range s.items {
		if transportSupport(item, transport) < 0 {
			continue
		}
		view := modelAvailability(item, model, now)
		if _, found := excluded[item.ID]; found {
			if view.Available {
				result.UnavailableReason = "accounts_busy_or_excluded"
			}
			continue
		}
		if view.Until == "" || usageLimitCooling(item, now) {
			continue
		}
		// Keep sub-second precision; management's RFC3339 projection truncates it.
		var until time.Time
		for key, entry := range item.Cooldowns {
			if (key == "" || key == strings.TrimSpace(model)) && entry.Until.After(until) {
				until = entry.Until
			}
		}
		if until.After(now) && (earliest.IsZero() || until.Before(earliest)) {
			earliest = until
		}
	}
	if !earliest.IsZero() {
		result.UnavailableReason = "accounts_cooling"
		result.RetryAfterSeconds = int(math.Ceil(earliest.Sub(now).Seconds()))
	}
	return result
}

func modelAvailability(item *account, model string, now time.Time) events.ModelAvailabilityView {
	// CP-SCHED-009: shared admission for management and every account selection.
	model = strings.TrimSpace(model)
	view := events.ModelAvailabilityView{Model: model}
	switch {
	case item == nil:
		view.Reason = "account_missing"
	case item.Status != events.StatusNormal:
		view.Reason = "account_" + item.Status
	case strings.TrimSpace(item.AccessToken) == "":
		view.Reason = "credential_missing"
	case snapshotExpired(item.ModelSnapshot, now):
		view.Reason = "snapshot_expired"
	case !accountSupportsModel(item, model, now):
		view.Reason = "model_not_supported"
	default:
		var until time.Time
		for key, entry := range item.Cooldowns {
			if (key == "" || key == model) && entry.Until.After(now) && entry.Until.After(until) {
				until = entry.Until
				view.Reason = entry.ErrorClass
				view.Until = until.Format(time.RFC3339)
			}
		}
		if view.Reason == "" && usageLimitCooling(item, now) {
			view.Reason = "usage_limit"
		}
		view.Available = view.Reason == ""
	}
	return view
}
