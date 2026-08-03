// Package searchhistory persists bounded Admin online-search history.
package searchhistory

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ai-proxy/internal/pkg/aiproxystate"

	"github.com/google/uuid"
)

const (
	defaultRetentionDays = 30
	defaultMaxItems      = 200
	maxListItems         = 100
	maxQueryBytes        = 8192
	maxOutputBytes       = 65536
	maxSources           = 32
	maxSourceFieldBytes  = 4096
)

// Config is intentionally internal policy rather than an Admin browser
// setting. Search answers can contain sensitive queries, so retention stays
// bounded even when temporary chat is disabled.
type Config struct {
	RetentionDays int
	MaxItems      int
}

type Store struct {
	docs *aiproxystate.Documents
	cfg  Config
	mu   sync.Mutex
}

type Source struct {
	Title   string `json:"title,omitempty"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

type Record struct {
	OwnerID     string
	Model       string
	ActualModel string
	Query       string
	OutputText  string
	Provider    string
	Sources     []Source
}

// Item is the bounded list projection. Full text and source URLs are absent
// until the owner explicitly selects one entry.
type Item struct {
	ID          string
	Model       string
	ActualModel string
	Query       string
	Provider    string
	CreatedAt   string
	ExpiresAt   string
}

type Detail struct {
	Item
	OutputText string
	Sources    []Source
}

func Open(database, memoryLimit string, threads int, cfg Config) (*Store, error) {
	docs, err := aiproxystate.Open(database, memoryLimit, threads)
	if err != nil {
		return nil, err
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = defaultRetentionDays
	}
	if cfg.MaxItems <= 0 {
		cfg.MaxItems = defaultMaxItems
	}
	return &Store{docs: docs, cfg: cfg}, nil
}

func (s *Store) Close() error {
	if s == nil || s.docs == nil {
		return nil
	}
	return s.docs.Close()
}

// Record stores only successful search results. Persistence failure is
// returned to the caller so Admin never falsely claims that a result will be
// available after a refresh.
func (s *Store) Record(input Record) (Detail, error) {
	if s == nil || s.docs == nil {
		return Detail{}, fmt.Errorf("search history is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	input.OwnerID = strings.TrimSpace(input.OwnerID)
	input.Model = strings.TrimSpace(input.Model)
	input.ActualModel = strings.TrimSpace(input.ActualModel)
	input.Query = trimUTF8(strings.TrimSpace(input.Query), maxQueryBytes)
	input.OutputText = trimUTF8(strings.TrimSpace(input.OutputText), maxOutputBytes)
	input.Provider = strings.TrimSpace(input.Provider)
	if input.OwnerID == "" || input.Model == "" || input.Query == "" || input.OutputText == "" || input.Provider == "" {
		return Detail{}, fmt.Errorf("search history record is incomplete")
	}
	sources := normalizeSources(input.Sources)
	rawSources, err := json.Marshal(sources)
	if err != nil {
		return Detail{}, fmt.Errorf("encode search history sources: %w", err)
	}
	now := time.Now().UTC()
	if _, err := s.docs.PurgeExpiredWebSearchHistory(now); err != nil {
		return Detail{}, err
	}
	row := aiproxystate.WebSearchHistoryRow{
		OwnerID: input.OwnerID, SearchID: uuid.NewString(), Model: input.Model, ActualModel: input.ActualModel,
		Query: input.Query, OutputText: input.OutputText, Provider: input.Provider, Sources: rawSources,
		CreatedAt: now, ExpiresAt: now.AddDate(0, 0, s.cfg.RetentionDays),
	}
	if err := s.docs.CreateWebSearchHistory(row); err != nil {
		return Detail{}, err
	}
	if _, err := s.docs.TrimWebSearchHistory(row.OwnerID, s.cfg.MaxItems); err != nil {
		return Detail{}, err
	}
	return detail(row, sources), nil
}

func (s *Store) List(ownerID string, limit int) ([]Item, error) {
	if s == nil || s.docs == nil {
		return nil, fmt.Errorf("search history is unavailable")
	}
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return nil, fmt.Errorf("search history owner is required")
	}
	if limit <= 0 || limit > maxListItems {
		limit = 50
	}
	if _, err := s.docs.PurgeExpiredWebSearchHistory(time.Now().UTC()); err != nil {
		return nil, err
	}
	rows, err := s.docs.ListWebSearchHistory(ownerID, limit)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(rows))
	for _, row := range rows {
		items = append(items, item(row))
	}
	return items, nil
}

func (s *Store) Get(ownerID, id string) (Detail, error) {
	if s == nil || s.docs == nil {
		return Detail{}, fmt.Errorf("search history is unavailable")
	}
	ownerID, id = strings.TrimSpace(ownerID), strings.TrimSpace(id)
	if ownerID == "" || id == "" {
		return Detail{}, fmt.Errorf("search history owner and id are required")
	}
	row, found, err := s.docs.LoadWebSearchHistory(ownerID, id)
	if err != nil {
		return Detail{}, err
	}
	if !found {
		return Detail{}, fmt.Errorf("search history entry not found")
	}
	var sources []Source
	if err := json.Unmarshal(row.Sources, &sources); err != nil {
		return Detail{}, fmt.Errorf("decode search history sources: %w", err)
	}
	return detail(row, sources), nil
}

func item(row aiproxystate.WebSearchHistoryRow) Item {
	return Item{
		ID: row.SearchID, Model: row.Model, ActualModel: row.ActualModel, Query: row.Query, Provider: row.Provider,
		CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339Nano), ExpiresAt: row.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

func detail(row aiproxystate.WebSearchHistoryRow, sources []Source) Detail {
	return Detail{Item: item(row), OutputText: row.OutputText, Sources: append([]Source(nil), sources...)}
}

func normalizeSources(values []Source) []Source {
	result := make([]Source, 0, min(len(values), maxSources))
	for _, source := range values {
		if len(result) >= maxSources {
			break
		}
		source = Source{
			Title:   trimUTF8(strings.TrimSpace(source.Title), maxSourceFieldBytes),
			URL:     trimUTF8(strings.TrimSpace(source.URL), maxSourceFieldBytes),
			Snippet: trimUTF8(strings.TrimSpace(source.Snippet), maxSourceFieldBytes),
		}
		if source.URL == "" {
			continue
		}
		result = append(result, source)
	}
	return result
}

func trimUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
