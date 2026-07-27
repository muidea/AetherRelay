// Package aiproxystate owns the shared local DuckDB connection setup and the
// schemas for durable, owner-scoped ai-proxy state.
package aiproxystate

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "github.com/duckdb/duckdb-go/v2"
)

// AccountRow is the persisted account-pool record. Position preserves the
// account owner's round-robin ordering without making it part of JSON.
type AccountRow struct {
	AccessToken string
	Position    int
	Payload     json.RawMessage
}

// ImageTaskRow is a task record scoped by its owner and client task ID.
type ImageTaskRow struct {
	OwnerID string
	TaskID  string
	Payload json.RawMessage
}

// ImageRow contains the queryable image index fields as columns. Payload is
// retained only for owner-private image metadata, not as a whole-store blob.
type ImageRow struct {
	Path      string
	Size      int64
	Width     int
	Height    int
	CreatedAt string
	Payload   json.RawMessage
}

// Documents is a narrow state database handle. It intentionally exposes no
// catch-all document-key API: each business owner writes only its own table.
type Documents struct {
	shared *sharedDatabase
	mu     sync.Mutex
	close  sync.Once
}

type sharedDatabase struct {
	db          *sql.DB
	memoryLimit string
	threads     int
	references  int
}

var (
	sharedDatabasesMu sync.Mutex
	sharedDatabases   = map[string]*sharedDatabase{}
)

