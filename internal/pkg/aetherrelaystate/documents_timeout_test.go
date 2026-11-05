package aetherrelaystate

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSecureDocumentDeadlineIncludesLockAndConnectionWait(t *testing.T) {
	for _, mode := range []string{"lock", "connections"} {
		t.Run(mode, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "state.duckdb"), "128MB", 1)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if mode == "lock" {
				s.mu.Lock()
				defer s.mu.Unlock()
			} else {
				s.shared.db.SetMaxOpenConns(1)
				conn, err := s.shared.db.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- s.ApplySecureDocumentsContext(ctx, "audit", []SecureDocumentRow{{ID: "one", Payload: []byte("sealed")}}, nil)
			}()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("state write ignored deadline")
			}
		})
	}
}
