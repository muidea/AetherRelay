package usage

import (
	"context"
	"time"
)

func (s *MemoryStore) EnsureClientAPIKey(_ context.Context, id string, createdAt time.Time) error {
	if s == nil || id == "" {
		return nil
	}
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clientKeys == nil {
		s.clientKeys = map[string]ClientAPIKeyMetadata{}
	}
	if _, ok := s.clientKeys[id]; !ok {
		s.clientKeys[id] = ClientAPIKeyMetadata{ID: id, CreatedAt: createdAt.UTC()}
	}
	return nil
}
func (s *MemoryStore) TouchClientAPIKey(_ context.Context, id string, usedAt time.Time) error {
	if s == nil || id == "" {
		return nil
	}
	if usedAt.IsZero() {
		usedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clientKeys == nil {
		s.clientKeys = map[string]ClientAPIKeyMetadata{}
	}
	item, ok := s.clientKeys[id]
	if !ok {
		item = ClientAPIKeyMetadata{ID: id, CreatedAt: usedAt.UTC()}
	}
	value := usedAt.UTC()
	item.LastUsedAt = &value
	s.clientKeys[id] = item
	return nil
}
func (s *MemoryStore) ClientAPIKeyMetadata(_ context.Context) (map[string]ClientAPIKeyMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := map[string]ClientAPIKeyMetadata{}
	for id, item := range s.clientKeys {
		result[id] = item
	}
	return result, nil
}
