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
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
)

// SecureDocumentRow is an encrypted owner-scoped record. Payload must already
// be an authenticated encryption envelope; Documents never sees plaintext.
type SecureDocumentRow struct {
	ID       string
	Position int
	Payload  []byte
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

// TemporaryConversationRow is one Admin temporary text conversation.
type TemporaryConversationRow struct {
	OwnerID                string
	ConversationID         string
	Title                  string
	AccountID              string
	Provider               string
	Model                  string
	ActualModel            string
	ThinkingEffort         string
	SystemPrompt           string
	UpstreamConversationID string
	ParentMessageID        string
	Status                 string
	CreatedAt              time.Time
	UpdatedAt              time.Time
	ExpiresAt              time.Time
}

// TemporaryMessageRow is one bounded message inside a temporary conversation.
type TemporaryMessageRow struct {
	OwnerID            string
	ConversationID     string
	Sequence           int64
	MessageID          string
	Role               string
	Content            string
	ImageMetadata      json.RawMessage
	AttachmentMetadata json.RawMessage
	UpstreamMessageID  string
	ActualModel        string
	Status             string
	ErrorClass         string
	ErrorMessage       string
	CreatedAt          time.Time
	CompletedAt        *time.Time
}

// TemporaryMessageImageRow keeps temporary-chat attachment bytes separate
// from message text. The metadata that may be returned to Admin clients is
// stored on TemporaryMessageRow; raw bytes are only read through the
// owner-scoped lookup below.
type TemporaryMessageImageRow struct {
	OwnerID        string
	ConversationID string
	MessageID      string
	ImageID        string
	ContentType    string
	Bytes          []byte
}

type TemporaryMessageAttachmentRow struct {
	OwnerID        string
	ConversationID string
	MessageID      string
	AttachmentID   string
	FileName       string
	ContentType    string
	Bytes          []byte
}

// WebSearchHistoryRow is one bounded, owner-scoped Admin web-search result.
// The result body and sources are deliberately separate from the list query so
// history navigation cannot accidentally return an unbounded result set.
type WebSearchHistoryRow struct {
	OwnerID     string
	SearchID    string
	Model       string
	ActualModel string
	Query       string
	OutputText  string
	Provider    string
	Sources     json.RawMessage
	CreatedAt   time.Time
	ExpiresAt   time.Time
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
		`DROP TABLE IF EXISTS chatgpt_accounts`,
		`DROP TABLE IF EXISTS codex_oauth_accounts`,
		`CREATE TABLE IF NOT EXISTS secure_documents (
            scope VARCHAR NOT NULL,
            id VARCHAR NOT NULL,
            position BIGINT NOT NULL,
            payload BLOB NOT NULL,
            updated_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
            PRIMARY KEY (scope, id)
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
		`CREATE TABLE IF NOT EXISTS chatgpt_temporary_conversations (
            owner_id VARCHAR NOT NULL,
            conversation_id VARCHAR NOT NULL,
            title VARCHAR NOT NULL,
            account_id VARCHAR NOT NULL,
            model VARCHAR NOT NULL,
			actual_model VARCHAR NOT NULL DEFAULT '',
            thinking_effort VARCHAR NOT NULL DEFAULT '',
            system_prompt VARCHAR NOT NULL DEFAULT '',
            upstream_conversation_id VARCHAR NOT NULL DEFAULT '',
            parent_message_id VARCHAR NOT NULL DEFAULT '',
            status VARCHAR NOT NULL,
            created_at TIMESTAMP NOT NULL,
            updated_at TIMESTAMP NOT NULL,
            expires_at TIMESTAMP NOT NULL,
            PRIMARY KEY (owner_id, conversation_id)
        )`,
		`CREATE TABLE IF NOT EXISTS chatgpt_temporary_messages (
            owner_id VARCHAR NOT NULL,
            conversation_id VARCHAR NOT NULL,
            sequence BIGINT NOT NULL,
            message_id VARCHAR NOT NULL,
            role VARCHAR NOT NULL,
            content VARCHAR NOT NULL,
			image_metadata JSON NOT NULL DEFAULT '[]',
            upstream_message_id VARCHAR NOT NULL DEFAULT '',
			actual_model VARCHAR NOT NULL DEFAULT '',
            status VARCHAR NOT NULL,
            error_class VARCHAR NOT NULL DEFAULT '',
            error_message VARCHAR NOT NULL DEFAULT '',
            created_at TIMESTAMP NOT NULL,
            completed_at TIMESTAMP,
            PRIMARY KEY (owner_id, conversation_id, sequence)
        )`,
		`CREATE TABLE IF NOT EXISTS chatgpt_temporary_message_images (
            owner_id VARCHAR NOT NULL,
            conversation_id VARCHAR NOT NULL,
            message_id VARCHAR NOT NULL,
            image_id VARCHAR NOT NULL,
            content_type VARCHAR NOT NULL,
            bytes BLOB NOT NULL,
            PRIMARY KEY (owner_id, conversation_id, message_id, image_id)
		)`,
		`CREATE TABLE IF NOT EXISTS chatgpt_temporary_message_attachments (
            owner_id VARCHAR NOT NULL,
            conversation_id VARCHAR NOT NULL,
            message_id VARCHAR NOT NULL,
            attachment_id VARCHAR NOT NULL,
            file_name VARCHAR NOT NULL,
            content_type VARCHAR NOT NULL,
            bytes BLOB NOT NULL,
            PRIMARY KEY (owner_id, conversation_id, message_id, attachment_id)
        )`,
		`CREATE TABLE IF NOT EXISTS chatgpt_web_search_history (
            owner_id VARCHAR NOT NULL,
            search_id VARCHAR NOT NULL,
            model VARCHAR NOT NULL,
            actual_model VARCHAR NOT NULL DEFAULT '',
            query VARCHAR NOT NULL,
            output_text VARCHAR NOT NULL,
            provider VARCHAR NOT NULL,
            sources JSON NOT NULL DEFAULT '[]',
            created_at TIMESTAMP NOT NULL,
            expires_at TIMESTAMP NOT NULL,
            PRIMARY KEY (owner_id, search_id)
        )`,
		`CREATE INDEX IF NOT EXISTS idx_chatgpt_temporary_conversations_owner_updated
            ON chatgpt_temporary_conversations(owner_id, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_chatgpt_temporary_messages_owner_conversation_sequence
            ON chatgpt_temporary_messages(owner_id, conversation_id, sequence)`,
		`CREATE INDEX IF NOT EXISTS idx_chatgpt_temporary_message_images_owner_conversation_message
            ON chatgpt_temporary_message_images(owner_id, conversation_id, message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_chatgpt_temporary_message_attachments_owner_conversation_message
            ON chatgpt_temporary_message_attachments(owner_id, conversation_id, message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_chatgpt_web_search_history_owner_created
            ON chatgpt_web_search_history(owner_id, created_at DESC)`,
		`ALTER TABLE chatgpt_temporary_conversations ADD COLUMN IF NOT EXISTS actual_model VARCHAR DEFAULT ''`,
		`ALTER TABLE chatgpt_temporary_conversations ADD COLUMN IF NOT EXISTS provider VARCHAR DEFAULT 'chatgptweb'`,
		`ALTER TABLE chatgpt_temporary_messages ADD COLUMN IF NOT EXISTS actual_model VARCHAR DEFAULT ''`,
		`ALTER TABLE chatgpt_temporary_messages ADD COLUMN IF NOT EXISTS image_metadata JSON DEFAULT '[]'`,
		`ALTER TABLE chatgpt_temporary_messages ADD COLUMN IF NOT EXISTS attachment_metadata JSON DEFAULT '[]'`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("migrate state schema: %w", err)
		}
	}
	return nil
}

func (s *Documents) LoadSecureDocuments(scope string) ([]SecureDocumentRow, error) {
	if s == nil || s.shared == nil || strings.TrimSpace(scope) == "" {
		return nil, fmt.Errorf("secure document scope is required")
	}
	rows, err := s.shared.db.Query(`SELECT id, position, payload FROM secure_documents WHERE scope = ? ORDER BY position, id`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SecureDocumentRow
	for rows.Next() {
		var row SecureDocumentRow
		if err := rows.Scan(&row.ID, &row.Position, &row.Payload); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (s *Documents) ReplaceSecureDocuments(scope string, values []SecureDocumentRow) error {
	if s == nil || s.shared == nil || strings.TrimSpace(scope) == "" {
		return fmt.Errorf("secure document scope is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.shared.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM secure_documents WHERE scope = ?`, scope); err != nil {
		return err
	}
	for _, value := range values {
		if strings.TrimSpace(value.ID) == "" || len(value.Payload) == 0 {
			return fmt.Errorf("secure document id and payload are required")
		}
		if _, err := tx.Exec(`INSERT INTO secure_documents(scope, id, position, payload, updated_at) VALUES (?, ?, ?, ?, NOW())`, scope, value.ID, value.Position, value.Payload); err != nil {
			return err
		}
	}
	return tx.Commit()
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

func (s *Documents) CreateTemporaryConversation(row TemporaryConversationRow) error {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return fmt.Errorf("state database is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.shared.db.Exec(`INSERT INTO chatgpt_temporary_conversations(
		owner_id, conversation_id, title, account_id, provider, model, actual_model, thinking_effort, system_prompt,
		upstream_conversation_id, parent_message_id, status, created_at, updated_at, expires_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.OwnerID, row.ConversationID, row.Title, row.AccountID, row.Provider, row.Model, row.ActualModel, row.ThinkingEffort, row.SystemPrompt,
		row.UpstreamConversationID, row.ParentMessageID, row.Status, row.CreatedAt, row.UpdatedAt, row.ExpiresAt,
	)
	return err
}

func (s *Documents) ListTemporaryConversations(ownerID string, limit int, updatedBefore *time.Time) ([]TemporaryConversationRow, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return nil, fmt.Errorf("state database is unavailable")
	}
	if limit <= 0 {
		limit = 50
	}
	var (
		rows *sql.Rows
		err  error
	)
	if updatedBefore == nil {
		rows, err = s.shared.db.Query(`SELECT owner_id, conversation_id, title, account_id, provider, model, actual_model, thinking_effort, system_prompt,
			upstream_conversation_id, parent_message_id, status, created_at, updated_at, expires_at
			FROM chatgpt_temporary_conversations
			WHERE owner_id = ? AND status != 'closed' AND expires_at > NOW()
			ORDER BY updated_at DESC, conversation_id ASC
			LIMIT ?`, ownerID, limit)
	} else {
		rows, err = s.shared.db.Query(`SELECT owner_id, conversation_id, title, account_id, provider, model, actual_model, thinking_effort, system_prompt,
			upstream_conversation_id, parent_message_id, status, created_at, updated_at, expires_at
			FROM chatgpt_temporary_conversations
			WHERE owner_id = ? AND status != 'closed' AND expires_at > NOW() AND updated_at < ?
			ORDER BY updated_at DESC, conversation_id ASC
			LIMIT ?`, ownerID, *updatedBefore, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTemporaryConversations(rows)
}

func (s *Documents) LoadTemporaryConversation(ownerID, conversationID string) (TemporaryConversationRow, bool, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return TemporaryConversationRow{}, false, fmt.Errorf("state database is unavailable")
	}
	row := s.shared.db.QueryRow(`SELECT owner_id, conversation_id, title, account_id, provider, model, actual_model, thinking_effort, system_prompt,
		upstream_conversation_id, parent_message_id, status, created_at, updated_at, expires_at
		FROM chatgpt_temporary_conversations
		WHERE owner_id = ? AND conversation_id = ? AND status != 'closed'`, ownerID, conversationID)
	item, err := scanTemporaryConversation(row)
	if err == sql.ErrNoRows {
		return TemporaryConversationRow{}, false, nil
	}
	if err != nil {
		return TemporaryConversationRow{}, false, err
	}
	return item, true, nil
}

func (s *Documents) CountTemporaryConversations(ownerID string) (int, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return 0, fmt.Errorf("state database is unavailable")
	}
	var count int
	err := s.shared.db.QueryRow(`SELECT COUNT(*) FROM chatgpt_temporary_conversations WHERE owner_id = ? AND status != 'closed'`, ownerID).Scan(&count)
	return count, err
}

func (s *Documents) UpdateTemporaryConversation(row TemporaryConversationRow) error {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return fmt.Errorf("state database is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.shared.db.Exec(`UPDATE chatgpt_temporary_conversations SET
		title = ?, account_id = ?, provider = ?, model = ?, actual_model = ?, thinking_effort = ?, system_prompt = ?,
		upstream_conversation_id = ?, parent_message_id = ?, status = ?, updated_at = ?, expires_at = ?
		WHERE owner_id = ? AND conversation_id = ?`,
		row.Title, row.AccountID, row.Provider, row.Model, row.ActualModel, row.ThinkingEffort, row.SystemPrompt,
		row.UpstreamConversationID, row.ParentMessageID, row.Status, row.UpdatedAt, row.ExpiresAt,
		row.OwnerID, row.ConversationID,
	)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("temporary conversation not found")
	}
	return nil
}

// StartTemporaryTurn persists the two local messages and the streaming
// conversation state as one transaction. A process crash must never expose a
// half-created turn as an apparently usable conversation.
func (s *Documents) StartTemporaryTurn(conversation TemporaryConversationRow, user, assistant TemporaryMessageRow, images []TemporaryMessageImageRow, attachments []TemporaryMessageAttachmentRow) error {
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
	for _, message := range []TemporaryMessageRow{user, assistant} {
		if _, err := tx.Exec(`INSERT INTO chatgpt_temporary_messages(
			owner_id, conversation_id, sequence, message_id, role, content, image_metadata, attachment_metadata, upstream_message_id, actual_model,
			status, error_class, error_message, created_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			message.OwnerID, message.ConversationID, message.Sequence, message.MessageID, message.Role, message.Content, imageMetadata(message.ImageMetadata), imageMetadata(message.AttachmentMetadata), message.UpstreamMessageID, message.ActualModel,
			message.Status, message.ErrorClass, message.ErrorMessage, message.CreatedAt, message.CompletedAt,
		); err != nil {
			return err
		}
	}
	for _, attachment := range attachments {
		if _, err := tx.Exec(`INSERT INTO chatgpt_temporary_message_attachments(
			owner_id, conversation_id, message_id, attachment_id, file_name, content_type, bytes
		) VALUES (?, ?, ?, ?, ?, ?, ?)`, attachment.OwnerID, attachment.ConversationID, attachment.MessageID, attachment.AttachmentID, attachment.FileName, attachment.ContentType, attachment.Bytes); err != nil {
			return err
		}
	}
	for _, image := range images {
		if _, err := tx.Exec(`INSERT INTO chatgpt_temporary_message_images(
			owner_id, conversation_id, message_id, image_id, content_type, bytes
		) VALUES (?, ?, ?, ?, ?, ?)`, image.OwnerID, image.ConversationID, image.MessageID, image.ImageID, image.ContentType, image.Bytes); err != nil {
			return err
		}
	}
	res, err := tx.Exec(`UPDATE chatgpt_temporary_conversations SET
		title = ?, account_id = ?, provider = ?, model = ?, actual_model = ?, thinking_effort = ?, system_prompt = ?,
		upstream_conversation_id = ?, parent_message_id = ?, status = ?, updated_at = ?, expires_at = ?
		WHERE owner_id = ? AND conversation_id = ?`,
		conversation.Title, conversation.AccountID, conversation.Provider, conversation.Model, conversation.ActualModel, conversation.ThinkingEffort, conversation.SystemPrompt,
		conversation.UpstreamConversationID, conversation.ParentMessageID, conversation.Status, conversation.UpdatedAt, conversation.ExpiresAt,
		conversation.OwnerID, conversation.ConversationID,
	)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("temporary conversation not found")
	}
	return tx.Commit()
}

// CompleteTemporaryTurn commits both message terminal states and the
// continuation anchors atomically. The assistant anchor is authoritative for
// the next Web turn, so it cannot be persisted separately from its message.
func (s *Documents) CompleteTemporaryTurn(conversation TemporaryConversationRow, user, assistant TemporaryMessageRow) error {
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
	for _, message := range []TemporaryMessageRow{user, assistant} {
		res, err := tx.Exec(`UPDATE chatgpt_temporary_messages SET
			content = ?, upstream_message_id = ?, actual_model = ?, status = ?, error_class = ?, error_message = ?, completed_at = ?
			WHERE owner_id = ? AND conversation_id = ? AND sequence = ?`,
			message.Content, message.UpstreamMessageID, message.ActualModel, message.Status, message.ErrorClass, message.ErrorMessage, message.CompletedAt,
			message.OwnerID, message.ConversationID, message.Sequence,
		)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return fmt.Errorf("temporary message not found")
		}
	}
	res, err := tx.Exec(`UPDATE chatgpt_temporary_conversations SET
		title = ?, account_id = ?, model = ?, actual_model = ?, thinking_effort = ?, system_prompt = ?,
		upstream_conversation_id = ?, parent_message_id = ?, status = ?, updated_at = ?, expires_at = ?
		WHERE owner_id = ? AND conversation_id = ?`,
		conversation.Title, conversation.AccountID, conversation.Model, conversation.ActualModel, conversation.ThinkingEffort, conversation.SystemPrompt,
		conversation.UpstreamConversationID, conversation.ParentMessageID, conversation.Status, conversation.UpdatedAt, conversation.ExpiresAt,
		conversation.OwnerID, conversation.ConversationID,
	)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("temporary conversation not found")
	}
	return tx.Commit()
}

