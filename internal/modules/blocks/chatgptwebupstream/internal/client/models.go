package client

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ModelOperation mirrors the upstream owner's restricted operation enum.
// Kept as a plain string here so the client package does not import events.
type ModelOperation string

const (
	ModelOperationChatCompletions  ModelOperation = "chat_completions"
	ModelOperationImageGenerations ModelOperation = "image_generations"
)

// ModelDescriptor is the constrained projection of one upstream model entry.
// Models without any verified operation are never returned.
type ModelDescriptor struct {
	ID         string
	Operations []ModelOperation
	CreatedAt  int64
	OwnedBy    string
}

// ListModels enumerates models available to the authenticated ChatGPT Web
// account via /backend-api/models. Only verified model IDs and operations
// enter the result; unknown upstream fields are ignored.
func (c *Client) ListModels() ([]ModelDescriptor, error) {
	if err := c.Bootstrap(); err != nil {
		return nil, err
	}
	body, err := c.get("/backend-api/models?history_and_training_disabled=false", "list_models")
	if err != nil {
		return nil, err
	}
	return ParseModelsResponse(body)
}

// ParseModelsResponse projects a raw /backend-api/models JSON payload into
// constrained ModelDescriptor values. It is exported for fixture tests.
func ParseModelsResponse(body []byte) ([]ModelDescriptor, error) {
	var payload struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	out := make([]ModelDescriptor, 0, len(payload.Models))
	seen := map[string]struct{}{}
	for _, raw := range payload.Models {
		desc, ok := projectModel(raw)
		if !ok {
			continue
		}
		if _, exists := seen[desc.ID]; exists {
			continue
		}
		seen[desc.ID] = struct{}{}
		out = append(out, desc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func projectModel(raw json.RawMessage) (ModelDescriptor, bool) {
	var item struct {
		Slug            string          `json:"slug"`
		ID              string          `json:"id"`
		Created         int64           `json:"created"`
		OwnedBy         string          `json:"owned_by"`
		Tags            []string        `json:"tags"`
		ProductFeatures json.RawMessage `json:"product_features"`
		EnabledTools    []string        `json:"enabled_tools"`
		Capabilities    json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return ModelDescriptor{}, false
	}
	// Design: slug is the sole model ID authority. The upstream `id` field is
	// deliberately ignored because it is not the ChatGPT Web routing model ID.
	id := strings.TrimSpace(item.Slug)
	if id == "" {
		return ModelDescriptor{}, false
	}
	if isResearchOnlyModel(id, item.Tags, item.EnabledTools) {
		return ModelDescriptor{}, false
	}
	ops := detectOperations(item.Tags, item.EnabledTools, item.ProductFeatures, item.Capabilities)
	if len(ops) == 0 {
		return ModelDescriptor{}, false
	}
	ownedBy := strings.TrimSpace(item.OwnedBy)
	if ownedBy == "" {
		ownedBy = "chatgpt"
	}
	return ModelDescriptor{
		ID:         id,
		Operations: ops,
		CreatedAt:  item.Created,
		OwnedBy:    ownedBy,
	}, true
}

// isResearchOnlyModel rejects research / deep-research / search-tool dedicated
// entries so they never enter the temporary-chat picker or /v1/models as ordinary
// chat_completions models.
func isResearchOnlyModel(id string, tags, enabledTools []string) bool {
	lowerID := strings.ToLower(strings.TrimSpace(id))
	if strings.Contains(lowerID, "deep_research") || strings.Contains(lowerID, "deep-research") ||
		strings.Contains(lowerID, "research") && !strings.Contains(lowerID, "o1") && !strings.Contains(lowerID, "o3") && !strings.Contains(lowerID, "o4") {
		// Explicit research product IDs only; avoid dropping general models whose
		// marketing names merely mention "research".
		if strings.Contains(lowerID, "deep_research") || strings.Contains(lowerID, "deep-research") ||
			strings.HasPrefix(lowerID, "research") || strings.Contains(lowerID, "-research") || strings.Contains(lowerID, "_research") {
			return true
		}
	}
	if hasAnySignal(tags, "deep_research", "deep-research", "research_only", "research_mode", "search_tool_only") {
		return true
	}
	if hasAnySignal(enabledTools, "deep_research", "deep-research", "research", "web_search_research") {
		// Tool presence alone is not enough for ordinary models; require research tag/id.
		if hasAnySignal(tags, "research", "deep_research", "deep-research") ||
			strings.Contains(lowerID, "research") {
			return true
		}
	}
	return false
}

// detectOperations projects only operations backed by verified signals.
//
// /backend-api/models is ChatGPT Web's conversation model picker: a listed
// entry with a slug is itself the verified signal for chat_completions, unless
// the entry is explicitly image-only. image_generations requires an explicit
// image tool / product-feature signal and is never inferred from the slug.
func detectOperations(tags, enabledTools []string, productFeatures, capabilities json.RawMessage) []ModelOperation {
	var ops []ModelOperation
	add := func(op ModelOperation) {
		for _, existing := range ops {
			if existing == op {
				return
			}
		}
		ops = append(ops, op)
	}

	imageOnly := hasAnySignal(tags, "image_generation_only", "image_gen_only", "image_only")
	if hasImageGenerationSignal(tags, enabledTools, productFeatures, capabilities) {
		add(ModelOperationImageGenerations)
	}
	if !imageOnly {
		add(ModelOperationChatCompletions)
	}
	return ops
}

func hasImageGenerationSignal(tags, enabledTools []string, productFeatures, capabilities json.RawMessage) bool {
	if hasAnySignal(tags, "image_generation", "image_gen", "dalle", "gpt_image") {
		return true
	}
	if hasAnySignal(enabledTools, "image_generation", "image_gen", "dalle", "t2i") {
		return true
	}
	// Require image-specific product feature keys; generic can_use_tools is not enough.
	if rawContainsAny(productFeatures, "image_gen", "image_generation", "dalle") {
		return true
	}
	if rawContainsAny(capabilities, "image_generation", "image_gen", "dalle") {
		return true
	}
	return false
}

func hasAnySignal(values []string, needles ...string) bool {
	for _, value := range values {
		lower := strings.ToLower(strings.TrimSpace(value))
		if lower == "" {
			continue
		}
		for _, needle := range needles {
			if lower == needle || strings.Contains(lower, needle) {
				return true
			}
		}
	}
	return false
}

func rawContainsAny(raw json.RawMessage, needles ...string) bool {
	if len(raw) == 0 {
		return false
	}
	lower := strings.ToLower(string(raw))
	for _, needle := range needles {
		if strings.Contains(lower, strings.ToLower(needle)) {
			return true
		}
	}
	return false
}
