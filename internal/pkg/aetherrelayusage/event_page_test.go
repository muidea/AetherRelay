package usage

import (
	"context"
	"testing"
	"time"

	trace "aetherrelay/internal/pkg/aetherrelaytrace"
)

func TestEventsLowMemoryPaginationWithLargeSideTables(t *testing.T) {
	s := openTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Seed at the ordinary budget, persist, then query at the minimum supported
	// budget. The original seven-way detail join OOMs on this fixture.
	for _, query := range []string{
		`INSERT INTO usage_events(event_id,started_at,usage_date,api_key_id,provider,model,state) SELECT 'event-'||lpad(i::VARCHAR,8,'0'), now(),current_date,'key','codexoauth','gpt-6.1-sol','started' FROM range(100000) t(i)`,
		`INSERT INTO usage_generation SELECT event_id,started_at,1000000,false FROM usage_events`,
		`INSERT INTO usage_generation_buffering SELECT event_id FROM usage_events`,
		`INSERT INTO usage_output_observation SELECT event_id,true FROM usage_events`,
		`INSERT INTO usage_account_selection SELECT event_id,'account','account@example.com' FROM usage_events`,
		`INSERT INTO usage_codex_model SELECT event_id,'gpt-6.1-sol','gpt-6.1-sol',false FROM usage_events`,
		`INSERT INTO usage_upstream_error SELECT event_id,'server_error','server_overloaded','',repeat('synthetic error ',20) FROM usage_events`,
		`INSERT INTO usage_execution_trace SELECT event_id,event_id,'main','host',2,1,true,'network','websocket','collected','{}' FROM usage_events`,
		`CHECKPOINT`,
		`SET memory_limit='32MB'`,
	} {
		if _, err := s.db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	f := EventFilter{UsageFilter: UsageFilter{Timezone: "Asia/Singapore", From: time.Now().Add(-24 * time.Hour), To: time.Now().Add(24 * time.Hour)}, PageSize: 50}
	first, err := s.Events(ctx, f)
	if err != nil || len(first.Events) != 50 || first.NextCursor == "" {
		t.Fatalf("bounded first page failed: count=%d reason=%s err=%v", len(first.Events), FailureReason(err), err)
	}
	e := first.Events[0]
	if e.EventID != "event-00099999" || e.AccountID != "account" || e.ResponseModel != "gpt-6.1-sol" || e.UpstreamErrorCode != "server_overloaded" || e.TraceRequestID != e.EventID || e.AttemptCount != 2 || !e.GenerationBuffered || !e.OutputTokensKnown || e.FirstOutputAt == nil {
		t.Fatalf("lost bounded-page details: %+v", e)
	}
	f.Cursor = first.NextCursor
	second, err := s.Events(ctx, f)
	if err != nil || len(second.Events) != 50 || second.Events[0].EventID != "event-00099949" {
		t.Fatalf("bounded cursor page failed: count=%d err=%v", len(second.Events), err)
	}
	// An exact-ID read and an empty page must also release every result/tx.
	f.Cursor = ""
	f.EventIDs = []string{first.Events[0].EventID}
	exact, err := s.Events(ctx, f)
	if err != nil || len(exact.Events) != 1 || exact.NextCursor != "" {
		t.Fatalf("exact page failed: %+v err=%v", exact, err)
	}
	f.EventIDs = []string{"absent"}
	empty, err := s.Events(ctx, f)
	if err != nil || len(empty.Events) != 0 || empty.NextCursor != "" || s.db.Stats().InUse != 0 {
		t.Fatalf("empty page or cleanup failed: %+v err=%v stats=%+v", empty, err, s.db.Stats())
	}
}

func TestEventPageDetailsShareSnapshotWithBaseRead(t *testing.T) {
	s := openTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	at := time.Now().UTC()
	if err := s.Start(ctx, StartRecord{EventID: "event", APIKeyID: "key", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	oldTrace := trace.Trace{Context: trace.Context{RequestID: "before"}}
	if err := s.WriteTrace(ctx, "event", oldTrace); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT event_id FROM usage_events WHERE event_id='event'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	newTrace := trace.Trace{Context: trace.Context{RequestID: "after"}}
	if err := s.Complete(ctx, CompleteRecord{EventID: id, CompletedAt: at.Add(time.Second), HTTPStatus: 200, Outcome: "success", AccountID: "after", Trace: &newTrace}); err != nil {
		t.Fatal(err)
	}
	page := []Event{{EventID: id}}
	if err := s.loadEventPageDetails(ctx, tx, []string{id}, page); err != nil {
		t.Fatal(err)
	}
	if page[0].TraceRequestID != "before" || page[0].AccountID != "" {
		t.Fatalf("mixed page snapshots: %+v", page[0])
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	current, err := s.Events(ctx, EventFilter{UsageFilter: UsageFilter{AllTime: true}})
	if err != nil || len(current.Events) != 1 || current.Events[0].TraceRequestID != "after" || current.Events[0].AccountID != "after" {
		t.Fatalf("new page missed settlement: %+v err=%v", current, err)
	}
}
