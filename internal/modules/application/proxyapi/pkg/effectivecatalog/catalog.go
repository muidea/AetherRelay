// Package effectivecatalog holds the pure request-time routing catalog
// synthesized from static config and ChatGPT Web account-pool discovery.
// It contains no EventHub, storage, HTTP, or mutable handles.
package effectivecatalog

import (
	"sort"
	"strings"
	"time"

	"ai-proxy/internal/pkg/aiproxyconfig"
)

const (
	BuiltinProviderID    = "chatgptweb"
	CodexOAuthProviderID = "codexoauth"
	CodexOAuthPriority   = config.DefaultCodexOAuthProviderPriority
	ChatGPTWebPriority   = config.DefaultChatGPTWebProviderPriority
)

// BuiltinProviderStatus is a coarse health projection for Admin and routing.
type BuiltinProviderStatus string

const (
	StatusDisabled    BuiltinProviderStatus = "disabled"
	StatusEmpty       BuiltinProviderStatus = "empty"
	StatusDiscovering BuiltinProviderStatus = "discovering"
	StatusReady       BuiltinProviderStatus = "ready"
	StatusDegraded    BuiltinProviderStatus = "degraded"
)

// BuiltinModel is one auto-discovered model bound to the chatgptweb owner.
type BuiltinModel struct {
	ID        string
	CreatedAt int64
	OwnedBy   string
	// ConflictWithStatic is true when another source publishes the same exact
	// model ID. It is display-only overlap information; both sources remain in
	// the effective candidate chain according to their routing policy.
	ConflictWithStatic bool
}

// BuiltinProvider is the non-persistent chatgptweb provider projection.
type BuiltinProvider struct {
	ID                string
	Enabled           bool
	Status            BuiltinProviderStatus
	AvailableAccounts int
	ModelCount        int
	ConflictCount     int
	ConflictModels    []string
	UpdatedAt         string
	UnavailableReason string
}

// Snapshot is the atomic read model shared by /v1/models and ResolveTransportPlan.
type Snapshot struct {
	StaticModels    map[string]config.ModelMetadata
	BuiltinProvider BuiltinProvider
	BuiltinModels   map[string]BuiltinModel
	// Codex OAuth models are discovered and cached by the independent Codex
	// account-pool owner; the account-level snapshot is their only authority.
	CodexOAuthProvider BuiltinProvider
	CodexOAuthModels   map[string]BuiltinModel
	// Candidates is the immutable model routing authority. Endpoint compatibility
	// is resolved later by the shared provider transport matrix.
	Candidates map[string][]Candidate
	// Version is the account-pool catalog generation used to build this snapshot.
	Version           uint64
	CodexOAuthVersion uint64
}

// Candidate is one eligible upstream source for a model.
type Candidate struct {
	ModelID             string
	RouteOwner          string
	Builtin             bool
	CreatedAt           int64
	OwnedBy             string
	Priority            int
	Fallback            bool
	ContextWindowTokens int
	MaxOutputTokens     int
	// SupportedEndpoints is the generation-consistent client path projection
	// calculated from the provider transport matrix when the snapshot is built.
	SupportedEndpoints []string
	// ConversionModes contains only conversion directions that this concrete
	// provider candidate can execute for the model.
	ConversionModes []string
}

// Route is the request-time resolved model route from either static or builtin.
type Route struct {
	ModelID    string
	RouteOwner string
	Builtin    bool
	CreatedAt  int64
	OwnedBy    string
	// Optional capacity fields; zero means unknown / omit from /v1/models.
	ContextWindowTokens int
	MaxOutputTokens     int
}

// Build constructs a snapshot from static config and account-pool catalog models.
// Same-ID sources are retained as ordered candidates rather than discarded.
func Build(cfg config.Config, poolVersion uint64, availableAccounts int, poolModels []PoolModel, updatedAt string) Snapshot {
	return BuildWithCodex(cfg, CatalogInput{Version: poolVersion, AvailableAccounts: availableAccounts, Models: poolModels, UpdatedAt: updatedAt}, CatalogInput{})
}

