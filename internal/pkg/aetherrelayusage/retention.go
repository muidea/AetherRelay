package usage

import (
	"context"
	"fmt"
	"time"
)

const retentionBatchSize = 2000

// PruneBefore removes completed calls and every event-owned side record in
// bounded transactions. In-flight calls and client/account configuration stay
// intact. A canceled sweep can resume safely at the next maintenance tick.
func (s *DuckDBStore) PruneBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	if cutoff.IsZero() {
		return 0, fmt.Errorf("retention cutoff is required")
	}
	var total int64
	for {
		n, err := s.pruneBatch(ctx, cutoff.UTC())
		total += n
		if err != nil {
			return total, err
		}
		if n < retentionBatchSize {
			break
		}
	}
	// Also retry a previously failed checkpoint when no rows remain to delete.
	// DuckDB may reclaim/reuse eligible blocks; file shrinkage is not guaranteed.
	if err := s.Checkpoint(ctx); err != nil {
		return total, fmt.Errorf("checkpoint retained usage: %w", err)
	}
	return total, nil
}

func (s *DuckDBStore) pruneBatch(ctx context.Context, cutoff time.Time) (int64, error) {
	s.write.Lock()
	defer s.write.Unlock()
	if s.closed.Load() {
		return 0, ErrStoreClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin usage retention: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	const expired = `event_id IN (SELECT event_id FROM usage_events WHERE state='completed' AND started_at < ? ORDER BY started_at, event_id LIMIT 2000)`
	for _, table := range []string{"usage_execution_trace", "usage_upstream_error", "usage_codex_model", "usage_account_selection", "usage_output_observation", "usage_generation_buffering", "usage_generation"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+expired, cutoff); err != nil {
			return 0, fmt.Errorf("prune %s: %w", table, err)
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM usage_events WHERE `+expired, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune usage events: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if n > 0 {
		s.cache.clear()
		s.optionsCache.clear()
	}
	return n, nil
}

func (s *MemoryStore) PruneBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	if cutoff.IsZero() {
		return 0, fmt.Errorf("retention cutoff is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return 0, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var n int64
	for id, ev := range s.events {
		if ev.State == StateCompleted && ev.StartedAt.Before(cutoff) {
			delete(s.events, id)
			delete(s.traces, id)
			n++
		}
	}
	return n, nil
}
