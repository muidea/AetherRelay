package usage

import (
	"context"
	"testing"
	"time"
)

func TestUsageWritesInvalidateCachesAndRejectLateReadPublication(t *testing.T) {
	s := openTestStore(t)
	s.cache = newDashboardCache(60, 8)
	s.optionsCache = newFilterOptionsCache(60, 8)
	ctx := context.Background()
	at := time.Now().UTC()
	f := UsageFilter{From: at.Add(-time.Hour), To: at.Add(time.Hour)}
	q := FilterOptionsQuery{From: f.From, To: f.To}
	oldDashboard, err := s.Dashboard(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	oldOptions, err := s.FilterOptions(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	dashboardKey := dashboardCacheKey(f)
	optionsKey := filterOptionsCacheKey(q.From, q.To, false)
	_, _, dashboardEpoch := s.cache.get(dashboardKey)
	_, _, optionsEpoch := s.optionsCache.get(optionsKey)
	if err := s.Start(ctx, StartRecord{EventID: "event", APIKeyID: "key", StartedAt: at, Provider: "before", Model: "model-before"}); err != nil {
		t.Fatal(err)
	}
	// Reproduce an old query publishing after Start invalidated the cache.
	s.cache.put(dashboardKey, oldDashboard, dashboardEpoch)
	s.optionsCache.put(optionsKey, oldOptions, optionsEpoch)
	dashboard, err := s.Dashboard(ctx, f)
	if err != nil || dashboard.Summary.Requests != 1 {
		t.Fatalf("late read hid the started request: %+v err=%v", dashboard, err)
	}
	options, err := s.FilterOptions(ctx, q)
	if err != nil || !contains(options.Providers, "before") || !contains(options.Models, "model-before") {
		t.Fatalf("late read hid new options: %+v err=%v", options, err)
	}
	_, _, dashboardEpoch = s.cache.get(dashboardKey)
	_, _, optionsEpoch = s.optionsCache.get(optionsKey)
	if err := s.Complete(ctx, CompleteRecord{EventID: "event", CompletedAt: at.Add(time.Second), HTTPStatus: 200, Outcome: "success", Provider: "after", Model: "model-after", OutputTokens: 7}); err != nil {
		t.Fatal(err)
	}
	s.cache.put(dashboardKey, dashboard, dashboardEpoch)
	s.optionsCache.put(optionsKey, options, optionsEpoch)
	dashboard, err = s.Dashboard(ctx, f)
	if err != nil || dashboard.Summary.SuccessRequests != 1 || dashboard.Summary.OutputTokens != 7 {
		t.Fatalf("late read hid settlement: %+v err=%v", dashboard, err)
	}
	options, err = s.FilterOptions(ctx, q)
	if err != nil || contains(options.Providers, "before") || !contains(options.Providers, "after") || !contains(options.Models, "model-after") {
		t.Fatalf("settlement did not refresh options: %+v err=%v", options, err)
	}
	if err := s.Start(ctx, StartRecord{EventID: "interrupted", APIKeyID: "key", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Dashboard(ctx, f); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverInterrupted(ctx, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	dashboard, err = s.Dashboard(ctx, f)
	if err != nil || dashboard.Summary.FailedRequests != 1 {
		t.Fatalf("recovery did not invalidate aggregates: %+v err=%v", dashboard, err)
	}
}

func TestUsageReadSnapshotRemainsConsistentAcrossConcurrentSettlement(t *testing.T) {
	s := openTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	at := time.Now().UTC()
	if err := s.Start(ctx, StartRecord{EventID: "event", APIKeyID: "key", StartedAt: at, Provider: "before"}); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	where, args := buildFilterWhere(UsageFilter{}, at.Add(-time.Hour), at.Add(time.Hour))
	summary, err := s.querySummary(ctx, tx, where, args)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, CompleteRecord{EventID: "event", CompletedAt: at.Add(time.Second), HTTPStatus: 200, Outcome: "success", Provider: "after", OutputTokens: 7}); err != nil {
		t.Fatalf("reader snapshot blocked accounting: %v", err)
	}
	daily, err := s.queryDaily(ctx, tx, where, args, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := s.queryByAPIKey(ctx, tx, where, args)
	if err != nil {
		t.Fatal(err)
	}
	providers, _, err := s.queryDistinctColumn(ctx, tx, "provider", at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if summary.SuccessRequests != 0 || summary.OutputTokens != 0 || len(daily) != 1 || daily[0].OutputTokens != 0 || len(keys) != 1 || keys[0].SuccessRequests != 0 || keys[0].OutputTokens != 0 || !contains(providers, "before") || contains(providers, "after") {
		t.Fatalf("query mixed snapshots: summary=%+v daily=%+v keys=%+v providers=%v", summary, daily, keys, providers)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	dashboard, err := s.Dashboard(ctx, UsageFilter{AllTime: true})
	if err != nil || dashboard.Summary.OutputTokens != 7 || dashboard.Summary.SuccessRequests != 1 {
		t.Fatalf("new query did not observe settlement: %+v err=%v", dashboard, err)
	}
}
