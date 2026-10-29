package store

import (
	"sort"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"aetherrelay/internal/pkg/aetherrelaycodex"
)

func normalizeUsageGroups(groups []aetherrelaycodex.UsageLimitGroup, snapshot events.AccountUsageSnapshot) []aetherrelaycodex.UsageLimitGroup {
	byID := map[string]aetherrelaycodex.UsageLimitGroup{}
	for _, group := range groups {
		group.ID = bounded(group.ID, 96)
		if group.ID == "" {
			continue
		}
		group.Scope, group.Model, group.Source = bounded(group.Scope, 32), bounded(group.Model, 128), bounded(group.Source, 32)
		group.ObservedAt = normalizeUsageTime(group.ObservedAt, usageObservationStart(snapshot))
		group.ExpiresAt = normalizeUsageTime(group.ExpiresAt, time.Time{})
		if group.ExpiresAt == "" {
			group.ExpiresAt = snapshot.ExpiresAt
		}
		group.ExhaustedWindowID = bounded(group.ExhaustedWindowID, 96)
		group.ResetAt = normalizeUsageTime(group.ResetAt, time.Time{})
		if group.ExhaustedWindowSeconds < 0 || group.ExhaustedWindowSeconds > 366*24*60*60 {
			group.ExhaustedWindowSeconds = 0
		}
		if usageGroupDenied(group) && group.ExhaustedWindowID == "" {
			for _, window := range snapshot.Windows {
				if window.GroupID != group.ID || window.ObservedAt != group.ObservedAt || !window.LimitReachedKnown || !window.LimitReached {
					continue
				}
				// An unknown reset dominates finite resets: there is no proof that all
				// exhausted windows have reset. Otherwise keep the latest actual reset.
				reset, known := parseExpiry(window.ResetAt)
				latest, _ := parseExpiry(group.ResetAt)
				if group.ExhaustedWindowID == "" || !known || group.ResetAt != "" && reset.After(latest) {
					group.ExhaustedWindowID, group.ExhaustedWindowSeconds, group.ResetAt = window.ID, window.WindowSeconds, window.ResetAt
				}
			}
		}
		if previous, ok := byID[group.ID]; !ok || newerUsageGroup(group, previous) {
			byID[group.ID] = group
		}
	}
	result := make([]aetherrelaycodex.UsageLimitGroup, 0, len(byID))
	for _, group := range byID {
		result = append(result, group)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func newerUsageGroup(candidate, previous aetherrelaycodex.UsageLimitGroup) bool {
	if !candidate.AllowedKnown && !candidate.LimitReachedKnown && (previous.AllowedKnown || previous.LimitReachedKnown) {
		return false
	}
	a, _ := parseExpiry(candidate.ObservedAt)
	b, _ := parseExpiry(previous.ObservedAt)
	return a.After(b)
}

func usageGroupDenied(group aetherrelaycodex.UsageLimitGroup) bool {
	return group.LimitReachedKnown && group.LimitReached || group.AllowedKnown && !group.Allowed
}

func usageGroupBlocks(group aetherrelaycodex.UsageLimitGroup, now time.Time) bool {
	if !usageGroupDenied(group) {
		return false
	}
	// The explanation is captured with the group's original observation,
	// independently of subsequent partial updates to the child windows.
	if group.ExhaustedWindowID != "" {
		return group.ResetAt == "" || resetAfter(group.ResetAt, now)
	}
	return true // Unknown cause remains bounded by the group's own TTL.
}

// Management describes the actual exhausted window, not the cache expiry used
// for routing retry hints. No reset is invented for an unexplained group denial.
func usageLimitDetails(item *account, model string, now time.Time) events.ModelAvailabilityView {
	snapshot, _ := freshUsageSnapshot(item, now)
	detail := events.ModelAvailabilityView{}
	var latest time.Time
	for _, window := range snapshot.Windows {
		if !usageWindowApplies(window, model) || !usageWindowBlocks(window, now) || window.UsedPercentKnown && window.UsedPercent < 100 {
			continue
		}
		reset, _ := parseExpiry(window.ResetAt)
		if detail.UsageLimitWindow == "" || detail.UsageLimitResetAt != "" && (window.ResetAt == "" || reset.After(latest)) {
			latest = reset
			detail.UsageLimitScope, detail.UsageLimitWindow = window.Scope, window.ID
			detail.UsageLimitWindowSeconds, detail.UsageLimitResetAt = window.WindowSeconds, window.ResetAt
		}
	}
	for _, group := range snapshot.Groups {
		if !usageWindowApplies(events.UsageWindow{Scope: group.Scope, Model: group.Model}, model) || !usageGroupBlocks(group, now) {
			continue
		}
		if detail.UsageLimitScope == "" {
			detail.UsageLimitScope = group.Scope
		}
		reset, _ := parseExpiry(group.ResetAt)
		if group.ExhaustedWindowID != "" && (detail.UsageLimitWindow == "" || detail.UsageLimitResetAt != "" && (group.ResetAt == "" || reset.After(latest))) {
			latest = reset
			detail.UsageLimitScope, detail.UsageLimitWindow = group.Scope, group.ExhaustedWindowID
			detail.UsageLimitWindowSeconds, detail.UsageLimitResetAt = group.ExhaustedWindowSeconds, group.ResetAt
		}
	}
	return detail
}
