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
	ID         string
	Operations []string
	CreatedAt  int64
	OwnedBy    string
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
	StaticModels    map[string]config.ModelInfo
	BuiltinProvider BuiltinProvider
	BuiltinModels   map[string]BuiltinModel
	// Codex OAuth models are discovered and cached by the independent Codex
	// account-pool owner; the account-level snapshot is their only authority.
	CodexOAuthProvider BuiltinProvider
	CodexOAuthModels   map[string]BuiltinModel
	// Candidates is the immutable model + operation routing authority. It
	// contains all compatible static and builtin sources in deterministic order.
	Candidates map[string][]Candidate
	// Version is the account-pool catalog generation used to build this snapshot.
	Version           uint64
	CodexOAuthVersion uint64
}

// Candidate is one eligible upstream source for a model. A candidate may serve
// only a subset of a model's operations when providers expose different
// endpoint capabilities.
type Candidate struct {
	ModelID             string
	RouteOwner          string
	Operations          []string
	Builtin             bool
	CreatedAt           int64
	OwnedBy             string
	Priority            int
	Fallback            bool
	ContextWindowTokens int
	MaxOutputTokens     int
}

func (c Candidate) SupportsOperation(operation string) bool {
	operation = strings.TrimSpace(operation)
	for _, item := range c.Operations {
		if item == operation {
			return true
		}
	}
	return false
}