// InterruptTemporaryConversation atomically records recovery state for every
// unfinished message in one conversation. It is used after process restart
// and after teardown if a worker could not report its own terminal state.
func (s *Documents) InterruptTemporaryConversation(conversation TemporaryConversationRow, messages []TemporaryMessageRow) error {
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
	for _, message := range messages {
		res, err := tx.Exec(`UPDATE chatgpt_temporary_messages SET
			content = ?, upstream_message_id = ?, actual_model = ?, status = ?, error_class = ?, error_message = ?, completed_at = ?
			WHERE owner_id = ? AND conversation_id = ? AND sequence = ?`,
			message.Content, message.UpstreamMessageID, message.ActualModel, message.Status, message.ErrorClass, message.ErrorMessage, message.CompletedAt,
			message.OwnerID, message.ConversationID, message.Sequence,
		)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return fmt.Errorf("temporary message not found")
		}
	}
	res, err := tx.Exec(`UPDATE chatgpt_temporary_conversations SET
		title = ?, account_id = ?, model = ?, actual_model = ?, thinking_effort = ?, system_prompt = ?,
		upstream_conversation_id = ?, parent_message_id = ?, status = ?, updated_at = ?, expires_at = ?
		WHERE owner_id = ? AND conversation_id = ?`,
		conversation.Title, conversation.AccountID, conversation.Model, conversation.ActualModel, conversation.ThinkingEffort, conversation.SystemPrompt,
		conversation.UpstreamConversationID, conversation.ParentMessageID, conversation.Status, conversation.UpdatedAt, conversation.ExpiresAt,
		conversation.OwnerID, conversation.ConversationID,
	)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("temporary conversation not found")
	}
	return tx.Commit()
}

