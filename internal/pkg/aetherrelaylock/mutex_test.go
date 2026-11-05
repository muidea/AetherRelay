package aetherrelaylock

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCanceledWaitDoesNotAcquireOrLeakLock(t *testing.T) {
	var m Mutex
	m.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.LockContext(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		m.Unlock()
		t.Fatal("canceled lock wait remained blocked")
	}
	m.Unlock()
	if err := m.LockContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired context acquired a free lock: %v", err)
	}
	m.Lock()
	m.Unlock()
}
