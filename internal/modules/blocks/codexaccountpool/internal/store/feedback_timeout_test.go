package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

func TestFeedbackDeadlineIncludesStoreLockWait(t *testing.T) {
	s := openTestStore(t)
	for name, run := range map[string]func(context.Context) error{
		"result": func(ctx context.Context) error {
			_, err := s.RecordResultContext(ctx, "account", "model", false, events.ErrorRateLimit, 0, false, "", false)
			return err
		},
		"usage_headers": func(ctx context.Context) error {
			_, err := s.MergeUsageSnapshotWithScheduleContext(ctx, "account", events.AccountUsageSnapshot{}, "response_header", "")
			return err
		},
		"transport": func(ctx context.Context) error {
			_, err := s.RecordTransportCapabilityContext(ctx, "account", events.TransportCompact, false)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			s.mu.Lock()
			defer s.mu.Unlock()
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
				t.Fatal("feedback did not stop waiting on store lock")
			}
		})
	}
}
