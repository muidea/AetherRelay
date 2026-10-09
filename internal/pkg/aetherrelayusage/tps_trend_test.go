package usage

import (
	"context"
	"testing"
	"time"
)

func TestTPSTrendUTCDaysMissingSamplesAndFilters(t *testing.T) {
	for _, backend := range []string{"memory", "duckdb"} {
		t.Run(backend, func(t *testing.T) {
			var store Store = NewMemoryStore()
			if backend == "duckdb" {
				store = openTestStore(t)
			}
			defer store.Close()
			ctx := context.Background()
			from := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
			to := from.Add(4 * 24 * time.Hour)
			// This request starts on Oct 10 locally and finishes on Oct 10 UTC,
			// but belongs to the Oct 9 UTC trend bucket.
			boundary := time.Date(2026, 10, 10, 7, 59, 59, 0, time.FixedZone("UTC+8", 8*60*60))
			for _, row := range []struct {
				id, key, provider, model, outcome string
				at                                time.Time
				tokens                            int64
				duration                          time.Duration
				estimated                         bool
			}{
				{"historic", "key-a", "p1", "m1", "success", from.Add(time.Hour), 5000, 0, false},
				{"a", "key-a", "p1", "m1", "success", boundary, 100, 10 * time.Second, false},
				{"b", "key-b", "p1", "m1", "upstream_failed", from.Add(36 * time.Hour), 900, 30 * time.Second, true},
				{"missing", "key-a", "p2", "m2", "success", from.Add(37 * time.Hour), 5000, 0, false},
				{"zero", "key-a", "p1", "m1", "success", from.Add(60 * time.Hour), 0, time.Second, false},
			} {
				if err := store.Start(ctx, StartRecord{EventID: row.id, APIKeyID: row.key, Provider: row.provider, Model: row.model, StartedAt: row.at}); err != nil {
					t.Fatal(err)
				}
				status := 200
				if row.outcome != "success" {
					status = 502
				}
				if err := store.Complete(ctx, CompleteRecord{EventID: row.id, CompletedAt: row.at.Add(row.duration + 2*time.Second), HTTPStatus: status, Outcome: row.outcome,
					OutputTokens: row.tokens, OutputTokensKnown: true, FirstOutputAt: row.at.Add(time.Second), GenerationDuration: row.duration, Estimated: row.estimated}); err != nil {
					t.Fatal(err)
				}
			}
			dashboard, err := store.Dashboard(ctx, UsageFilter{From: from, To: to})
			if err != nil || len(dashboard.Daily) != 4 {
				t.Fatal(dashboard, err)
			}
			for i, day := range dashboard.Daily {
				if day.Date != from.Add(time.Duration(i)*24*time.Hour).Format("2006-01-02") {
					t.Fatal("wrong UTC bucket", day)
				}
			}
			day := dashboard.Daily[1]
			if day.TPS == nil || *day.TPS != 25 || day.TPSSamples != 2 || day.TPSOutputTokens != 1000 || day.TPSGenerationDurationMS != 40000 || day.TPSEstimatedSamples != 1 || day.TPSPartialSamples != 1 || day.OutputTokens != 6000 {
				t.Fatalf("weighted day: %+v", day)
			}
			zero := dashboard.Daily[2]
			if zero.TPS == nil || *zero.TPS != 0 || zero.TPSSamples != 1 || zero.TPSGenerationDurationMS != 1000 {
				t.Fatal("known zero lost", zero)
			}
			for _, i := range []int{0, 3} {
				if day := dashboard.Daily[i]; day.TPS != nil || day.TPSSamples != 0 || day.TPSGenerationDurationMS != 0 {
					t.Fatal("missing timing or empty day became zero TPS", day)
				}
			}
			estimated := true
			for _, test := range []struct {
				name   string
				filter UsageFilter
				want   float64
				known  bool
			}{
				{"key", UsageFilter{APIKeyID: "key-a"}, 10, true},
				{"provider", UsageFilter{Provider: "p2"}, 0, false},
				{"model", UsageFilter{Model: "m2"}, 0, false},
				{"outcome", UsageFilter{Outcome: "success"}, 10, true},
				{"estimated", UsageFilter{Estimated: &estimated}, 30, true},
			} {
				t.Run(test.name, func(t *testing.T) {
					test.filter.From, test.filter.To = from, to
					filtered, err := store.Dashboard(ctx, test.filter)
					if err != nil || len(filtered.Daily) != 4 {
						t.Fatal(filtered, err)
					}
					day := filtered.Daily[1]
					if test.known {
						if day.TPS == nil || *day.TPS != test.want || day.TPSSamples != 1 {
							t.Fatal(day)
						}
					} else if day.TPS != nil || day.TPSSamples != 0 {
						t.Fatal(day)
					}
				})
			}
		})
	}
}
