package usage

import (
	"context"
	"database/sql"
	"time"
)

func (s *DuckDBStore) ListClientAPIKeys(ctx context.Context) (map[string]ClientAPIKeyRecord, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT api_key_id,coalesce(key_hash,''),coalesce(enabled,TRUE),created_at,last_used_at,last_rotated_at,revoked_at FROM client_api_key_metadata`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := map[string]ClientAPIKeyRecord{}
	for rows.Next() {
		var r ClientAPIKeyRecord
		var a, b, c sql.NullTime
		if e := rows.Scan(&r.ID, &r.Hash, &r.Enabled, &r.CreatedAt, &a, &b, &c); e != nil {
			return nil, e
		}
		if a.Valid {
			r.LastUsedAt = &a.Time
		}
		if b.Valid {
			r.LastRotatedAt = &b.Time
		}
		if c.Valid {
			r.RevokedAt = &c.Time
		}
		out[r.ID] = r
	}
	return out, rows.Err()
}
func (s *DuckDBStore) CreateClientAPIKey(ctx context.Context, r ClientAPIKeyRecord) error {
	s.write.Lock()
	defer s.write.Unlock()
	_, e := s.db.ExecContext(ctx, `INSERT INTO client_api_key_metadata(api_key_id,key_hash,enabled,created_at) VALUES(?,?,?,?)`, r.ID, r.Hash, r.Enabled, r.CreatedAt.UTC())
	return e
}
func (s *DuckDBStore) SetClientAPIKeyEnabled(ctx context.Context, id string, v bool) error {
	s.write.Lock()
	defer s.write.Unlock()
	res, e := s.db.ExecContext(ctx, `UPDATE client_api_key_metadata SET enabled=?,revoked_at=CASE WHEN ? THEN NULL ELSE COALESCE(revoked_at,NOW()) END WHERE api_key_id=?`, v, v, id)
	if e == nil {
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return sql.ErrNoRows
		}
	}
	return e
}
func (s *DuckDBStore) RotateClientAPIKey(ctx context.Context, id, h string, t time.Time) error {
	s.write.Lock()
	defer s.write.Unlock()
	res, e := s.db.ExecContext(ctx, `UPDATE client_api_key_metadata SET key_hash=?,enabled=TRUE,revoked_at=NULL,last_rotated_at=? WHERE api_key_id=?`, h, t.UTC(), id)
	if e == nil {
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return sql.ErrNoRows
		}
	}
	return e
}
func (s *DuckDBStore) RevokeClientAPIKey(ctx context.Context, id string, t time.Time) error {
	s.write.Lock()
	defer s.write.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE client_api_key_metadata SET enabled=FALSE,revoked_at=? WHERE api_key_id=?`, t.UTC(), id)
	if err == nil {
		if n, e := res.RowsAffected(); e == nil && n == 0 {
			return sql.ErrNoRows
		}
	}
	return err
}

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
