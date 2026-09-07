package store

import (
	"strings"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

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
