package store

import (
	"sort"
	"strings"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

// Each window carries its own observation interval. A partial response cannot
// make an unobserved window fresh again, including windows loaded from old data.
func normalizeUsageSnapshot(snapshot events.AccountUsageSnapshot) events.AccountUsageSnapshot {
	out := events.AccountUsageSnapshot{
		PlanType: bounded(snapshot.PlanType, 64), Complete: snapshot.Complete,
		StartedAt:  normalizeUsageTime(snapshot.StartedAt, time.Time{}),
		ObservedAt: normalizeUsageTime(snapshot.ObservedAt, time.Now().UTC()),
		ExpiresAt:  normalizeUsageTime(snapshot.ExpiresAt, time.Time{}),
	}
	observed := usageObservationStart(out)
	byID := make(map[string]events.UsageWindow, len(snapshot.Windows))
	for _, window := range snapshot.Windows {
		window.ID = bounded(window.ID, 96)
		if window.ID == "" {
			continue
		}
		if window.ID == "header-primary" || window.ID == "header-secondary" {
			window.ID = strings.Replace(window.ID, "header-", "standard-", 1)
			if window.Source == "" {
				window.Source = "response_header"
			}
		}
		window.Label = bounded(window.Label, 128)
		window.Model = bounded(window.Model, 128)
		window.Source = bounded(window.Source, 32)
		window.Scope = bounded(window.Scope, 32)
		if window.Scope == "" {
			switch {
			case strings.HasPrefix(window.ID, "standard-"), window.ID == "primary", window.ID == "secondary":
				window.Scope = "standard"
			case strings.HasPrefix(window.ID, "code-review-"):
				window.Scope = "code-review"
			case strings.HasPrefix(window.ID, "additional-"):
				window.Scope = "additional"
			default:
				window.Scope = "unknown"
			}
		}
		window.ObservedAt = normalizeUsageTime(window.ObservedAt, observed)
		window.ExpiresAt = normalizeUsageTime(window.ExpiresAt, time.Time{})
		if window.ExpiresAt == "" {
			window.ExpiresAt = out.ExpiresAt
		}
		if window.UsedPercentKnown {
			if window.UsedPercent != window.UsedPercent {
				window.UsedPercent, window.UsedPercentKnown = 0, false
			} else {
				window.UsedPercent = min(100, max(0, window.UsedPercent))
				if window.UsedPercent >= 100 {
					window.LimitReached, window.LimitReachedKnown = true, true
				}
			}
		} else {
			window.UsedPercent = 0
		}
		if window.WindowSeconds < 0 || window.WindowSeconds > 366*24*60*60 {
			window.WindowSeconds = 0
		}
		window.ResetAt = normalizeUsageTime(window.ResetAt, time.Time{})
		if previous, ok := byID[window.ID]; !ok || newerUsageWindow(window, previous) {
			byID[window.ID] = window
		}
	}
	for _, window := range byID {
		out.Windows = append(out.Windows, window)
	}
	sort.Slice(out.Windows, func(i, j int) bool { return out.Windows[i].ID < out.Windows[j].ID })
	return out
}

func usageObservationStart(snapshot events.AccountUsageSnapshot) time.Time {
	if started, ok := parseExpiry(snapshot.StartedAt); ok {
		return started
	}
	observed, _ := parseExpiry(snapshot.ObservedAt)
	return observed
}

func prepareUsageSnapshot(snapshot events.AccountUsageSnapshot, source string) events.AccountUsageSnapshot {
	// Observation metadata belongs to the incoming request, not to caller-
	// supplied per-window timestamps or a composite snapshot.
	snapshot.Windows = append([]events.UsageWindow(nil), snapshot.Windows...)
	for i := range snapshot.Windows {
		snapshot.Windows[i].Source = strings.TrimSpace(source)
		snapshot.Windows[i].ObservedAt = ""
		snapshot.Windows[i].ExpiresAt = ""
	}
	return normalizeUsageSnapshot(snapshot)
}

func newerUsageWindow(candidate, previous events.UsageWindow) bool {
	// Reset/duration metadata alone does not supersede known quota status, nor
	// can it extend the lifetime of that status by borrowing a fresh timestamp.
	if !hasUsageStatus(candidate) && hasUsageStatus(previous) {
		return false
	}
	a, _ := parseExpiry(candidate.ObservedAt)
	b, _ := parseExpiry(previous.ObservedAt)
	if !a.Equal(b) {
		return a.After(b)
	}
	// Older snapshots stamped both duplicate header and endpoint IDs with one
	// time. In that legacy tie, prefer the endpoint's canonical window.
	return previous.Source == "response_header" && candidate.Source != "response_header"
}

func hasUsageStatus(window events.UsageWindow) bool {
	return window.UsedPercentKnown || window.AllowedKnown || window.LimitReachedKnown || window.LimitReached
}

func mergeUsageObservations(previous *events.AccountUsageSnapshot, incoming events.AccountUsageSnapshot) *events.AccountUsageSnapshot {
	if previous == nil {
		return &incoming
	}
	existing := normalizeUsageSnapshot(*previous)
	byID := make(map[string]int, len(existing.Windows))
	for i, window := range existing.Windows {
		byID[window.ID] = i
	}
	for _, window := range incoming.Windows {
		if i, ok := byID[window.ID]; ok {
			if newerUsageWindow(window, existing.Windows[i]) {
				existing.Windows[i] = window
			}
		} else {
			byID[window.ID] = len(existing.Windows)
			existing.Windows = append(existing.Windows, window)
		}
	}
	if usageObservationStart(incoming).After(usageObservationStart(existing)) {
		existing.StartedAt = incoming.StartedAt
		existing.ObservedAt = incoming.ObservedAt
		existing.ExpiresAt = incoming.ExpiresAt
		existing.Complete = false // A union of observations is not a full response.
		if incoming.PlanType != "" {
			existing.PlanType = incoming.PlanType
		}
	}
	clean := normalizeUsageSnapshot(existing)
	return &clean
}

func usageWindowApplies(window events.UsageWindow, model string) bool {
	switch window.Scope {
	case "standard":
		return true
	case "code-review":
		return model == "codex-auto-review"
	case "additional":
		return window.Model != "" && window.Model == model
	default:
		return false
	}
}

func freshUsageSnapshot(item *account, now time.Time) (events.AccountUsageSnapshot, bool) {
	if item == nil || item.UsageSnapshot == nil {
		return events.AccountUsageSnapshot{}, false
	}
	snapshot := normalizeUsageSnapshot(*item.UsageSnapshot)
	fresh := snapshot.Windows[:0]
	for _, window := range snapshot.Windows {
		if expiry, ok := parseExpiry(window.ExpiresAt); ok && expiry.After(now) {
			fresh = append(fresh, window)
		}
	}
	snapshot.Windows = fresh
	return snapshot, len(fresh) > 0
}

func usageWindowBlocks(window events.UsageWindow, now time.Time) bool {
	return window.LimitReached && (window.ResetAt == "" || resetAfter(window.ResetAt, now))
}

func usageLimitCooling(item *account, now time.Time, models ...string) bool {
	_, blocked := usageLimitResetAt(item, now, models...)
	return blocked
}

// Retry hints use the same scope and window expiry as actual admission.
func usageLimitResetAt(item *account, now time.Time, models ...string) (time.Time, bool) {
	snapshot, _ := freshUsageSnapshot(item, now)
	model := ""
	if len(models) > 0 {
		model = models[0]
	}
	var latest time.Time
	for _, window := range snapshot.Windows {
		if !usageWindowApplies(window, model) || !usageWindowBlocks(window, now) {
			continue
		}
		expires, _ := parseExpiry(window.ExpiresAt)
		reset, ok := parseExpiry(window.ResetAt)
		if !ok || reset.After(expires) {
			reset = expires
		}
		if reset.After(latest) {
			latest = reset
		}
	}
	return latest, !latest.IsZero()
}

func windowHasHeadroom(window events.UsageWindow) bool {
	if window.LimitReached || window.AllowedKnown && !window.Allowed {
		return false
	}
	return window.UsedPercentKnown && window.UsedPercent < 100 || window.AllowedKnown && window.Allowed || window.LimitReachedKnown
}

func freshQuotaHeadroom(item *account, now time.Time, model string) bool {
	snapshot, _ := freshUsageSnapshot(item, now)
	positive := false
	for _, window := range snapshot.Windows {
		if !usageWindowApplies(window, model) {
			continue
		}
		if usageWindowBlocks(window, now) {
			return false
		}
		positive = positive || windowHasHeadroom(window)
	}
	return positive
}

// Old data can be identified as quota-derived only by matching the scoped
// exhausted observation and its reset. Unknown rate limits stay untouched.
func legacyQuotaCooldown(item *account, model string, entry cooldown) bool {
	observation, ok := item.QuotaObservations[model]
	reset, valid := parseExpiry(observation.ResetAt)
	_, observed := parseExpiry(observation.ObservedAt)
	return entry.Source == "" && entry.ErrorClass == events.ErrorRateLimit && ok && observation.State == "exhausted" && observed && valid && reset.Equal(entry.Until)
}

func reconcileQuotaRecovery(item *account, incoming events.AccountUsageSnapshot, now time.Time) {
	started, valid := parseExpiry(incoming.StartedAt)
	finished, completeTime := parseExpiry(incoming.ObservedAt)
	if !incoming.Complete || !valid || !completeTime || finished.Before(started) || finished.After(now) || usageLimitCooling(item, now) {
		return
	}
	// Ordinary primary AND secondary windows must be present and explicitly
	// healthy in this one full response; an empty/partial 200 is not recovery.
	healthy := map[string]bool{}
	for _, window := range incoming.Windows {
		if window.Scope == "standard" {
			expires, ok := parseExpiry(window.ExpiresAt)
			if !ok || !expires.After(now) || !windowHasHeadroom(window) {
				return
			}
			healthy[window.ID] = true
		}
		if (window.Scope == "unknown" || window.Scope == "additional" && window.Model == "") && (window.LimitReached || window.AllowedKnown && !window.Allowed) {
			return // No proof that an unscoped restriction recovered.
		}
	}
	if !healthy["standard-primary"] || !healthy["standard-secondary"] {
		return
	}
	for model, observation := range item.QuotaObservations {
		observed, ok := parseExpiry(observation.ObservedAt)
		if ok && !strings.Contains(observation.ObservedAt, ".") {
			// Legacy timestamps had only second precision. Require a request
			// beginning beyond that whole second before treating it as newer.
			observed = observed.Add(time.Second)
		}
		if observation.State != "exhausted" || !ok || !started.After(observed) {
			continue
		}
		// Do not reconcile model-local evidence from ordinary account windows.
		if model != "" {
			continue
		}
		entry, cooling := item.Cooldowns[model]
		if cooling {
			quotaDerived := entry.Source == "usage_limit" || legacyQuotaCooldown(item, model, entry)
			entryObserved, _ := parseExpiry(entry.ObservedAt)
			if quotaDerived && (entryObserved.IsZero() || started.After(entryObserved)) {
				delete(item.Cooldowns, model)
			}
		}
		delete(item.QuotaObservations, model)
	}
}