// CatalogInput is the constrained union emitted by one account-pool owner.
// It contains no EventHub or persistence handles.
type CatalogInput struct {
	Version           uint64
	AvailableAccounts int
	Models            []PoolModel
	UpdatedAt         string
}

// BuildWithCodex constructs both builtin projections from their separate,
// account-scoped discovery caches. Same-ID sources remain ordered candidates.
func BuildWithCodex(cfg config.Config, chatGPT, codex CatalogInput) Snapshot {
	static := exactProviderModels(cfg)
	snap := Snapshot{
		StaticModels:  static,
		Version:       chatGPT.Version,
		BuiltinModels: map[string]BuiltinModel{},
		BuiltinProvider: BuiltinProvider{
			ID:                BuiltinProviderID,
			Enabled:           config.EffectiveChatGPTWebProviderEnabled(cfg.ChatGPTWeb),
			AvailableAccounts: chatGPT.AvailableAccounts,
			UpdatedAt:         strings.TrimSpace(chatGPT.UpdatedAt),
		},
		CodexOAuthModels:   map[string]BuiltinModel{},
		Candidates:         map[string][]Candidate{},
		CodexOAuthProvider: BuiltinProvider{ID: CodexOAuthProviderID, Enabled: config.EffectiveCodexOAuthProviderEnabled(cfg.CodexOAuth), AvailableAccounts: codex.AvailableAccounts, UpdatedAt: strings.TrimSpace(codex.UpdatedAt)},
		CodexOAuthVersion:  codex.Version,
	}
	if !config.EffectiveChatGPTWebProviderEnabled(cfg.ChatGPTWeb) {
		snap.BuiltinProvider.Status = StatusDisabled
		snap.BuiltinProvider.UnavailableReason = "chatgpt web provider routing is disabled"
	} else {
		var conflicts []string
		for _, model := range chatGPT.Models {
			id := strings.TrimSpace(model.ID)
			if id == "" {
				continue
			}
			entry := BuiltinModel{
				ID:        id,
				CreatedAt: model.CreatedAt,
				OwnedBy:   strings.TrimSpace(model.OwnedBy),
			}
			if _, exists := static[id]; exists {
				entry.ConflictWithStatic = true
				conflicts = append(conflicts, id)
				snap.BuiltinModels[id] = entry
				continue
			}
			snap.BuiltinModels[id] = entry
		}
		sort.Strings(conflicts)
		snap.BuiltinProvider.ConflictModels = conflicts
		snap.BuiltinProvider.ConflictCount = len(conflicts)
		snap.BuiltinProvider.ModelCount = len(snap.BuiltinModels)
		switch {
		case chatGPT.AvailableAccounts == 0:
			snap.BuiltinProvider.Status = StatusEmpty
			snap.BuiltinProvider.UnavailableReason = "no available chatgpt web accounts"
		case len(snap.BuiltinModels) == 0 && chatGPT.Version == 0:
			snap.BuiltinProvider.Status = StatusDiscovering
			snap.BuiltinProvider.UnavailableReason = "model discovery has not completed"
		case len(snap.BuiltinModels) == 0:
			snap.BuiltinProvider.Status = StatusEmpty
			snap.BuiltinProvider.UnavailableReason = "no discoverable models"
		default:
			snap.BuiltinProvider.Status = StatusReady
		}
	}
	buildCodexOAuth(&snap, cfg, static, codex)
	buildCandidates(&snap, cfg)
	return snap
}

