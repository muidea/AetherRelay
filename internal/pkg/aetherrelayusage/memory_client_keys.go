package usage

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"aetherrelay/internal/pkg/aetherrelayclientaccess"
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
		if s.clientKeyRecords == nil {
			s.clientKeyRecords = map[string]ClientAPIKeyRecord{}
		}
		s.clientKeyRecords[id] = ClientAPIKeyRecord{ID: id, Enabled: true, CreatedAt: createdAt.UTC(), ProviderAccess: clientaccess.All()}
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
		m.ProviderAccess = clientaccess.Clone(m.ProviderAccess)
		o[id] = m
	}
	for id, m := range s.clientKeys {
		if _, ok := o[id]; !ok {
			o[id] = ClientAPIKeyRecord{ID: id, Enabled: true, CreatedAt: m.CreatedAt, LastUsedAt: m.LastUsedAt, ProviderAccess: clientaccess.All()}
		}
	}
	return o, nil
}
func (s *MemoryStore) CreateClientAPIKey(_ context.Context, r ClientAPIKeyRecord) error {
	policy, err := clientaccess.Normalize(r.ProviderAccess)
	if err != nil {
		return err
	}
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
	r.ProviderAccess = policy
	s.clientKeyRecords[r.ID] = r
	return nil
}
func (s *MemoryStore) SetClientAPIKeyProviderAccess(_ context.Context, id string, value clientaccess.Policy) error {
	policy, err := clientaccess.Normalize(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.clientKeyRecords[id]
	if !ok {
		return errClientAPIKeyNotFound
	}
	record.ProviderAccess = policy
	s.clientKeyRecords[id] = record
	return nil
}
func (s *MemoryStore) ClientAPIKeyIDsForProvider(_ context.Context, providerID string) ([]string, error) {
	providerID = strings.ToLower(strings.TrimSpace(providerID))
	s.mu.Lock()
	defer s.mu.Unlock()
	result := []string{}
	for id, record := range s.clientKeyRecords {
		if record.ProviderAccess.Mode == clientaccess.ModeSelected && record.ProviderAccess.Allows(providerID) {
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result, nil
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

func (s *MemoryStore) DeleteClientAPIKey(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clientKeyRecords[id]; !ok {
		return errClientAPIKeyNotFound
	}
	delete(s.clientKeyRecords, id)
	delete(s.clientKeys, id)
	for eventID, item := range s.events {
		if item != nil && item.APIKeyID == id {
			delete(s.events, eventID)
		}
	}
	return nil
}