func (s *Documents) DeleteTemporaryConversation(ownerID, conversationID string) error {
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
	if _, err := tx.Exec(`DELETE FROM chatgpt_temporary_message_images WHERE owner_id = ? AND conversation_id = ?`, ownerID, conversationID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM chatgpt_temporary_message_attachments WHERE owner_id = ? AND conversation_id = ?`, ownerID, conversationID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM chatgpt_temporary_messages WHERE owner_id = ? AND conversation_id = ?`, ownerID, conversationID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM chatgpt_temporary_conversations WHERE owner_id = ? AND conversation_id = ?`, ownerID, conversationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Documents) AppendTemporaryMessage(row TemporaryMessageRow) error {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return fmt.Errorf("state database is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.shared.db.Exec(`INSERT INTO chatgpt_temporary_messages(
		owner_id, conversation_id, sequence, message_id, role, content, image_metadata, attachment_metadata, upstream_message_id, actual_model,
		status, error_class, error_message, created_at, completed_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.OwnerID, row.ConversationID, row.Sequence, row.MessageID, row.Role, row.Content, imageMetadata(row.ImageMetadata), imageMetadata(row.AttachmentMetadata), row.UpstreamMessageID, row.ActualModel,
		row.Status, row.ErrorClass, row.ErrorMessage, row.CreatedAt, row.CompletedAt,
	)
	return err
}

func (s *Documents) UpdateTemporaryMessage(row TemporaryMessageRow) error {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return fmt.Errorf("state database is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.shared.db.Exec(`UPDATE chatgpt_temporary_messages SET
		content = ?, upstream_message_id = ?, actual_model = ?, status = ?, error_class = ?, error_message = ?, completed_at = ?
		WHERE owner_id = ? AND conversation_id = ? AND sequence = ?`,
		row.Content, row.UpstreamMessageID, row.ActualModel, row.Status, row.ErrorClass, row.ErrorMessage, row.CompletedAt,
		row.OwnerID, row.ConversationID, row.Sequence,
	)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("temporary message not found")
	}
	return nil
}

// GetTemporaryMessageImage returns an attachment only when every owner and
// conversation identifier matches. Callers must turn a missing row into the
// same not-found response as an unknown message so attachment IDs cannot be
// enumerated across Admin owners.
func (s *Documents) GetTemporaryMessageImage(ownerID, conversationID, messageID, imageID string) (TemporaryMessageImageRow, bool, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return TemporaryMessageImageRow{}, false, fmt.Errorf("state database is unavailable")
	}
	row := TemporaryMessageImageRow{OwnerID: ownerID, ConversationID: conversationID, MessageID: messageID, ImageID: imageID}
	err := s.shared.db.QueryRow(`SELECT content_type, bytes
		FROM chatgpt_temporary_message_images
		WHERE owner_id = ? AND conversation_id = ? AND message_id = ? AND image_id = ?`, ownerID, conversationID, messageID, imageID).Scan(&row.ContentType, &row.Bytes)
	if err == sql.ErrNoRows {
		return TemporaryMessageImageRow{}, false, nil
	}
	if err != nil {
		return TemporaryMessageImageRow{}, false, err
	}
	return row, true, nil
}

func (s *Documents) GetTemporaryMessageAttachment(ownerID, conversationID, messageID, attachmentID string) (TemporaryMessageAttachmentRow, bool, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return TemporaryMessageAttachmentRow{}, false, fmt.Errorf("state database is unavailable")
	}
	row := TemporaryMessageAttachmentRow{OwnerID: ownerID, ConversationID: conversationID, MessageID: messageID, AttachmentID: attachmentID}
	err := s.shared.db.QueryRow(`SELECT file_name, content_type, bytes
		FROM chatgpt_temporary_message_attachments
		WHERE owner_id = ? AND conversation_id = ? AND message_id = ? AND attachment_id = ?`, ownerID, conversationID, messageID, attachmentID).Scan(&row.FileName, &row.ContentType, &row.Bytes)
	if err == sql.ErrNoRows {
		return TemporaryMessageAttachmentRow{}, false, nil
	}
	if err != nil {
		return TemporaryMessageAttachmentRow{}, false, err
	}
	return row, true, nil
}

func (s *Documents) ListTemporaryMessages(ownerID, conversationID string, beforeSequence *int64, limit int) ([]TemporaryMessageRow, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return nil, fmt.Errorf("state database is unavailable")
	}
	if limit <= 0 {
		limit = 50
	}
	var (
		rows *sql.Rows
		err  error
	)
	if beforeSequence == nil {
		rows, err = s.shared.db.Query(`SELECT owner_id, conversation_id, sequence, message_id, role, content, COALESCE(CAST(image_metadata AS VARCHAR), '[]'), COALESCE(CAST(attachment_metadata AS VARCHAR), '[]'), upstream_message_id, actual_model,
			status, error_class, error_message, created_at, completed_at
			FROM chatgpt_temporary_messages
			WHERE owner_id = ? AND conversation_id = ?
			ORDER BY sequence ASC
			LIMIT ?`, ownerID, conversationID, limit)
	} else {
		rows, err = s.shared.db.Query(`SELECT owner_id, conversation_id, sequence, message_id, role, content, COALESCE(CAST(image_metadata AS VARCHAR), '[]'), COALESCE(CAST(attachment_metadata AS VARCHAR), '[]'), upstream_message_id, actual_model,
			status, error_class, error_message, created_at, completed_at
			FROM chatgpt_temporary_messages
			WHERE owner_id = ? AND conversation_id = ? AND sequence < ?
			ORDER BY sequence DESC
			LIMIT ?`, ownerID, conversationID, *beforeSequence, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := scanTemporaryMessages(rows)
	if err != nil {
		return nil, err
	}
	if beforeSequence != nil {
		// reverse to ascending order for callers
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	}
	return items, nil
}

func (s *Documents) NextTemporaryMessageSequence(ownerID, conversationID string) (int64, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return 0, fmt.Errorf("state database is unavailable")
	}
	var max sql.NullInt64
	if err := s.shared.db.QueryRow(`SELECT MAX(sequence) FROM chatgpt_temporary_messages WHERE owner_id = ? AND conversation_id = ?`, ownerID, conversationID).Scan(&max); err != nil {
		return 0, err
	}
	if !max.Valid {
		return 1, nil
	}
	return max.Int64 + 1, nil
}

func (s *Documents) ListStreamingTemporaryConversations() ([]TemporaryConversationRow, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return nil, fmt.Errorf("state database is unavailable")
	}
	rows, err := s.shared.db.Query(`SELECT owner_id, conversation_id, title, account_id, provider, model, actual_model, thinking_effort, system_prompt,
		upstream_conversation_id, parent_message_id, status, created_at, updated_at, expires_at
		FROM chatgpt_temporary_conversations
		WHERE status = 'streaming'
		ORDER BY updated_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTemporaryConversations(rows)
}

func (s *Documents) ListStreamingTemporaryMessages(ownerID, conversationID string) ([]TemporaryMessageRow, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return nil, fmt.Errorf("state database is unavailable")
	}
	rows, err := s.shared.db.Query(`SELECT owner_id, conversation_id, sequence, message_id, role, content, COALESCE(CAST(image_metadata AS VARCHAR), '[]'), COALESCE(CAST(attachment_metadata AS VARCHAR), '[]'), upstream_message_id, actual_model,
		status, error_class, error_message, created_at, completed_at
		FROM chatgpt_temporary_messages
		WHERE owner_id = ? AND conversation_id = ? AND status = 'streaming'
		ORDER BY sequence ASC`, ownerID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTemporaryMessages(rows)
}

func (s *Documents) PurgeExpiredTemporaryConversations(now time.Time) (int, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return 0, fmt.Errorf("state database is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.shared.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT owner_id, conversation_id FROM chatgpt_temporary_conversations
		WHERE expires_at <= ? AND status != 'streaming'`, now)
	if err != nil {
		return 0, err
	}
	type key struct{ ownerID, conversationID string }
	var keys []key
	for rows.Next() {
		var item key
		if err := rows.Scan(&item.ownerID, &item.conversationID); err != nil {
			rows.Close()
			return 0, err
		}
		keys = append(keys, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, item := range keys {
		if _, err := tx.Exec(`DELETE FROM chatgpt_temporary_message_images WHERE owner_id = ? AND conversation_id = ?`, item.ownerID, item.conversationID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DELETE FROM chatgpt_temporary_message_attachments WHERE owner_id = ? AND conversation_id = ?`, item.ownerID, item.conversationID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DELETE FROM chatgpt_temporary_messages WHERE owner_id = ? AND conversation_id = ?`, item.ownerID, item.conversationID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DELETE FROM chatgpt_temporary_conversations WHERE owner_id = ? AND conversation_id = ?`, item.ownerID, item.conversationID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(keys), nil
}

// CreateWebSearchHistory stores one already-bounded successful Admin search.
// Callers must use the matching owner ID for every subsequent read.
func (s *Documents) CreateWebSearchHistory(row WebSearchHistoryRow) error {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return fmt.Errorf("state database is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.shared.db.Exec(`INSERT INTO chatgpt_web_search_history(
		owner_id, search_id, model, actual_model, query, output_text, provider, sources, created_at, expires_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.OwnerID, row.SearchID, row.Model, row.ActualModel, row.Query, row.OutputText, row.Provider, imageMetadata(row.Sources), row.CreatedAt, row.ExpiresAt,
	)
	return err
}

// ListWebSearchHistory returns metadata only. Full answers and source lists
// are retrieved through LoadWebSearchHistory for one owner-scoped record.
func (s *Documents) ListWebSearchHistory(ownerID string, limit int) ([]WebSearchHistoryRow, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return nil, fmt.Errorf("state database is unavailable")
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.shared.db.Query(`SELECT owner_id, search_id, model, actual_model, query, provider, created_at, expires_at
		FROM chatgpt_web_search_history
		WHERE owner_id = ? AND expires_at > NOW()
		ORDER BY created_at DESC, search_id DESC
		LIMIT ?`, ownerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]WebSearchHistoryRow, 0)
	for rows.Next() {
		var item WebSearchHistoryRow
		if err := rows.Scan(&item.OwnerID, &item.SearchID, &item.Model, &item.ActualModel, &item.Query, &item.Provider, &item.CreatedAt, &item.ExpiresAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Documents) LoadWebSearchHistory(ownerID, searchID string) (WebSearchHistoryRow, bool, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return WebSearchHistoryRow{}, false, fmt.Errorf("state database is unavailable")
	}
	var item WebSearchHistoryRow
	var sources string
	err := s.shared.db.QueryRow(`SELECT owner_id, search_id, model, actual_model, query, output_text, provider, CAST(sources AS VARCHAR), created_at, expires_at
		FROM chatgpt_web_search_history
		WHERE owner_id = ? AND search_id = ? AND expires_at > NOW()`, ownerID, searchID).Scan(
		&item.OwnerID, &item.SearchID, &item.Model, &item.ActualModel, &item.Query, &item.OutputText, &item.Provider, &sources, &item.CreatedAt, &item.ExpiresAt,
	)
	if err == sql.ErrNoRows {
		return WebSearchHistoryRow{}, false, nil
	}
	if err != nil {
		return WebSearchHistoryRow{}, false, err
	}
	item.Sources = json.RawMessage(sources)
	return item, true, nil
}

func (s *Documents) PurgeExpiredWebSearchHistory(now time.Time) (int, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return 0, fmt.Errorf("state database is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.shared.db.Exec(`DELETE FROM chatgpt_web_search_history WHERE expires_at <= ?`, now)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	return int(count), err
}

// TrimWebSearchHistory drops the oldest entries for one owner after a new
// record has been accepted. A non-positive maxItems intentionally removes all
// matching rows so a caller never accidentally retains unbounded history.
func (s *Documents) TrimWebSearchHistory(ownerID string, maxItems int) (int, error) {
	if s == nil || s.shared == nil || s.shared.db == nil {
		return 0, fmt.Errorf("state database is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.shared.db.Query(`SELECT search_id FROM chatgpt_web_search_history
		WHERE owner_id = ?
		ORDER BY created_at DESC, search_id DESC
		OFFSET ?`, ownerID, max(0, maxItems))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := s.shared.db.Exec(`DELETE FROM chatgpt_web_search_history WHERE owner_id = ? AND search_id = ?`, ownerID, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTemporaryConversation(scanner rowScanner) (TemporaryConversationRow, error) {
	var row TemporaryConversationRow
	err := scanner.Scan(
		&row.OwnerID, &row.ConversationID, &row.Title, &row.AccountID, &row.Provider, &row.Model, &row.ActualModel, &row.ThinkingEffort, &row.SystemPrompt,
		&row.UpstreamConversationID, &row.ParentMessageID, &row.Status, &row.CreatedAt, &row.UpdatedAt, &row.ExpiresAt,
	)
	return row, err
}

func scanTemporaryConversations(rows *sql.Rows) ([]TemporaryConversationRow, error) {
	var result []TemporaryConversationRow
	for rows.Next() {
		row, err := scanTemporaryConversation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func scanTemporaryMessage(scanner rowScanner) (TemporaryMessageRow, error) {
	var row TemporaryMessageRow
	var completedAt sql.NullTime
	var imageMetadata, attachmentMetadata string
	err := scanner.Scan(
		&row.OwnerID, &row.ConversationID, &row.Sequence, &row.MessageID, &row.Role, &row.Content, &imageMetadata, &attachmentMetadata, &row.UpstreamMessageID, &row.ActualModel,
		&row.Status, &row.ErrorClass, &row.ErrorMessage, &row.CreatedAt, &completedAt,
	)
	if err != nil {
		return TemporaryMessageRow{}, err
	}
	if completedAt.Valid {
		ts := completedAt.Time
		row.CompletedAt = &ts
	}
	row.ImageMetadata = json.RawMessage(imageMetadata)
	row.AttachmentMetadata = json.RawMessage(attachmentMetadata)
	return row, nil
}

func scanTemporaryMessages(rows *sql.Rows) ([]TemporaryMessageRow, error) {
	var result []TemporaryMessageRow
	for rows.Next() {
		row, err := scanTemporaryMessage(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func imageMetadata(value json.RawMessage) []byte {
	if len(value) == 0 {
		return []byte("[]")
	}
	return []byte(value)
}
