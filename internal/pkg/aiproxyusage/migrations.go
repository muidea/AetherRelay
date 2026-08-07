package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	currentSchemaVersion = 1
	currentSchemaName    = "usage_final_v1"
)

// migrate owns only the usage runtime tables. Historical schemas are not
// upgraded: any schema generation other than the current final v1 is replaced
// atomically, while an existing final v1 is left intact across restarts.
func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema initialization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        VARCHAR NOT NULL,
    applied_at  TIMESTAMPTZ NOT NULL
)`); err != nil {
		return fmt.Errorf("create schema version table: %w", err)
	}

	var version int
	var name string
	err = tx.QueryRowContext(ctx, `SELECT version, name FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version, &name)
	switch {
	case err == nil && version == currentSchemaVersion && name == currentSchemaName:
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit schema check: %w", err)
		}
		return nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("read schema version: %w", err)
	}

	for _, statement := range []string{
		`DROP TABLE IF EXISTS usage_events_v2`,
		`DROP TABLE IF EXISTS usage_events`,
		`DROP TABLE IF EXISTS client_api_key_metadata`,
		`DELETE FROM schema_migrations`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("reset historical usage schema: %w", err)
		}
	}

	if err := createFinalSchema(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
		currentSchemaVersion, currentSchemaName, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("record final schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit final schema initialization: %w", err)
	}
	return nil
}

func createFinalSchema(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE usage_events (
    event_id                    VARCHAR PRIMARY KEY,
    round_id                    BIGINT,
    started_at                  TIMESTAMPTZ NOT NULL,
    completed_at                TIMESTAMPTZ,
    usage_date                  DATE NOT NULL,

    api_key_id                  VARCHAR NOT NULL,
    provider                    VARCHAR,
    model                       VARCHAR,
    operation                   VARCHAR,
    route                       VARCHAR,
    client_endpoint             VARCHAR,
    client_protocol             VARCHAR,
    upstream_protocol           VARCHAR,
    upstream_endpoint           VARCHAR,
    conversion_mode             VARCHAR,
    conversion_level            INTEGER NOT NULL DEFAULT 0,
    conversion_duration_ms      BIGINT NOT NULL DEFAULT 0,
    conversion_degraded         BOOLEAN NOT NULL DEFAULT FALSE,
    ignored_features            VARCHAR,
    unsupported_features        VARCHAR,

    upstream_status             INTEGER,
    upstream_content_type       VARCHAR,
    upstream_content_length     BIGINT,
    upstream_transfer_encoding VARCHAR,

    input_tokens                BIGINT NOT NULL DEFAULT 0,
    output_tokens               BIGINT NOT NULL DEFAULT 0,
    total_tokens                BIGINT NOT NULL DEFAULT 0,
    cached_input_tokens         BIGINT NOT NULL DEFAULT 0,
    cache_creation_input_tokens BIGINT NOT NULL DEFAULT 0,

    http_status                 INTEGER,
    outcome                     VARCHAR,
    error_code                  VARCHAR,
    duration_ms                 BIGINT,
    upstream_duration_ms        BIGINT,
    stream                      BOOLEAN NOT NULL DEFAULT FALSE,
    estimated                   BOOLEAN NOT NULL DEFAULT FALSE,
    state                       VARCHAR NOT NULL,

    CHECK (state IN ('started', 'completed')),
    CHECK (state <> 'completed' OR (completed_at IS NOT NULL AND http_status IS NOT NULL AND outcome IS NOT NULL AND length(outcome) > 0)),
    CHECK (input_tokens >= 0),
    CHECK (output_tokens >= 0),
    CHECK (total_tokens >= 0),
    CHECK (cached_input_tokens >= 0),
    CHECK (cache_creation_input_tokens >= 0),
    CHECK (conversion_level >= 0),
    CHECK (conversion_duration_ms >= 0),
    CHECK (upstream_content_length IS NULL OR upstream_content_length >= 0)
)`,
		`CREATE INDEX idx_usage_events_started_at ON usage_events(started_at)`,
		`CREATE INDEX idx_usage_events_key_time ON usage_events(api_key_id, started_at)`,
		`CREATE INDEX idx_usage_events_date_key ON usage_events(usage_date, api_key_id)`,
		`CREATE INDEX idx_usage_events_provider_model ON usage_events(provider, model)`,
		`CREATE TABLE client_api_key_metadata (
    api_key_id      VARCHAR PRIMARY KEY,
    created_at      TIMESTAMPTZ NOT NULL,
    last_used_at    TIMESTAMPTZ,
    key_hash        VARCHAR,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    last_rotated_at TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ
)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create final usage schema: %w", err)
		}
	}
	return nil
}