func buildCodexOAuth(snap *Snapshot, cfg config.Config, static map[string]config.ModelMetadata, catalog CatalogInput) {
	if snap == nil {
		return
	}
	provider := &snap.CodexOAuthProvider
	if !config.EffectiveCodexOAuthProviderEnabled(cfg.CodexOAuth) {
		provider.Status = StatusDisabled
		provider.UnavailableReason = "Codex OAuth provider routing is disabled"
		return
	}
	conflicts := make([]string, 0)
	for _, model := range catalog.Models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		entry := BuiltinModel{ID: id, CreatedAt: model.CreatedAt, OwnedBy: strings.TrimSpace(model.OwnedBy)}
		if entry.OwnedBy == "" {
			entry.OwnedBy = "codex"
		}
		if _, exists := static[id]; exists {
			entry.ConflictWithStatic = true
			conflicts = append(conflicts, id)
		}
		if _, exists := snap.BuiltinModels[id]; exists {
			entry.ConflictWithStatic = true
			conflicts = append(conflicts, id)
		}
		snap.CodexOAuthModels[id] = entry
	}
	sort.Strings(conflicts)
	provider.ConflictModels = uniqueSorted(conflicts)
	provider.ConflictCount = len(provider.ConflictModels)
	provider.ModelCount = len(snap.CodexOAuthModels)
	switch {
	case catalog.AvailableAccounts == 0:
		provider.Status = StatusEmpty
		provider.UnavailableReason = "no available Codex OAuth accounts"
	case len(snap.CodexOAuthModels) == 0 && catalog.Version == 0:
		provider.Status = StatusDiscovering
		provider.UnavailableReason = "model discovery has not completed"
	case len(snap.CodexOAuthModels) == 0:
		provider.Status = StatusEmpty
		provider.UnavailableReason = "no discoverable Codex models"
	default:
		provider.Status = StatusReady
	}
}

func buildCandidates(snap *Snapshot, cfg config.Config) {
	if snap == nil {
		return
	}
	snap.Candidates = map[string][]Candidate{}
	modelIDs := map[string]struct{}{}
	for id := range snap.StaticModels {
		modelIDs[id] = struct{}{}
	}
	for id := range snap.BuiltinModels {
		modelIDs[id] = struct{}{}
	}
	for id := range snap.CodexOAuthModels {
		modelIDs[id] = struct{}{}
	}
	for id := range modelIDs {
		metadata := cfg.ModelMetadata[id]
		for name, provider := range cfg.Providers {
			if provider.Disabled || !config.ProviderMatchesModel(name, provider, id) {
				continue
			}
			snap.Candidates[id] = append(snap.Candidates[id], Candidate{
				ModelID: id, RouteOwner: name,
				Priority: config.EffectiveProviderPriority(provider), Fallback: config.EffectiveProviderFallback(provider),
				ContextWindowTokens: metadata.ContextWindowTokens, MaxOutputTokens: metadata.MaxOutputTokens,
				SupportedEndpoints: serviceablePathsForModel(provider, id, metadata),
				ConversionModes:    conversionModesForModel(provider, id, metadata),
			})
		}
	}
	if snap.CodexOAuthProvider.Status == StatusReady {
		for id, model := range snap.CodexOAuthModels {
			metadata := cfg.ModelMetadata[id]
			providerView := BuiltinProviderViewFor(CodexOAuthProviderID)
			snap.Candidates[id] = append(snap.Candidates[id], Candidate{
				ModelID: id, RouteOwner: CodexOAuthProviderID, Builtin: true,
				CreatedAt: model.CreatedAt, OwnedBy: model.OwnedBy, Priority: config.EffectiveCodexOAuthProviderPriority(cfg.CodexOAuth), Fallback: true,
				ContextWindowTokens: metadata.ContextWindowTokens, MaxOutputTokens: metadata.MaxOutputTokens,
				SupportedEndpoints: config.ServiceableInboundPaths(providerView),
			})
		}
	}
	if snap.BuiltinProvider.Status == StatusReady {
		for id, model := range snap.BuiltinModels {
			metadata := cfg.ModelMetadata[id]
			providerView := BuiltinProviderViewFor(BuiltinProviderID)
			snap.Candidates[id] = append(snap.Candidates[id], Candidate{
				ModelID: id, RouteOwner: BuiltinProviderID, Builtin: true,
				CreatedAt: model.CreatedAt, OwnedBy: model.OwnedBy, Priority: config.EffectiveChatGPTWebProviderPriority(cfg.ChatGPTWeb), Fallback: false,
				ContextWindowTokens: metadata.ContextWindowTokens, MaxOutputTokens: metadata.MaxOutputTokens,
				SupportedEndpoints: config.ServiceableInboundPaths(providerView),
			})
		}
	}
	for id, candidates := range snap.Candidates {
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].Priority != candidates[j].Priority {
				return candidates[i].Priority > candidates[j].Priority
			}
			return candidates[i].RouteOwner < candidates[j].RouteOwner
		})
		snap.Candidates[id] = candidates
	}
}

