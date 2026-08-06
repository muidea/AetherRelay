package usage

import (
	"context"
	"errors"
	"time"
)

var errClientAPIKeyNotFound = errors.New("client api key not found")

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
func (s *MemoryStore) ListClientAPIKeys(_ context.Context) (map[string]ClientAPIKeyRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := map[string]ClientAPIKeyRecord{}
	for id, m := range s.clientKeyRecords {
		o[id] = m
	}
	for id, m := range s.clientKeys {
		if _, ok := o[id]; !ok {
			o[id] = ClientAPIKeyRecord{ID: id, Enabled: true, CreatedAt: m.CreatedAt, LastUsedAt: m.LastUsedAt}
		}
	}
	return o, nil
}
func (s *MemoryStore) CreateClientAPIKey(_ context.Context, r ClientAPIKeyRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clientKeys == nil {
		s.clientKeys = map[string]ClientAPIKeyMetadata{}
	}
	if s.clientKeyRecords == nil {
		s.clientKeyRecords = map[string]ClientAPIKeyRecord{}
	}
	if _, exists := s.clientKeyRecords[r.ID]; exists {
		return ErrDuplicateEvent
	}
	s.clientKeys[r.ID] = ClientAPIKeyMetadata{ID: r.ID, CreatedAt: r.CreatedAt}
	s.clientKeyRecords[r.ID] = r
	return nil
}
func (s *MemoryStore) SetClientAPIKeyEnabled(_ context.Context, id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.clientKeyRecords[id]
	if !ok {
		return errClientAPIKeyNotFound
	}
	r.Enabled = enabled
	s.clientKeyRecords[id] = r
	return nil
}
func (s *MemoryStore) RotateClientAPIKey(_ context.Context, id, hash string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.clientKeyRecords[id]
	if !ok {
		return errClientAPIKeyNotFound
	}
	r.Hash, r.Enabled, r.LastRotatedAt, r.RevokedAt = hash, true, &t, nil
	s.clientKeyRecords[id] = r
	return nil
}
func (s *MemoryStore) RevokeClientAPIKey(_ context.Context, id string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.clientKeyRecords[id]
	if !ok {
		return errClientAPIKeyNotFound
	}
	r.Enabled = false
	r.RevokedAt = &t
	s.clientKeyRecords[id] = r
	return nil
}
