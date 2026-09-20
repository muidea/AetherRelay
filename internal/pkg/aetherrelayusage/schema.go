package usage

import (
	"context"
	"database/sql"
	"fmt"
)

// initializeSchema creates the final usage schema and verifies every column
// used by the runtime. Only additive observation flags extend the current
// schema; incompatible historical layouts fail without resetting their data.
func initializeSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema initialization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingTables int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables
WHERE table_name IN ('usage_events', 'client_api_key_metadata', 'client_api_key_provider_access')`).Scan(&existingTables); err != nil {
		return fmt.Errorf("inspect usage schema: %w", err)
	}
	if existingTables > 0 {
		if err := verifySchema(ctx, tx); err != nil {
			return err
		}
	}
	if err := createSchema(ctx, tx); err != nil {
		return err
	}
	if err := verifySchema(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema initialization: %w", err)
	}
	// DuckDB cannot reliably commit multiple ALTERs on a persisted indexed table
	// in one transaction. Each additive step is independently atomic/idempotent;
	// interruption is safe to resume and never rewrites existing usage or keys.
	for _, column := range []string{"cached_input_tokens_known", "cache_creation_input_tokens_known"} {
		if _, err := db.ExecContext(ctx, "ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS "+column+" BOOLEAN DEFAULT FALSE"); err != nil {
			return fmt.Errorf("add usage observation column: %w", err)
		}
	}
	return nil
}

func createSchema(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS usage_events (
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
    failure_class               VARCHAR,
    retryable                   BOOLEAN,
    retry_after_seconds         INTEGER,
    duration_ms                 BIGINT,
    first_event_duration_ms     BIGINT,
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
		`CREATE INDEX IF NOT EXISTS idx_usage_events_started_at ON usage_events(started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_events_key_time ON usage_events(api_key_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_events_date_key ON usage_events(usage_date, api_key_id)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_events_provider_model ON usage_events(provider, model)`,
		`CREATE TABLE IF NOT EXISTS client_api_key_metadata (
    api_key_id      VARCHAR PRIMARY KEY,
    created_at      TIMESTAMPTZ NOT NULL,
    last_used_at    TIMESTAMPTZ,
    key_hash        VARCHAR,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    last_rotated_at TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ,
    deleting_at     TIMESTAMPTZ,
    provider_access_mode VARCHAR NOT NULL,
    CHECK (provider_access_mode IN ('all', 'selected'))
)`,
		`CREATE TABLE IF NOT EXISTS client_api_key_provider_access (
    api_key_id  VARCHAR NOT NULL,
    provider_id VARCHAR NOT NULL,
    PRIMARY KEY (api_key_id, provider_id)
)`,
		`CREATE INDEX IF NOT EXISTS idx_client_api_key_provider_access_provider ON client_api_key_provider_access(provider_id)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create usage schema: %w", err)
		}
	}
	return nil
}

func verifySchema(ctx context.Context, tx *sql.Tx) error {
	probes := []string{
		`SELECT event_id, round_id, started_at, completed_at, usage_date,
api_key_id, provider, model, operation, route, client_endpoint, client_protocol,
upstream_protocol, upstream_endpoint, conversion_mode, conversion_level,
conversion_duration_ms, conversion_degraded, ignored_features, unsupported_features,
upstream_status, upstream_content_type, upstream_content_length, upstream_transfer_encoding,
input_tokens, output_tokens, total_tokens, cached_input_tokens, cache_creation_input_tokens,
http_status, outcome, error_code, failure_class, retryable, retry_after_seconds,
duration_ms, first_event_duration_ms, upstream_duration_ms, stream, estimated, state
FROM usage_events LIMIT 0`,
		`SELECT api_key_id, created_at, last_used_at, key_hash, enabled,
last_rotated_at, revoked_at, deleting_at, provider_access_mode
FROM client_api_key_metadata LIMIT 0`,
		`SELECT api_key_id, provider_id FROM client_api_key_provider_access LIMIT 0`,
	}
	for _, probe := range probes {
		rows, err := tx.QueryContext(ctx, probe)
		if err != nil {
			return fmt.Errorf("usage database does not match the final schema: %w", err)
		}
		_ = rows.Close()
	}
	return nil
}
