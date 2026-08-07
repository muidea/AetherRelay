package providerstore

import (
	"encoding/json"
	"fmt"
	"sort"

	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxycredential"
	"ai-proxy/internal/pkg/aiproxystate"
)

const (
	secureDocumentScope = "provider_catalog"
	catalogDocumentID   = "catalog"
)

type storedProvider struct {
	Name                 string                                                 `json:"name"`
	Protocol             string                                                 `json:"protocol"`
	BaseURL              string                                                 `json:"base_url"`
	APIKey               string                                                 `json:"api_key"`
	Models               []string                                               `json:"models"`
	Priority             int                                                    `json:"priority"`
	Fallback             bool                                                   `json:"fallback"`
	Endpoints            []string                                               `json:"endpoints"`
	AllowUnauthenticated bool                                                   `json:"allow_unauthenticated"`
	ConversionReleases   map[string]map[string]config.ProviderConversionRelease `json:"conversion_releases,omitempty"`
	Disabled             bool                                                   `json:"disabled"`
}

type Store struct {
	documents *aiproxystate.Documents
	codec     *aiproxycredential.Codec
}

// Initialized checks only whether an encrypted Provider catalog exists. It
// never reads plaintext and is used to distinguish a new installation from a
// missing-key recovery failure.
func Initialized(databasePath, memoryLimit string, threads int) (bool, error) {
	documents, err := aiproxystate.Open(databasePath, memoryLimit, threads)
	if err != nil {
		return false, err
	}
	defer documents.Close()
	rows, err := documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil {
		return false, err
	}
	return len(rows) != 0, nil
}

func Open(databasePath, memoryLimit string, threads int, codec *aiproxycredential.Codec) (*Store, error) {
	if codec == nil {
		return nil, fmt.Errorf("provider credential codec is required")
	}
	documents, err := aiproxystate.Open(databasePath, memoryLimit, threads)
	if err != nil {
		return nil, err
	}
	return &Store{documents: documents, codec: codec}, nil
}

func (s *Store) Close() error {
	if s == nil || s.documents == nil {
		return nil
	}
	return s.documents.Close()
}

func (s *Store) Load() (map[string]config.Provider, bool, error) {
	rows, err := s.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	if len(rows) != 1 || rows[0].ID != catalogDocumentID {
		return nil, false, fmt.Errorf("provider catalog storage is inconsistent")
	}
	payload, err := s.codec.Open(secureDocumentScope, catalogDocumentID, rows[0].Payload)
	if err != nil {
		return nil, false, err
	}
	var stored []storedProvider
	if err := json.Unmarshal(payload, &stored); err != nil {
		return nil, false, fmt.Errorf("decode provider catalog: %w", err)
	}
	providers := make(map[string]config.Provider, len(stored))
	for _, value := range stored {
		provider := config.Provider{Name: value.Name, Protocol: value.Protocol, BaseURL: value.BaseURL, APIKey: value.APIKey, Models: append([]string(nil), value.Models...), Endpoints: append([]string(nil), value.Endpoints...), AllowUnauthenticated: value.AllowUnauthenticated, ConversionReleases: cloneConversionReleases(value.ConversionReleases), Disabled: value.Disabled}
		config.ConfigureProviderPolicy(&provider, value.Priority, value.Fallback)
		providers[value.Name] = provider
	}
	return providers, true, nil
}

func (s *Store) Replace(providers map[string]config.Provider) error {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	stored := make([]storedProvider, 0, len(names))
	for _, name := range names {
		provider := providers[name]
		stored = append(stored, storedProvider{Name: name, Protocol: provider.Protocol, BaseURL: provider.BaseURL, APIKey: provider.APIKey, Models: append([]string(nil), provider.Models...), Priority: config.EffectiveProviderPriority(provider), Fallback: config.EffectiveProviderFallback(provider), Endpoints: append([]string(nil), provider.Endpoints...), AllowUnauthenticated: provider.AllowUnauthenticated, ConversionReleases: cloneConversionReleases(provider.ConversionReleases), Disabled: provider.Disabled})
	}
	payload, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	sealed, err := s.codec.Seal(secureDocumentScope, catalogDocumentID, payload)
	if err != nil {
		return err
	}
	return s.documents.ReplaceSecureDocuments(secureDocumentScope, []aiproxystate.SecureDocumentRow{{ID: catalogDocumentID, Payload: sealed}})
}

func cloneConversionReleases(input map[string]map[string]config.ProviderConversionRelease) map[string]map[string]config.ProviderConversionRelease {
	if input == nil {
		return nil
	}
	result := make(map[string]map[string]config.ProviderConversionRelease, len(input))
	for modelID, directions := range input {
		copied := make(map[string]config.ProviderConversionRelease, len(directions))
		for direction, release := range directions {
			copied[direction] = release
		}
		result[modelID] = copied
	}
	return result
}