// Route is the request-time resolved model route from either static or builtin.
type Route struct {
	ModelID    string
	RouteOwner string
	Operations []string
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
	static := map[string]config.ModelInfo{}
	for id, info := range cfg.ModelCatalog {
		static[id] = info
	}
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
		if !cfg.ChatGPTWeb.Enabled {
			snap.BuiltinProvider.UnavailableReason = "chatgpt web account-pool runtime is disabled"
		} else {
			snap.BuiltinProvider.UnavailableReason = "chatgpt web provider routing is disabled"
		}
	} else {
		var conflicts []string
		for _, model := range chatGPT.Models {
			id := strings.TrimSpace(model.ID)
			if id == "" || len(model.Operations) == 0 {
				continue
			}
			entry := BuiltinModel{
				ID:         id,
				Operations: uniqueSorted(model.Operations),
				CreatedAt:  model.CreatedAt,
				OwnedBy:    strings.TrimSpace(model.OwnedBy),
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

func buildCodexOAuth(snap *Snapshot, cfg config.Config, static map[string]config.ModelInfo, catalog CatalogInput) {
	if snap == nil {
		return
	}
	provider := &snap.CodexOAuthProvider
	if !config.EffectiveCodexOAuthProviderEnabled(cfg.CodexOAuth) {
		provider.Status = StatusDisabled
		if !cfg.CodexOAuth.Enabled {
			provider.UnavailableReason = "Codex OAuth account-pool runtime is disabled"
		} else {
			provider.UnavailableReason = "Codex OAuth provider routing is disabled"
		}
		return
	}
	conflicts := make([]string, 0)
	for _, model := range catalog.Models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		entry := BuiltinModel{ID: id, Operations: []string{config.ModelOperationChatCompletions}, CreatedAt: model.CreatedAt, OwnedBy: strings.TrimSpace(model.OwnedBy)}
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
	for id, info := range snap.StaticModels {
		names := append([]string(nil), info.RouteOwners...)
		if len(names) == 0 && strings.TrimSpace(info.RouteOwner) != "" {
			names = []string{info.RouteOwner}
		}
		for _, name := range names {
			provider, ok := cfg.Providers[name]
			if !ok {
				// Focused catalog tests and read-only projections may carry an
				// already-resolved static owner without its full provider object.
				// Preserve that authority; request execution still fail-fast checks
				// the provider before creating an upstream request.
				snap.Candidates[id] = append(snap.Candidates[id], Candidate{
					ModelID: id, RouteOwner: name, Operations: append([]string(nil), info.Operations...),
					Priority: config.DefaultProviderPriority, Fallback: true,
					ContextWindowTokens: info.ContextWindowTokens, MaxOutputTokens: info.MaxOutputTokens,
				})
				continue
			}
			if provider.Disabled {
				continue
			}
			operations := supportedStaticOperations(info.Operations, provider)
			if len(operations) == 0 {
				continue
			}
			snap.Candidates[id] = append(snap.Candidates[id], Candidate{
				ModelID:             id,
				RouteOwner:          name,
				Operations:          operations,
				Priority:            config.EffectiveProviderPriority(provider),
				Fallback:            config.EffectiveProviderFallback(provider),
				ContextWindowTokens: info.ContextWindowTokens,
				MaxOutputTokens:     info.MaxOutputTokens,
			})
		}
	}
	if snap.CodexOAuthProvider.Status == StatusReady {
		for id, model := range snap.CodexOAuthModels {
			snap.Candidates[id] = append(snap.Candidates[id], Candidate{
				ModelID: id, RouteOwner: CodexOAuthProviderID, Operations: append([]string(nil), model.Operations...), Builtin: true,
				CreatedAt: model.CreatedAt, OwnedBy: model.OwnedBy, Priority: config.EffectiveCodexOAuthProviderPriority(cfg.CodexOAuth), Fallback: true,
			})
		}
	}
	if snap.BuiltinProvider.Status == StatusReady {
		for id, model := range snap.BuiltinModels {
			snap.Candidates[id] = append(snap.Candidates[id], Candidate{
				ModelID: id, RouteOwner: BuiltinProviderID, Operations: append([]string(nil), model.Operations...), Builtin: true,
				CreatedAt: model.CreatedAt, OwnedBy: model.OwnedBy, Priority: config.EffectiveChatGPTWebProviderPriority(cfg.ChatGPTWeb), Fallback: false,
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

func supportedStaticOperations(operations []string, provider config.Provider) []string {
	result := make([]string, 0, len(operations))
	for _, operation := range operations {
		path := config.OperationToPrimaryInboundPath(operation)
		if path != "" && config.ProviderSupportsInboundPath(provider, path) {
			result = append(result, operation)
		}
	}
	return result
}

// Reconfigure rebuilds an existing auto-discovered catalog against a new
// static configuration. It keeps only constrained in-memory discovery data;
// the account pool remains the authority and an immediate refresh follows it.
func Reconfigure(cfg config.Config, previous Snapshot) Snapshot {
	poolModels := make([]PoolModel, 0, len(previous.BuiltinModels))
	for _, model := range previous.BuiltinModels {
		poolModels = append(poolModels, PoolModel{
			ID:         model.ID,
			Operations: append([]string(nil), model.Operations...),
			CreatedAt:  model.CreatedAt,
			OwnedBy:    model.OwnedBy,
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
	ID         string
	Operations []string
	CreatedAt  int64
	OwnedBy    string
}

// CandidatesFor resolves all ordered candidates that can serve an operation.
func (s Snapshot) CandidatesFor(modelID, operation string) []Candidate {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	items := s.Candidates[modelID]
	result := make([]Candidate, 0, len(items))
	for _, item := range items {
		if operation != "" && !item.SupportsOperation(operation) {
			continue
		}
		item.Operations = append([]string(nil), item.Operations...)
		result = append(result, item)
	}
	return result
}

// Lookup resolves the first ordered candidate and the union of all model
// operations. New request routing should call CandidatesFor with its operation.
func (s Snapshot) Lookup(modelID string) (Route, bool) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return Route{}, false
	}
	candidates := s.CandidatesFor(modelID, "")
	if len(candidates) == 0 {
		return Route{}, false
	}
	primary := candidates[0]
	operations := map[string]struct{}{}
	for _, candidate := range candidates {
		for _, operation := range candidate.Operations {
			operations[operation] = struct{}{}
		}
	}
	return Route{
		ModelID: primary.ModelID, RouteOwner: primary.RouteOwner, Operations: sortedOperations(operations), Builtin: primary.Builtin,
		CreatedAt: primary.CreatedAt, OwnedBy: primary.OwnedBy, ContextWindowTokens: primary.ContextWindowTokens, MaxOutputTokens: primary.MaxOutputTokens,
	}, true
}

// SupportsOperation reports whether the resolved route allows operation.
func (r Route) SupportsOperation(operation string) bool {
	operation = strings.TrimSpace(operation)
	for _, op := range r.Operations {
		if op == operation {
			return true
		}
	}
	return false
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

func sortedOperations(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// BuiltinProviderView returns a synthetic Provider used by the transport matrix.
func BuiltinProviderView() config.Provider {
	return BuiltinProviderViewFor(BuiltinProviderID)
}

// BuiltinProviderViewFor returns the synthetic provider needed by the
// transport matrix for a concrete builtin route owner.
func BuiltinProviderViewFor(owner string) config.Provider {
	if owner == CodexOAuthProviderID {
		return config.Provider{Name: CodexOAuthProviderID, Protocol: CodexOAuthProviderID, EndpointCapabilities: []string{config.EndpointCapabilityResponses}, Priority: CodexOAuthPriority, Fallback: true}
	}
	return config.Provider{
		Name:                 BuiltinProviderID,
		Protocol:             BuiltinProviderID,
		EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions, config.EndpointCapabilityResponses, config.EndpointCapabilityImages},
		Priority:             ChatGPTWebPriority,
		Disabled:             false,
	}
}

// Empty returns a disabled empty snapshot.
func Empty() Snapshot {
	return Snapshot{
		StaticModels:     map[string]config.ModelInfo{},
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
