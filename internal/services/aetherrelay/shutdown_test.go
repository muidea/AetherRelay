package aetherrelay

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestShutdownRetriesWithFreshBudget(t *testing.T) {
	calls := 0
	shutdownUntilComplete(func(ctx context.Context) error {
		calls++
		if ctx.Err() != nil {
			t.Fatal("attempt inherited a canceled context")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("attempt has no deadline")
		}
		if calls == 1 {
			<-ctx.Done()
			return errors.New("not drained")
		}
		return nil
	}, 10*time.Millisecond, 0)
	if calls != 2 {
		t.Fatalf("attempts=%d", calls)
	}
}
