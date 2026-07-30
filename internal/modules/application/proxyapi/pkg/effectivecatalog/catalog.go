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

const BuiltinProviderID = "chatgptweb"

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
	// ConflictWithStatic is true when a static provider already owns this model ID.
	// Such models are excluded from the effective catalog but retained for Admin.
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
	// Version is the account-pool catalog generation used to build this snapshot.
	Version uint64
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
// Static exact model IDs always win over builtin models with the same ID.
func Build(cfg config.Config, poolVersion uint64, availableAccounts int, poolModels []PoolModel, updatedAt string) Snapshot {
	static := map[string]config.ModelInfo{}
	for id, info := range cfg.ModelCatalog {
		static[id] = info
	}
	snap := Snapshot{
		StaticModels:  static,
		Version:       poolVersion,
		BuiltinModels: map[string]BuiltinModel{},
		BuiltinProvider: BuiltinProvider{
			ID:                BuiltinProviderID,
			Enabled:           cfg.ChatGPTWeb.Enabled,
			AvailableAccounts: availableAccounts,
			UpdatedAt:         strings.TrimSpace(updatedAt),
		},
	}
	if !cfg.ChatGPTWeb.Enabled {
		snap.BuiltinProvider.Status = StatusDisabled
		snap.BuiltinProvider.UnavailableReason = "chatgpt_web is not enabled"
		return snap
	}

	var conflicts []string
	for _, model := range poolModels {
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
			// Retain the constrained entry in the internal snapshot so a config
			// hot reload can rebuild conflicts without waiting for discovery. Lookup
			// and SortedModelIDs still exclude it from the effective directory.
			snap.BuiltinModels[id] = entry
			continue
		}
		snap.BuiltinModels[id] = entry
	}
	sort.Strings(conflicts)
	snap.BuiltinProvider.ConflictModels = conflicts
	snap.BuiltinProvider.ConflictCount = len(conflicts)
	for _, model := range snap.BuiltinModels {
		if !model.ConflictWithStatic {
			snap.BuiltinProvider.ModelCount++
		}
	}

	switch {
	case availableAccounts == 0:
		snap.BuiltinProvider.Status = StatusEmpty
		snap.BuiltinProvider.UnavailableReason = "no available chatgpt web accounts"
	case len(snap.BuiltinModels) == 0 && poolVersion == 0:
		snap.BuiltinProvider.Status = StatusDiscovering
		snap.BuiltinProvider.UnavailableReason = "model discovery has not completed"
	case len(snap.BuiltinModels) == 0:
		snap.BuiltinProvider.Status = StatusEmpty
		snap.BuiltinProvider.UnavailableReason = "no discoverable models"
	case len(conflicts) > 0:
		snap.BuiltinProvider.Status = StatusDegraded
	default:
		snap.BuiltinProvider.Status = StatusReady
	}
	return snap
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
	return Build(cfg, previous.Version, previous.BuiltinProvider.AvailableAccounts, poolModels, previous.BuiltinProvider.UpdatedAt)
}

// PoolModel is the discovery input DTO (account-pool catalog projection).
type PoolModel struct {
	ID         string
	Operations []string
	CreatedAt  int64
	OwnedBy    string
}

// Lookup resolves an exact model ID from the effective catalog.
func (s Snapshot) Lookup(modelID string) (Route, bool) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return Route{}, false
	}
	if info, ok := s.StaticModels[modelID]; ok {
		return Route{
			ModelID:             info.ID,
			RouteOwner:          info.RouteOwner,
			Operations:          append([]string(nil), info.Operations...),
			ContextWindowTokens: info.ContextWindowTokens,
			MaxOutputTokens:     info.MaxOutputTokens,
		}, true
	}
	if model, ok := s.BuiltinModels[modelID]; ok && !model.ConflictWithStatic {
		return Route{
			ModelID:    model.ID,
			RouteOwner: BuiltinProviderID,
			Operations: append([]string(nil), model.Operations...),
			Builtin:    true,
			CreatedAt:  model.CreatedAt,
			OwnedBy:    model.OwnedBy,
		}, true
	}
	return Route{}, false
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

// SortedModelIDs returns static + builtin model IDs in stable order for /v1/models.
func (s Snapshot) SortedModelIDs() []string {
	ids := make([]string, 0, len(s.StaticModels)+len(s.BuiltinModels))
	for id := range s.StaticModels {
		ids = append(ids, id)
	}
	for id, model := range s.BuiltinModels {
		if model.ConflictWithStatic {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// BuiltinProviderView returns a synthetic Provider used by the transport matrix.
func BuiltinProviderView() config.Provider {
	return config.Provider{
		Name:                 BuiltinProviderID,
		Protocol:             BuiltinProviderID,
		EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions, config.EndpointCapabilityResponses, config.EndpointCapabilityImages},
		Disabled:             false,
	}
}

// Empty returns a disabled empty snapshot.
func Empty() Snapshot {
	return Snapshot{
		StaticModels:  map[string]config.ModelInfo{},
		BuiltinModels: map[string]BuiltinModel{},
		BuiltinProvider: BuiltinProvider{
			ID:     BuiltinProviderID,
			Status: StatusDisabled,
		},
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
