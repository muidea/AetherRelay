package usage

import (
	"context"
	"testing"
	"time"
)

// CP-OBS-016: pending status refresh and local rejection diagnostics.
func TestEventIDLookupReadsLatestStateAndPreservesOtherFilters(t *testing.T) {
	for _, kind := range []string{"memory", "duckdb"} {
		t.Run(kind, func(t *testing.T) {
			var store Store
			if kind == "memory" {
				store = NewMemoryStore()
				t.Cleanup(func() { _ = store.Close() })
			} else {
				store = openTestStore(t)
			}
			ctx := context.Background()
			at := time.Now().UTC().Add(-time.Minute)
			for _, id := range []string{"pending", "other", "quote' OR TRUE --"} {
				if err := store.Start(ctx, StartRecord{EventID: id, APIKeyID: "thought-flow", Model: "auto", StartedAt: at}); err != nil {
					t.Fatal(err)
				}
			}
			filter := EventFilter{UsageFilter: UsageFilter{AllTime: true}, PageSize: 100, EventIDs: []string{"pending"}}
			page, err := store.Events(ctx, filter)
			if err != nil || len(page.Events) != 1 || page.Events[0].State != StateStarted {
				t.Fatalf("pending lookup=%+v err=%v", page, err)
			}
			if err := store.Complete(ctx, CompleteRecord{EventID: "pending", CompletedAt: at.Add(time.Second), HTTPStatus: 200, Outcome: "success", Provider: "chatgptweb", Model: "gpt-6", InputTokens: 2940, OutputTokens: 1922}); err != nil {
				t.Fatal(err)
			}
			page, err = store.Events(ctx, filter)
			if err != nil || len(page.Events) != 1 || page.Events[0].State != StateCompleted || page.Events[0].Model != "gpt-6" || page.Events[0].TotalTokens != 4862 || page.NextCursor != "" {
				t.Fatalf("latest lookup=%+v err=%v", page, err)
			}
			filter.APIKeyID = "different-key"
			page, err = store.Events(ctx, filter)
			if err != nil || len(page.Events) != 0 {
				t.Fatalf("ID lookup bypassed key filter: %+v err=%v", page, err)
			}
			filter.APIKeyID = ""
			filter.EventIDs = []string{"quote' OR TRUE --", "missing", "quote' OR TRUE --"}
			page, err = store.Events(ctx, filter)
			if err != nil || len(page.Events) != 1 || page.Events[0].EventID != "quote' OR TRUE --" {
				t.Fatalf("exact/duplicate IDs=%+v err=%v", page, err)
			}
			filter.EventIDs = make([]string, 101)
			if _, err = store.Events(ctx, filter); err == nil {
				t.Fatal("unbounded ID lookup accepted")
			}
		})
	}
}
