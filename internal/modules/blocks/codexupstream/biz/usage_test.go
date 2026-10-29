package biz

import (
	"testing"
	"time"
)

func TestUsageGroupRestrictionDoesNotContaminateWindows(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for _, body := range []string{
		`{"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},"secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_after_seconds":500000}}}`,
		`{"rateLimit":{"allowed":false,"limitReached":true,"primaryWindow":{"usedPercent":"0","limitWindowSeconds":18000,"resetAfterSeconds":18000},"secondaryWindow":{"usedPercent":"100","limitWindowSeconds":604800,"resetAfterSeconds":500000}}}`,
	} {
		_, windows, groups, err := parseUsage([]byte(body), now)
		if err != nil || len(windows) != 2 || len(groups) != 1 {
			t.Fatalf("windows=%+v groups=%+v err=%v", windows, groups, err)
		}
		group := groups[0]
		if !group.AllowedKnown || group.Allowed || !group.LimitReachedKnown || !group.LimitReached {
			t.Fatalf("lost group restriction: %+v", group)
		}
		primary, weekly := windows[0], windows[1]
		if primary.GroupID != group.ID || !primary.UsedPercentKnown || primary.UsedPercent != 0 || !primary.AllowedKnown || !primary.Allowed || !primary.LimitReachedKnown || primary.LimitReached {
			t.Fatalf("healthy 5-hour window contaminated: %+v", primary)
		}
		if weekly.GroupID != group.ID || !weekly.UsedPercentKnown || weekly.UsedPercent != 100 || weekly.Allowed || !weekly.LimitReached {
			t.Fatalf("weekly exhaustion lost: %+v", weekly)
		}
	}
}

func TestUsageUnknownPercentAndGroupsWithoutWindows(t *testing.T) {
	now := time.Now().UTC()
	for _, percent := range []string{"", `"used_percent":null,`, `"used_percent":"NaN",`, `"used_percent":101,`} {
		_, windows, groups, err := parseUsage([]byte(`{"rate_limit":{"allowed":false,"primary_window":{`+percent+`"reset_after_seconds":60}}}`), now)
		if err != nil || len(windows) != 1 || len(groups) != 1 {
			t.Fatalf("windows=%+v groups=%+v err=%v", windows, groups, err)
		}
		if window := windows[0]; window.UsedPercentKnown || window.AllowedKnown || window.LimitReachedKnown || window.LimitReached {
			t.Fatalf("fabricated window status: %+v", window)
		}
		if !groups[0].AllowedKnown || groups[0].Allowed || groups[0].LimitReachedKnown {
			t.Fatalf("raw presence changed: %+v", groups[0])
		}
	}
	for _, body := range []string{
		`{"rate_limit":{"allowed":false,"primary_window":null,"secondary_window":null}}`,
		`{"rate_limit":{"limit_reached":true}}`,
	} {
		_, windows, groups, err := parseUsage([]byte(body), now)
		if err != nil || len(windows) != 0 || len(groups) != 1 {
			t.Fatalf("group-only restriction lost: windows=%+v groups=%+v err=%v", windows, groups, err)
		}
	}
	for _, body := range []string{`{}`, `{"rate_limit":{}}`, `{"rate_limit":{"allowed":null}}`} {
		if _, _, _, err := parseUsage([]byte(body), now); err == nil {
			t.Fatalf("empty response accepted: %s", body)
		}
	}
}

func TestUsageGroupAndWindowEvidenceRemainIndependent(t *testing.T) {
	_, windows, groups, err := parseUsage([]byte(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":100},"secondary_window":{"used_percent":null}}}`), time.Now().UTC())
	if err != nil || len(windows) != 2 || len(groups) != 1 {
		t.Fatal(err)
	}
	if !groups[0].Allowed || groups[0].LimitReached || !windows[0].LimitReached || windows[1].AllowedKnown || windows[1].UsedPercentKnown {
		t.Fatalf("independent facts overwritten: windows=%+v groups=%+v", windows, groups)
	}
}