func serviceablePathsForModel(provider config.Provider, modelID string, metadata config.ModelMetadata) []string {
	paths := config.ServiceableInboundPaths(provider)
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		available := false
		for _, transport := range config.ResolveProviderTransports(provider, path) {
			if transport.Mode == "responses_to_anthropic" && !conversionCapabilityAvailable(provider, modelID, metadata, "responses_to_anthropic") {
				continue
			}
			if transport.Mode == "anthropic_to_responses" && !conversionCapabilityAvailable(provider, modelID, metadata, "anthropic_to_responses") {
				continue
			}
			available = true
			break
		}
		if available {
			result = append(result, path)
		}
	}
	return result
}

func conversionCapabilityAvailable(provider config.Provider, modelID string, metadata config.ModelMetadata, direction string) bool {
	capability, ok := metadata.ConversionCapabilities[direction]
	return ok && config.ConversionCapabilityUsable(direction, capability) && config.ProviderConversionReleased(provider, modelID, direction)
}

func conversionModesForModel(provider config.Provider, modelID string, metadata config.ModelMetadata) []string {
	seen := map[string]struct{}{}
	result := []string{}
	for _, path := range config.ServiceableInboundPaths(provider) {
		for _, transport := range config.ResolveProviderTransports(provider, path) {
			if !conversionCapabilityAvailable(provider, modelID, metadata, transport.Mode) {
				continue
			}
			if transport.Mode != config.ConversionDirectionResponsesToAnthropic && transport.Mode != config.ConversionDirectionAnthropicToResponses {
				continue
			}
			if _, exists := seen[transport.Mode]; exists {
				continue
			}
			seen[transport.Mode] = struct{}{}
			result = append(result, transport.Mode)
		}
	}
	sort.Strings(result)
	return result
}

func exactProviderModels(cfg config.Config) map[string]config.ModelMetadata {
	models := map[string]config.ModelMetadata{}
	for _, provider := range cfg.Providers {
		if provider.Disabled {
			continue
		}
		for _, pattern := range provider.Models {
			id := strings.TrimSpace(pattern)
			if id == "" || id == "*" || strings.HasSuffix(id, "*") {
				continue
			}
			metadata := cfg.ModelMetadata[id]
			metadata.ID = id
			models[id] = metadata
		}
	}
	return models
}

