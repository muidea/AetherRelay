package usage

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestCacheStatistics(t *testing.T) {
	for _, backend := range []string{"memory", "duckdb"} {
		t.Run(backend, func(t *testing.T) {
			var store Store = NewMemoryStore()
			if backend == "duckdb" {
				s := openTestStore(t)
				s.cache = newDashboardCache(60, 16)
				store = s
			}
			t.Cleanup(func() { _ = store.Close() })
			ctx := context.Background()
			from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			to := from.AddDate(0, 0, 4)
			for _, rec := range []struct {
				id, key, provider, model, outcome string
				day                               int
				input, cached, creation           int64
				estimated                         bool
			}{
				{"a", "key-a", "p", "m", "success", 0, 100, 100, 10, false},
				{"b", "key-a", "p", "m", "success", 0, 900, 0, 20, false},
				{"c", "key-b", "q", "n", "upstream_failed", 2, 300, 150, 30, true},
				{"zero", "key-zero", "q", "n", "success", 2, 0, 0, 0, false},
			} {
				at := from.AddDate(0, 0, rec.day)
				if err := store.Start(ctx, StartRecord{EventID: rec.id, APIKeyID: rec.key, StartedAt: at, Provider: rec.provider, Model: rec.model}); err != nil {
					t.Fatal(err)
				}
				if err := store.Complete(ctx, CompleteRecord{EventID: rec.id, CompletedAt: at.Add(time.Second), InputTokens: rec.input, OutputTokens: 20, CachedInputTokens: rec.cached, CacheCreationInputTokens: rec.creation, HTTPStatus: 200, Outcome: rec.outcome, Estimated: rec.estimated}); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Start(ctx, StartRecord{EventID: "pending", APIKeyID: "key-a", StartedAt: from, Provider: "p", Model: "m"}); err != nil {
				t.Fatal(err)
			}
			exact := false
			for _, tc := range []struct {
				name                    string
				filter                  UsageFilter
				input, cached, creation int64
			}{
				{"all", UsageFilter{}, 1300, 250, 60},
				{"key", UsageFilter{APIKeyID: "key-a"}, 1000, 100, 30},
				{"provider", UsageFilter{Provider: "p"}, 1000, 100, 30},
				{"model", UsageFilter{Model: "m"}, 1000, 100, 30},
				{"outcome", UsageFilter{Outcome: "upstream_failed"}, 300, 150, 30},
				{"exact", UsageFilter{Estimated: &exact}, 1000, 100, 30},
				{"time", UsageFilter{To: from.AddDate(0, 0, 1)}, 1000, 100, 30},
				{"empty", UsageFilter{Provider: "missing"}, 0, 0, 0},
				{"zero", UsageFilter{APIKeyID: "key-zero"}, 0, 0, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					f := tc.filter
					f.From = from
					if f.To.IsZero() {
						f.To = to
					}
					// Repeated reads also exercise the DuckDB dashboard cache.
					for i := 0; i < 2; i++ {
						dash, err := store.Dashboard(ctx, f)
						if err != nil {
							t.Fatal(err)
						}
						s := dash.Summary
						if s.InputTokens != tc.input || s.CachedInputTokens != tc.cached || s.CacheCreationInputTokens != tc.creation {
							t.Fatalf("summary=%+v", s)
						}
						assertCacheRate(t, s.CacheHitRate, tc.cached, tc.input)
						var dailyCached, dailyCreation, keyCached, keyCreation int64
						for _, b := range dash.Daily {
							assertCacheRate(t, b.CacheHitRate, b.CachedInputTokens, b.InputTokens)
							dailyCached += b.CachedInputTokens
							dailyCreation += b.CacheCreationInputTokens
						}
						for _, k := range dash.ByAPIKey {
							assertCacheRate(t, k.CacheHitRate, k.CachedInputTokens, k.InputTokens)
							keyCached += k.CachedInputTokens
							keyCreation += k.CacheCreationInputTokens
						}
						if dailyCached != tc.cached || keyCached != tc.cached || dailyCreation != tc.creation || keyCreation != tc.creation {
							t.Fatalf("cache aggregates disagree: %+v", dash)
						}
						if tc.name == "all" && (len(dash.Daily) != 4 || dash.Daily[1].CacheHitRate != 0 || dash.Daily[0].CacheHitRate != 0.1 || dash.Daily[2].CacheHitRate != 0.5) {
							t.Fatalf("daily=%+v", dash.Daily)
						}
					}
				})
			}
			byKey, err := store.AllTimeByKey(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if byKey["key-a"].CacheHitRate != 0.1 || byKey["key-a"].CachedInputTokens != 100 || byKey["key-a"].CacheCreationInputTokens != 30 {
				t.Fatalf("all-time=%+v", byKey)
			}
			page, err := store.Events(ctx, EventFilter{UsageFilter: UsageFilter{From: from, To: to}, PageSize: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Events) != 5 {
				t.Fatalf("events=%+v", page)
			}
			for _, e := range page.Events {
				assertCacheRate(t, e.CacheHitRate, e.CachedInputTokens, e.InputTokens)
			}
			if err := store.Complete(ctx, CompleteRecord{EventID: "pending", CompletedAt: from.Add(time.Second), InputTokens: 100, CachedInputTokens: 100, HTTPStatus: 200, Outcome: "success"}); err != nil {
				t.Fatal(err)
			}
			if s, ok := store.(*DuckDBStore); ok {
				// Dashboard intentionally uses a short TTL rather than invalidating
				// on each write. Expire entries without sleeping to check refresh.
				s.cache.mu.Lock()
				for _, entry := range s.cache.items {
					entry.expiresAt = time.Now().Add(-time.Second)
				}
				s.cache.mu.Unlock()
			}
			dash, err := store.Dashboard(ctx, UsageFilter{From: from, To: to})
			if err != nil {
				t.Fatal(err)
			}
			if dash.Summary.CachedInputTokens != 350 {
				t.Fatalf("stale cache statistics: %+v", dash.Summary)
			}
			assertCacheRate(t, dash.Summary.CacheHitRate, 350, 1400)
		})
	}
}

func assertCacheRate(t *testing.T, got float64, cached, input int64) {
	t.Helper()
	want := 0.0
	if input > 0 {
		want = float64(cached) / float64(input)
	}
	if math.IsNaN(got) || math.IsInf(got, 0) || math.Abs(got-want) > 1e-12 {
		t.Fatalf("cache rate=%v, want %v (%d/%d)", got, want, cached, input)
	}
}
