package usage

import (
	"context"
	"database/sql"
	"time"
)

func (s *DuckDBStore) EnsureClientAPIKey(ctx context.Context, id string, createdAt time.Time) error {
	if s == nil || s.closed.Load() {
		return ErrStoreUnavailable
	}
	if id == "" {
		return nil
	}
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	s.write.Lock()
	defer s.write.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO client_api_key_metadata(api_key_id, created_at) VALUES (?, ?) ON CONFLICT(api_key_id) DO NOTHING`, id, createdAt.UTC())
	return err
}

func (s *DuckDBStore) TouchClientAPIKey(ctx context.Context, id string, usedAt time.Time) error {
	if s == nil || s.closed.Load() {
		return ErrStoreUnavailable
	}
	if id == "" {
		return nil
	}
	if usedAt.IsZero() {
		usedAt = time.Now().UTC()
	}
	s.write.Lock()
	defer s.write.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO client_api_key_metadata(api_key_id, created_at, last_used_at) VALUES (?, ?, ?) ON CONFLICT(api_key_id) DO UPDATE SET last_used_at = excluded.last_used_at`, id, usedAt.UTC(), usedAt.UTC())
	return err
}

func (s *DuckDBStore) ClientAPIKeyMetadata(ctx context.Context) (map[string]ClientAPIKeyMetadata, error) {
	result := map[string]ClientAPIKeyMetadata{}
	if s == nil || s.closed.Load() {
		return result, ErrStoreUnavailable
	}
	rows, err := s.db.QueryContext(ctx, `SELECT api_key_id, created_at, last_used_at FROM client_api_key_metadata`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item ClientAPIKeyMetadata
		var last sql.NullTime
		if err := rows.Scan(&item.ID, &item.CreatedAt, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			value := last.Time
			item.LastUsedAt = &value
		}
		result[item.ID] = item
	}
	return result, rows.Err()
}