// Reconfigure rebuilds an existing auto-discovered catalog against a new
// static configuration. It keeps only constrained in-memory discovery data;
// the account pool remains the authority and an immediate refresh follows it.
func Reconfigure(cfg config.Config, previous Snapshot) Snapshot {
	poolModels := make([]PoolModel, 0, len(previous.BuiltinModels))
	for _, model := range previous.BuiltinModels {
		poolModels = append(poolModels, PoolModel{
			ID:        model.ID,
			CreatedAt: model.CreatedAt,
			OwnedBy:   model.OwnedBy,
		})
	}
	codexModels := make([]PoolModel, 0, len(previous.CodexOAuthModels))
	for _, model := range previous.CodexOAuthModels {
		codexModels = append(codexModels, PoolModel{ID: model.ID, CreatedAt: model.CreatedAt, OwnedBy: model.OwnedBy})
	}
	return BuildWithCodex(cfg,
		CatalogInput{Version: previous.Version, AvailableAccounts: previous.BuiltinProvider.AvailableAccounts, Models: poolModels, UpdatedAt: previous.BuiltinProvider.UpdatedAt},
		CatalogInput{Version: previous.CodexOAuthVersion, AvailableAccounts: previous.CodexOAuthProvider.AvailableAccounts, Models: codexModels, UpdatedAt: previous.CodexOAuthProvider.UpdatedAt},
	)
}

// PoolModel is the discovery input DTO (account-pool catalog projection).
type PoolModel struct {
	ID        string
	CreatedAt int64
	OwnedBy   string
}

// CandidatesFor resolves all ordered candidates for an exact model ID.
func (s Snapshot) CandidatesFor(modelID string) []Candidate {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	items := s.Candidates[modelID]
	result := make([]Candidate, 0, len(items))
	for _, item := range items {
		result = append(result, item)
	}
	return result
}

// Lookup resolves the first ordered candidate for a model.
func (s Snapshot) Lookup(modelID string) (Route, bool) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return Route{}, false
	}
	candidates := s.CandidatesFor(modelID)
	if len(candidates) == 0 {
		return Route{}, false
	}
	primary := candidates[0]
	return Route{
		ModelID: primary.ModelID, RouteOwner: primary.RouteOwner, Builtin: primary.Builtin,
		CreatedAt: primary.CreatedAt, OwnedBy: primary.OwnedBy, ContextWindowTokens: primary.ContextWindowTokens, MaxOutputTokens: primary.MaxOutputTokens,
	}, true
}

// SortedModelIDs returns each model with at least one candidate in stable order.
func (s Snapshot) SortedModelIDs() []string {
	ids := make([]string, 0, len(s.Candidates))
	for id, candidates := range s.Candidates {
		if len(candidates) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// BuiltinProviderView returns a synthetic Provider used by the transport matrix.
func BuiltinProviderView() config.Provider {
	return BuiltinProviderViewFor(BuiltinProviderID)
}

// BuiltinProviderViewFor returns the synthetic provider needed by the
// transport matrix for a concrete builtin route owner.
func BuiltinProviderViewFor(owner string) config.Provider {
	if owner == CodexOAuthProviderID {
		return config.Provider{Name: CodexOAuthProviderID, Protocol: CodexOAuthProviderID, Endpoints: []string{config.ProviderEndpointResponses}, Priority: CodexOAuthPriority, Fallback: true}
	}
	return config.Provider{
		Name:      BuiltinProviderID,
		Protocol:  BuiltinProviderID,
		Endpoints: []string{config.ProviderEndpointChatCompletions, config.ProviderEndpointResponses, config.ProviderEndpointImages},
		Priority:  ChatGPTWebPriority,
		Disabled:  false,
	}
}

// Empty returns a disabled empty snapshot.
func Empty() Snapshot {
	return Snapshot{
		StaticModels:     map[string]config.ModelMetadata{},
		BuiltinModels:    map[string]BuiltinModel{},
		CodexOAuthModels: map[string]BuiltinModel{},
		Candidates:       map[string][]Candidate{},
		BuiltinProvider: BuiltinProvider{
			ID:     BuiltinProviderID,
			Status: StatusDisabled,
		},
		CodexOAuthProvider: BuiltinProvider{ID: CodexOAuthProviderID, Status: StatusDisabled},
	}
}

// FromStatic builds a snapshot containing only the static catalog (chatgpt web off).
func FromStatic(cfg config.Config) Snapshot {
	return Build(cfg, 0, 0, nil, time.Time{}.Format(time.RFC3339))
}

func uniqueSorted(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
