// Package aiproxystate provides the shared local DuckDB document substrate.
// Business owners keep their own schemas and document keys; this package owns
// only connection setup and atomic JSON document replacement.
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

type Documents struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string, memoryLimit string, threads int) (*Documents, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("state database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
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
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS state_documents (
        document_key VARCHAR PRIMARY KEY,
        payload JSON NOT NULL,
        updated_at TIMESTAMP NOT NULL DEFAULT current_timestamp
    )`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate state documents: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return &Documents{db: db}, nil
}

func (s *Documents) Load(key string, target any) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("state documents are unavailable")
	}
	var payload []byte
	err := s.db.QueryRow(`SELECT CAST(payload AS VARCHAR) FROM state_documents WHERE document_key = ?`, key).Scan(&payload)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(payload, target); err != nil {
		return false, fmt.Errorf("decode state document %q: %w", key, err)
	}
	return true, nil
}

func (s *Documents) Save(key string, value any) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("state documents are unavailable")
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec(`INSERT INTO state_documents(document_key, payload, updated_at)
	        VALUES (?, CAST(? AS JSON), NOW())
	        ON CONFLICT(document_key) DO UPDATE SET payload = excluded.payload, updated_at = NOW()`, key, string(payload))
	return err
}