func Open(path, memoryLimit string, threads int) (*Documents, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("state database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	memoryLimit = strings.TrimSpace(memoryLimit)
	sharedDatabasesMu.Lock()
	defer sharedDatabasesMu.Unlock()
	if existing := sharedDatabases[path]; existing != nil {
		if existing.memoryLimit != memoryLimit || existing.threads != threads {
			return nil, fmt.Errorf("state database %q is already open with different resource settings", path)
		}
		existing.references++
		return &Documents{shared: existing}, nil
	}
	db, err := openDatabase(path, memoryLimit, threads)
	if err != nil {
		return nil, err
	}
	shared := &sharedDatabase{db: db, memoryLimit: memoryLimit, threads: threads, references: 1}
	sharedDatabases[path] = shared
	_ = os.Chmod(path, 0o600)
	return &Documents{shared: shared}, nil
}

func openDatabase(path, memoryLimit string, threads int) (*sql.DB, error) {
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect state database: %w", err)
	}
	if _, err := db.Exec("SET enable_external_access = false"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure state database: %w", err)
	}
	if memoryLimit != "" {
		if _, err := db.Exec("SET memory_limit = '" + strings.ReplaceAll(memoryLimit, "'", "") + "'"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure state memory limit: %w", err)
		}
	}
	if threads > 0 {
		if _, err := db.Exec(fmt.Sprintf("SET threads = %d", threads)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure state threads: %w", err)
		}
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS chatgpt_accounts (
            access_token VARCHAR PRIMARY KEY,
            position BIGINT NOT NULL,
            payload JSON NOT NULL,
            updated_at TIMESTAMP NOT NULL DEFAULT current_timestamp
        )`,
		`CREATE TABLE IF NOT EXISTS chatgpt_image_tasks (
            owner_id VARCHAR NOT NULL,
            task_id VARCHAR NOT NULL,
            payload JSON NOT NULL,
            updated_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
            PRIMARY KEY (owner_id, task_id)
        )`,
		`CREATE TABLE IF NOT EXISTS chatgpt_images (
            path VARCHAR PRIMARY KEY,
            size BIGINT NOT NULL,
            width INTEGER NOT NULL,
            height INTEGER NOT NULL,
            created_at VARCHAR NOT NULL,
            payload JSON NOT NULL,
            updated_at TIMESTAMP NOT NULL DEFAULT current_timestamp
        )`,
		`CREATE TABLE IF NOT EXISTS chatgpt_image_tags (
            path VARCHAR PRIMARY KEY,
            tags JSON NOT NULL,
            updated_at TIMESTAMP NOT NULL DEFAULT current_timestamp
        )`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("migrate state schema: %w", err)
		}
	}
	return nil
}

func (s *Documents) Close() error {
	if s == nil || s.shared == nil {
		return nil
	}
	var err error
	s.close.Do(func() {
		sharedDatabasesMu.Lock()
		defer sharedDatabasesMu.Unlock()
		s.shared.references--
		if s.shared.references == 0 {
			for path, candidate := range sharedDatabases {
				if candidate == s.shared {
					delete(sharedDatabases, path)
					break
				}
			}
			err = s.shared.db.Close()
		}
	})
	return err
}

func (s *Documents) LoadAccounts() ([]AccountRow, error) {
	rows, err := s.queryRows(`SELECT access_token, position, CAST(payload AS VARCHAR) FROM chatgpt_accounts ORDER BY position, access_token`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AccountRow
	for rows.Next() {
		var row AccountRow
		var payload string
		if err := rows.Scan(&row.AccessToken, &row.Position, &payload); err != nil {
			return nil, err
		}
		row.Payload = json.RawMessage(payload)
		result = append(result, row)
	}
	return result, rows.Err()
}

func (s *Documents) ReplaceAccounts(values []AccountRow) error {
	return s.replace("DELETE FROM chatgpt_accounts", func(tx *sql.Tx) error {
		for _, value := range values {
			if _, err := tx.Exec(`INSERT INTO chatgpt_accounts(access_token, position, payload, updated_at) VALUES (?, ?, CAST(? AS JSON), NOW())`, value.AccessToken, value.Position, string(value.Payload)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Documents) LoadImageTasks() ([]ImageTaskRow, error) {
	rows, err := s.queryRows(`SELECT owner_id, task_id, CAST(payload AS VARCHAR) FROM chatgpt_image_tasks ORDER BY owner_id, task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ImageTaskRow
	for rows.Next() {
		var row ImageTaskRow
		var payload string
		if err := rows.Scan(&row.OwnerID, &row.TaskID, &payload); err != nil {
			return nil, err
		}
		row.Payload = json.RawMessage(payload)
		result = append(result, row)
	}
	return result, rows.Err()
}

func (s *Documents) ReplaceImageTasks(values []ImageTaskRow) error {
	return s.replace("DELETE FROM chatgpt_image_tasks", func(tx *sql.Tx) error {
		for _, value := range values {
			if _, err := tx.Exec(`INSERT INTO chatgpt_image_tasks(owner_id, task_id, payload, updated_at) VALUES (?, ?, CAST(? AS JSON), NOW())`, value.OwnerID, value.TaskID, string(value.Payload)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Documents) LoadImages() ([]ImageRow, error) {
	rows, err := s.queryRows(`SELECT path, size, width, height, created_at, CAST(payload AS VARCHAR) FROM chatgpt_images ORDER BY created_at DESC, path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ImageRow
	for rows.Next() {
		var row ImageRow
		var payload string
		if err := rows.Scan(&row.Path, &row.Size, &row.Width, &row.Height, &row.CreatedAt, &payload); err != nil {
			return nil, err
		}
		row.Payload = json.RawMessage(payload)
		result = append(result, row)
	}
	return result, rows.Err()
}

func (s *Documents) ReplaceImages(values []ImageRow) error {
	return s.replace("DELETE FROM chatgpt_images", func(tx *sql.Tx) error {
		for _, value := range values {
			if _, err := tx.Exec(`INSERT INTO chatgpt_images(path, size, width, height, created_at, payload, updated_at) VALUES (?, ?, ?, ?, ?, CAST(? AS JSON), NOW())`, value.Path, value.Size, value.Width, value.Height, value.CreatedAt, string(value.Payload)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Documents) LoadImageTags() (map[string][]string, error) {
	rows, err := s.queryRows(`SELECT path, CAST(tags AS VARCHAR) FROM chatgpt_image_tags ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string][]string{}
	for rows.Next() {
		var path string
		var raw string
		if err := rows.Scan(&path, &raw); err != nil {
			return nil, err
		}
		var tags []string
		if err := json.Unmarshal([]byte(raw), &tags); err != nil {
			return nil, fmt.Errorf("decode image tags for %q: %w", path, err)
		}
		result[path] = tags
	}
	return result, rows.Err()
}

func (s *Documents) ReplaceImageTags(values map[string][]string) error {
	return s.replace("DELETE FROM chatgpt_image_tags", func(tx *sql.Tx) error {
		for path, tags := range values {
			payload, err := json.Marshal(tags)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO chatgpt_image_tags(path, tags, updated_at) VALUES (?, CAST(? AS JSON), NOW())`, path, string(payload)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Documents) queryRows(query string) (*sql.Rows, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return nil, fmt.Errorf("state database is unavailable")
	}
	return s.shared.db.Query(query)
}

func (s *Documents) replace(deleteStatement string, insert func(*sql.Tx) error) error {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return fmt.Errorf("state database is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.shared.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(deleteStatement); err != nil {
		return err
	}
	if err := insert(tx); err != nil {
		return err
	}
	return tx.Commit()
}
