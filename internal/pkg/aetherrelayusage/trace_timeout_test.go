package usage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	trace "aetherrelay/internal/pkg/aetherrelaytrace"
)

func TestTraceCheckpointDeadlineIncludesWriterLockWait(t *testing.T) {
	s, err := OpenDuckDB(testCfg(filepath.Join(t.TempDir(), "usage.duckdb")))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.write.Lock()
	defer s.write.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.WriteTrace(ctx, "event", trace.Trace{}) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("checkpoint exceeded its budget while waiting for writer lock")
	}
}

func TestUsageSettlementAndShutdownDeadlineIncludeWriterLockWait(t *testing.T) {
	s, err := OpenDuckDB(testCfg(filepath.Join(t.TempDir(), "usage.duckdb")))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for name, run := range map[string]func(context.Context) error{
		"start": func(ctx context.Context) error {
			return s.Start(ctx, StartRecord{EventID: "event", APIKeyID: "client"})
		},
		"complete": func(ctx context.Context) error {
			return s.Complete(ctx, CompleteRecord{EventID: "event", HTTPStatus: 503, Outcome: "provider_unavailable"})
		},
		"recover":    func(ctx context.Context) error { _, err := s.RecoverInterrupted(ctx, time.Now()); return err },
		"retention":  func(ctx context.Context) error { _, err := s.PruneBefore(ctx, time.Now()); return err },
		"checkpoint": s.Checkpoint,
		"close":      s.CloseContext,
	} {
		t.Run(name, func(t *testing.T) {
			s.write.Lock()
			defer s.write.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- run(ctx) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("usage mutation ignored its deadline")
			}
		})
	}
}

func TestTraceStateQueryFailureIsNotSettledEvent(t *testing.T) {
	s := openTestStore(t)
	// Corrupt only this isolated test database to exercise a real SQL failure.
	if _, err := s.db.ExecContext(context.Background(), `DROP TABLE usage_events`); err != nil {
		t.Fatal(err)
	}
	err := s.WriteTrace(context.Background(), "event", trace.Trace{})
	if !errors.Is(err, ErrStoreUnavailable) || errors.Is(err, ErrEventNotStarted) {
		t.Fatalf("database failure classified as settled event: %v", err)
	}
}
