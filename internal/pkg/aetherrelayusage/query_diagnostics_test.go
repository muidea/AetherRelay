package usage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

func TestQueryFailureClassificationDoesNotExposeDriverMessage(t *testing.T) {
	s := openTestStore(t)
	for _, tc := range []struct {
		name   string
		code   duckdb.ErrorType
		reason string
	}{
		{"memory", duckdb.ErrorTypeOutOfMemory, "duckdb_out_of_memory"},
		{"interrupt", duckdb.ErrorTypeInterrupt, "duckdb_interrupted"},
		{"io", duckdb.ErrorTypeIO, "duckdb_io"},
		{"catalog", duckdb.ErrorTypeCatalog, "duckdb_catalog"},
		{"binder", duckdb.ErrorTypeBinder, "duckdb_binder"},
		{"conversion", duckdb.ErrorTypeConversion, "duckdb_conversion"},
		{"internal", duckdb.ErrorTypeInternal, "duckdb_internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause := &duckdb.Error{Type: tc.code, Msg: "secret-driver-value"}
			err := s.queryFailure("events", "query", cause)
			// Exercise wrapping as used by the process-local EventHub port.
			wrapped := fmt.Errorf("usage command: %w", err)
			if !errors.Is(wrapped, ErrStoreUnavailable) || !errors.Is(wrapped, cause) || FailureReason(wrapped) != tc.reason {
				t.Fatalf("failure classification lost: %v", wrapped)
			}
			details, ok := QueryFailureDetailsFromError(wrapped)
			if !ok || details.Operation != "events" || details.Stage != "query" || details.DriverCode != int(tc.code) {
				t.Fatalf("details=%+v ok=%v", details, ok)
			}
			if strings.Contains(wrapped.Error(), "secret-") {
				t.Fatal("raw driver message exposed")
			}
		})
	}
}

func TestEventsPoolDeadlinePreservesCauseAndPoolOccupancy(t *testing.T) {
	s := openTestStore(t)
	s.db.SetMaxOpenConns(1)
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = s.Events(ctx, EventFilter{UsageFilter: UsageFilter{AllTime: true}})
	details, ok := QueryFailureDetailsFromError(err)
	if !errors.Is(err, context.DeadlineExceeded) || FailureReason(err) != "deadline_exceeded" || !ok || details.Stage != "query" || details.PoolInUse != 1 || details.PoolWaitCount == 0 {
		t.Fatalf("err=%v details=%+v ok=%v", err, details, ok)
	}
}

func TestEventsQueryAndScanFailuresHaveDistinctDiagnostics(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		s := openTestStore(t)
		if _, err := s.db.Exec(`DROP TABLE usage_generation`); err != nil {
			t.Fatal(err)
		}
		_, err := s.Events(context.Background(), EventFilter{UsageFilter: UsageFilter{AllTime: true}})
		details, ok := QueryFailureDetailsFromError(err)
		if !ok || details.Stage != "query" || FailureReason(err) != "duckdb_catalog" {
			t.Fatalf("err=%v details=%+v", err, details)
		}
	})
	t.Run("scan", func(t *testing.T) {
		s := openTestStore(t)
		// Deliberately violate the current data contract in this temporary DB.
		// Reading must report a conversion failure without logging the value.
		for _, query := range []string{
			`DROP INDEX idx_usage_trace_cause`,
			`ALTER TABLE usage_execution_trace ALTER COLUMN attempt_count TYPE DOUBLE`,
			`INSERT INTO usage_events(event_id,started_at,usage_date,api_key_id,state) VALUES ('event',now(),current_date,'key','started')`,
			`INSERT INTO usage_execution_trace VALUES ('event','request','main','host',1.25,0,false,'','','collected','{}')`,
		} {
			if _, err := s.db.Exec(query); err != nil {
				t.Fatal(err)
			}
		}
		_, err := s.Events(context.Background(), EventFilter{UsageFilter: UsageFilter{AllTime: true}})
		details, ok := QueryFailureDetailsFromError(err)
		if !ok || details.Stage != "scan" || FailureReason(err) != "row_scan_failed" || strings.Contains(err.Error(), "secret-") {
			t.Fatalf("err=%v details=%+v", err, details)
		}
	})
}

func TestCanceledDuckDBQueryDoesNotPoisonNextUsageQuery(t *testing.T) {
	s := openTestStore(t)
	// Reuse the same physical connection after each real query interruption.
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	if err := s.Start(context.Background(), StartRecord{EventID: "event", APIKeyID: "key"}); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		_, err := s.db.ExecContext(ctx, `SELECT sum(a.i*b.i) FROM range(1000000) a(i) CROSS JOIN range(1000000) b(i)`)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected real query interruption: %v", err)
		}
		readCtx, readCancel := context.WithTimeout(context.Background(), time.Second)
		page, err := s.Events(readCtx, EventFilter{UsageFilter: UsageFilter{AllTime: true}})
		readCancel()
		if err != nil || len(page.Events) != 1 || page.Events[0].EventID != "event" {
			t.Fatalf("canceled query affected subsequent read: page=%+v err=%v", page, err)
		}
	}
}
