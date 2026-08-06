package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxyusage"

	"go.yaml.in/yaml/v4"
)

type clientKeyView struct {
	ID         string `json:"id"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  string `json:"created_at,omitempty"`
	LastUsedAt string `json:"last_used_at,omitempty"`
}

type createClientKeyRequest struct {
	ID      string `json:"id"`
	Enabled *bool  `json:"enabled"`
}

type updateClientKeyRequest struct {
	Enabled *bool `json:"enabled"`
}

func (h *Handler) listClientAPIKeys(w http.ResponseWriter) {
	cfg := h.runtime.ConfigSnapshot()
	metadata := map[string]usage.ClientAPIKeyMetadata{}
	if h.usageStore != nil {
		for id := range cfg.ClientAPIKeys {
			_ = h.usageStore.EnsureClientAPIKey(context.Background(), id, time.Now().UTC())
		}
		if loaded, err := h.usageStore.ClientAPIKeyMetadata(context.Background()); err == nil {
			metadata = loaded
		}
	}
	keys := clientKeyViews(cfg, metadata)
	writeJSON(w, http.StatusOK, map[string]any{"client_api_keys": keys, "writable": strings.TrimSpace(h.configPath) != "", "hot_reload": true})
}

func clientKeyViews(cfg config.Config, metadata map[string]usage.ClientAPIKeyMetadata) []clientKeyView {
	ids := make([]string, 0, len(cfg.ClientAPIKeys))
	for id := range cfg.ClientAPIKeys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]clientKeyView, 0, len(ids))
	for _, id := range ids {
		key := cfg.ClientAPIKeys[id]
		view := clientKeyView{ID: id, Enabled: key.Enabled}
		if meta, ok := metadata[id]; ok {
			view.CreatedAt = meta.CreatedAt.UTC().Format(time.RFC3339)
			if meta.LastUsedAt != nil {
				view.LastUsedAt = meta.LastUsedAt.UTC().Format(time.RFC3339)
			}
		}
		result = append(result, view)
	}
	return result
}

func (h *Handler) createClientAPIKey(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminMutation(w, r) {
		return
	}
	var input createClientKeyRequest
	if !decodeAdminJSON(w, r, &input) {
		return
	}
	id := strings.ToLower(strings.TrimSpace(input.ID))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	secret, hash, err := generateClientKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate client API key")
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	if _, ok := h.runtime.ConfigSnapshot().ClientAPIKeys[id]; ok {
		writeError(w, http.StatusConflict, "client API key id already exists")
		return
	}
	rewrite, err := prepareClientKeysRewrite(h.configPath, h.adminBasePath(), func(node *yaml.Node) error {
		if mappingValue(node, id) != nil {
			return errors.New("client API key id already exists")
		}
		entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendScalar(entry, "api_key_hash", hash, "!!str")
		appendScalar(entry, "enabled", fmt.Sprintf("%t", enabled), "!!bool")
		node.Content = append(node.Content, mappingKey(id), entry)
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.activateAndCommitConfig(rewrite); err != nil {
		writeError(w, http.StatusInternalServerError, "activate config: "+err.Error())
		return
	}
	if h.usageStore != nil {
		_ = h.usageStore.EnsureClientAPIKey(context.Background(), id, time.Now().UTC())
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "enabled": enabled, "api_key": secret, "message": "Copy this API key now. It cannot be displayed again."})
}

func (h *Handler) clientAPIKeyAction(w http.ResponseWriter, r *http.Request, rel string) {
	if !h.requireAdminMutation(w, r) {
		return
	}
	rest := strings.TrimPrefix(rel, "/api/client-api-keys/")
	rotate := strings.HasSuffix(rest, "/rotate")
	id := strings.TrimSuffix(rest, "/rotate")
	id = strings.ToLower(strings.TrimSpace(strings.Trim(id, "/")))
	if id == "" || id == config.ReservedClientAPIKeyID {
		writeError(w, http.StatusBadRequest, "invalid client API key id")
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	if _, ok := h.runtime.ConfigSnapshot().ClientAPIKeys[id]; !ok {
		writeError(w, http.StatusNotFound, "client API key not found")
		return
	}
	switch {
	case rotate && r.Method == http.MethodPost:
		secret, hash, err := generateClientKey()
		if err != nil {
			writeError(w, 500, "generate client API key")
			return
		}
		rewrite, err := prepareClientKeysRewrite(h.configPath, h.adminBasePath(), func(node *yaml.Node) error {
			entry := mappingValue(node, id)
			if entry == nil {
				return errors.New("client API key not found")
			}
			removeMappingValue(entry, "api_key")
			setMappingValue(entry, "api_key_hash", scalar(hash, "!!str"))
			return nil
		})
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := h.activateAndCommitConfig(rewrite); err != nil {
			writeError(w, 500, "activate config: "+err.Error())
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]any{"id": id, "api_key": secret, "message": "Copy this API key now. It cannot be displayed again."})
	case !rotate && r.Method == http.MethodPatch:
		var input updateClientKeyRequest
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		if input.Enabled == nil {
			writeError(w, 400, "enabled is required")
			return
		}
		rewrite, err := prepareClientKeysRewrite(h.configPath, h.adminBasePath(), func(node *yaml.Node) error {
			entry := mappingValue(node, id)
			if entry == nil {
				return errors.New("client API key not found")
			}
			setMappingValue(entry, "enabled", scalar(fmt.Sprintf("%t", *input.Enabled), "!!bool"))
			return nil
		})
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := h.activateAndCommitConfig(rewrite); err != nil {
			writeError(w, 500, "activate config: "+err.Error())
			return
		}
		writeJSON(w, 200, clientKeyView{ID: id, Enabled: *input.Enabled})
	case !rotate && r.Method == http.MethodDelete:
		rewrite, err := prepareClientKeysRewrite(h.configPath, h.adminBasePath(), func(node *yaml.Node) error { removeMappingValue(node, id); return nil })
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := h.activateAndCommitConfig(rewrite); err != nil {
			writeError(w, 500, "activate config: "+err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		writeError(w, 400, "invalid request: "+err.Error())
		return false
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, 400, "invalid request: multiple JSON values")
		return false
	}
	return true
}

func generateClientKey() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	key := "sk_" + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(key))
	return key, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func prepareClientKeysRewrite(path, expectedAdminBasePath string, mutate func(*yaml.Node) error) (*configRewrite, error) {
	return prepareConfigRewrite(path, expectedAdminBasePath, func(root *yaml.Node) error {
		node := mappingValue(root, "client_api_keys")
		if node == nil {
			node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setMappingValue(root, "client_api_keys", node)
		}
		if node.Kind != yaml.MappingNode {
			return errors.New("client_api_keys must be a mapping")
		}
		return mutate(node)
	})
}

func removeMappingValue(mapping *yaml.Node, key string) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}
