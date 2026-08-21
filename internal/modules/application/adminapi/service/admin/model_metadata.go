package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"aetherrelay/internal/pkg/aetherrelayconfig"
	"go.yaml.in/yaml/v4"
)

type modelMetadataView struct {
	ID                     string                                 `json:"id"`
	ContextWindowTokens    int                                    `json:"context_window_tokens,omitempty"`
	MaxContextWindowTokens int                                    `json:"max_context_window_tokens,omitempty"`
	MaxOutputTokens        int                                    `json:"max_output_tokens,omitempty"`
	ReasoningDeclared      bool                                   `json:"reasoning_declared"`
	ReasoningSupported     bool                                   `json:"reasoning_supported"`
	ReasoningDefaultEffort string                                 `json:"reasoning_default_effort,omitempty"`
	ReasoningEfforts       []string                               `json:"reasoning_efforts,omitempty"`
	NativeResponsesTools   bool                                   `json:"native_responses_tools"`
	NativeResponsesImages  bool                                   `json:"native_responses_images"`
	ConversionCapabilities map[string]config.ConversionCapability `json:"conversion_capabilities,omitempty"`
}

type modelMetadataPatch struct {
	ContextWindowTokens    *int      `json:"context_window_tokens"`
	MaxContextWindowTokens *int      `json:"max_context_window_tokens"`
	MaxOutputTokens        *int      `json:"max_output_tokens"`
	ReasoningSupported     *bool     `json:"reasoning_supported"`
	ReasoningDefaultEffort *string   `json:"reasoning_default_effort"`
	ReasoningEfforts       *[]string `json:"reasoning_efforts"`
	NativeResponsesTools   *bool     `json:"native_responses_tools"`
	NativeResponsesImages  *bool     `json:"native_responses_images"`
}

func (h *Handler) listModelMetadata(w http.ResponseWriter) {
	cfg := h.runtime.ConfigSnapshot()
	ids := make([]string, 0, len(cfg.ModelMetadata))
	for id := range cfg.ModelMetadata {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]modelMetadataView, 0, len(ids))
	for _, id := range ids {
		m := cfg.ModelMetadata[id]
		items = append(items, modelMetadataView{ID: id, ContextWindowTokens: m.ContextWindowTokens, MaxContextWindowTokens: m.MaxContextWindowTokens, MaxOutputTokens: m.MaxOutputTokens, ReasoningDeclared: m.ReasoningDeclared, ReasoningSupported: m.ReasoningSupported, ReasoningDefaultEffort: m.ReasoningDefaultEffort, ReasoningEfforts: append([]string(nil), m.ReasoningEfforts...), NativeResponsesTools: m.NativeResponsesTools, NativeResponsesImages: m.NativeResponsesImages, ConversionCapabilities: m.ConversionCapabilities})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "writable": strings.TrimSpace(h.configPath) != ""})
}

func (h *Handler) patchModelMetadata(w http.ResponseWriter, r *http.Request, rel string) {
	id, err := url.PathUnescape(strings.TrimPrefix(rel, "/api/model-metadata/"))
	if err != nil || strings.TrimSpace(id) == "" {
		writeError(w, http.StatusBadRequest, "model id is required")
		return
	}
	var input modelMetadataPatch
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if input.ContextWindowTokens != nil && *input.ContextWindowTokens < 0 || input.MaxContextWindowTokens != nil && *input.MaxContextWindowTokens < 0 || input.MaxOutputTokens != nil && *input.MaxOutputTokens < 0 {
		writeError(w, http.StatusBadRequest, "token limits must be non-negative")
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	current := h.runtime.ConfigSnapshot()
	metadata, ok := current.ModelMetadata[id]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("model metadata %q not found", id))
		return
	}
	contextWindow, maxContextWindow := metadata.ContextWindowTokens, metadata.MaxContextWindowTokens
	if input.ContextWindowTokens != nil {
		contextWindow = *input.ContextWindowTokens
	}
	if input.MaxContextWindowTokens != nil {
		maxContextWindow = *input.MaxContextWindowTokens
	}
	if contextWindow > 0 && maxContextWindow > 0 && maxContextWindow < contextWindow {
		writeError(w, http.StatusBadRequest, "max_context_window_tokens must be greater than or equal to context_window_tokens")
		return
	}
	base := current.AdminAuth.BasePath
	rewrite, err := prepareConfigRewrite(h.configPath, base, func(root *yaml.Node) error { return mutateModelMetadataYAML(root, id, input) })
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err := h.activateAndCommitConfig(rewrite); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "message": "model metadata updated and activated"})
}

func mutateModelMetadataYAML(root *yaml.Node, id string, input modelMetadataPatch) error {
	var mapping *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "model_metadata" {
			mapping = root.Content[i+1]
			break
		}
	}
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return fmt.Errorf("model_metadata section is unavailable")
	}
	var entry *yaml.Node
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == id {
			entry = mapping.Content[i+1]
			break
		}
	}
	if entry == nil || entry.Kind != yaml.MappingNode {
		return fmt.Errorf("model metadata %q is unavailable", id)
	}
	set := func(key, val string) {
		for i := 0; i+1 < len(entry.Content); i += 2 {
			if entry.Content[i].Value == key {
				entry.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val}
				return
			}
		}
		entry.Content = append(entry.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val})
	}
	if input.ContextWindowTokens != nil {
		set("context_window_tokens", fmt.Sprint(*input.ContextWindowTokens))
	}
	if input.MaxContextWindowTokens != nil {
		set("max_context_window_tokens", fmt.Sprint(*input.MaxContextWindowTokens))
	}
	if input.MaxOutputTokens != nil {
		set("max_output_tokens", fmt.Sprint(*input.MaxOutputTokens))
	}
	if input.ReasoningSupported != nil {
		set("reasoning_supported", fmt.Sprint(*input.ReasoningSupported))
	}
	if input.ReasoningDefaultEffort != nil {
		set("reasoning_default_effort", strings.ToLower(strings.TrimSpace(*input.ReasoningDefaultEffort)))
	}
	if input.ReasoningEfforts != nil {
		vals := make([]*yaml.Node, 0, len(*input.ReasoningEfforts))
		for _, v := range *input.ReasoningEfforts {
			vals = append(vals, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: strings.ToLower(strings.TrimSpace(v))})
		}
		set("reasoning_efforts", "["+strings.Join(*input.ReasoningEfforts, ", ")+"]")
		_ = vals
	}
	if input.NativeResponsesTools != nil {
		set("native_responses_tools", fmt.Sprint(*input.NativeResponsesTools))
	}
	if input.NativeResponsesImages != nil {
		set("native_responses_images", fmt.Sprint(*input.NativeResponsesImages))
	}
	return nil
}
