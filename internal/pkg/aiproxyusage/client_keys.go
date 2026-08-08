package usage

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"ai-proxy/internal/pkg/aiproxyclientaccess"
)

func (s *DuckDBStore) ListClientAPIKeys(ctx context.Context) (map[string]ClientAPIKeyRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, e := tx.QueryContext(ctx, `SELECT api_key_id,coalesce(key_hash,''),coalesce(enabled,TRUE),created_at,last_used_at,last_rotated_at,revoked_at,provider_access_mode FROM client_api_key_metadata`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := map[string]ClientAPIKeyRecord{}
	for rows.Next() {
		var r ClientAPIKeyRecord
		var a, b, c sql.NullTime
		if e := rows.Scan(&r.ID, &r.Hash, &r.Enabled, &r.CreatedAt, &a, &b, &c, &r.ProviderAccess.Mode); e != nil {
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	accessRows, err := tx.QueryContext(ctx, `SELECT api_key_id,provider_id FROM client_api_key_provider_access ORDER BY api_key_id,provider_id`)
	if err != nil {
		return nil, err
	}
	for accessRows.Next() {
		var keyID, providerID string
		if err := accessRows.Scan(&keyID, &providerID); err != nil {
			_ = accessRows.Close()
			return nil, err
		}
		record, ok := out[keyID]
		if !ok {
			continue
		}
		record.ProviderAccess.ProviderIDs = append(record.ProviderAccess.ProviderIDs, providerID)
		out[keyID] = record
	}
	if err := accessRows.Err(); err != nil {
		_ = accessRows.Close()
		return nil, err
	}
	if err := accessRows.Close(); err != nil {
		return nil, err
	}
	for id, record := range out {
		policy, err := clientaccess.Normalize(record.ProviderAccess)
		if err != nil {
			return nil, err
		}
		record.ProviderAccess = policy
		out[id] = record
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
func (s *DuckDBStore) CreateClientAPIKey(ctx context.Context, r ClientAPIKeyRecord) error {
	policy, err := clientaccess.Normalize(r.ProviderAccess)
	if err != nil {
		return err
	}
	s.write.Lock()
	defer s.write.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO client_api_key_metadata(api_key_id,key_hash,enabled,created_at,provider_access_mode) VALUES(?,?,?,?,?)`, r.ID, r.Hash, r.Enabled, r.CreatedAt.UTC(), policy.Mode); err != nil {
		return err
	}
	if err := insertProviderAccess(ctx, tx, r.ID, policy); err != nil {
		return err
	}
	return tx.Commit()
}

func insertProviderAccess(ctx context.Context, tx *sql.Tx, id string, policy clientaccess.Policy) error {
	for _, providerID := range policy.ProviderIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO client_api_key_provider_access(api_key_id,provider_id) VALUES(?,?)`, id, providerID); err != nil {
			return err
		}
	}
	return nil
}

func (s *DuckDBStore) SetClientAPIKeyProviderAccess(ctx context.Context, id string, value clientaccess.Policy) error {
	policy, err := clientaccess.Normalize(value)
	if err != nil {
		return err
	}
	s.write.Lock()
	defer s.write.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE client_api_key_metadata SET provider_access_mode=? WHERE api_key_id=?`, policy.Mode, id)
	if err != nil {
		return err
	}
	if count, countErr := result.RowsAffected(); countErr == nil && count == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM client_api_key_provider_access WHERE api_key_id=?`, id); err != nil {
		return err
	}
	if err := insertProviderAccess(ctx, tx, id, policy); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *DuckDBStore) ClientAPIKeyIDsForProvider(ctx context.Context, providerID string) ([]string, error) {
	providerID = strings.ToLower(strings.TrimSpace(providerID))
	rows, err := s.db.QueryContext(ctx, `SELECT a.api_key_id FROM client_api_key_provider_access a JOIN client_api_key_metadata k ON k.api_key_id=a.api_key_id WHERE a.provider_id=? AND k.provider_access_mode='selected' ORDER BY a.api_key_id`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	sort.Strings(result)
	return result, rows.Err()
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

func (s *DuckDBStore) DeleteClientAPIKey(ctx context.Context, id string) error {
	s.write.Lock()
	defer s.write.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM client_api_key_metadata WHERE api_key_id=?`, id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM client_api_key_provider_access WHERE api_key_id=?`, id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM client_api_key_metadata WHERE api_key_id=?`, id)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_events WHERE api_key_id=?`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.cache.clear()
	s.optionsCache.clear()
	return nil
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO client_api_key_metadata(api_key_id, created_at, provider_access_mode) VALUES (?, ?, 'all') ON CONFLICT(api_key_id) DO NOTHING`, id, createdAt.UTC())
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO client_api_key_metadata(api_key_id, created_at, last_used_at, provider_access_mode) VALUES (?, ?, ?, 'all') ON CONFLICT(api_key_id) DO UPDATE SET last_used_at = excluded.last_used_at`, id, usedAt.UTC(), usedAt.UTC())
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
