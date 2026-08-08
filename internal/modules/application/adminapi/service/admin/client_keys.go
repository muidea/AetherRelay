package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"ai-proxy/internal/pkg/aiproxyarchive"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxyusage"
)

type clientKeyView struct {
	ID            string `json:"id"`
	Enabled       bool   `json:"enabled"`
	CreatedAt     string `json:"created_at,omitempty"`
	LastUsedAt    string `json:"last_used_at,omitempty"`
	LastRotatedAt string `json:"last_rotated_at,omitempty"`
	RevokedAt     string `json:"revoked_at,omitempty"`
}

type createClientKeyRequest struct {
	ID      string `json:"id"`
	Enabled *bool  `json:"enabled"`
}

type updateClientKeyRequest struct {
	Enabled *bool `json:"enabled"`
}

func (h *Handler) listClientAPIKeys(w http.ResponseWriter) {
	if h.usageStore != nil {
		if records, err := h.usageStore.ListClientAPIKeys(context.Background()); err == nil {
			keys := make([]clientKeyView, 0, len(records))
			for _, r := range records {
				v := clientKeyView{ID: r.ID, Enabled: r.Enabled, CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339)}
				if r.LastUsedAt != nil {
					v.LastUsedAt = r.LastUsedAt.UTC().Format(time.RFC3339)
				}
				if r.LastRotatedAt != nil {
					v.LastRotatedAt = r.LastRotatedAt.UTC().Format(time.RFC3339)
				}
				if r.RevokedAt != nil {
					v.RevokedAt = r.RevokedAt.UTC().Format(time.RFC3339)
				}
				keys = append(keys, v)
			}
			sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
			writeJSON(w, 200, map[string]any{"client_api_keys": keys, "writable": true, "hot_reload": true})
			return
		}
	}
	writeError(w, http.StatusServiceUnavailable, "client API key store unavailable")
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
	if h.usageStore != nil {
		if err := h.usageStore.CreateClientAPIKey(r.Context(), usage.ClientAPIKeyRecord{ID: id, Hash: hash, Enabled: enabled, CreatedAt: time.Now().UTC()}); err != nil {
			writeError(w, 409, "client API key id already exists")
			return
		}
		if err := h.refreshClientKeyRuntime(r.Context()); err != nil {
			writeError(w, 500, "activate client API keys")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 201, map[string]any{"id": id, "enabled": enabled, "api_key": secret, "message": "Copy this API key now. It cannot be displayed again."})
		return
	}
	writeError(w, http.StatusServiceUnavailable, "client API key store unavailable")
}

func (h *Handler) clientAPIKeyAction(w http.ResponseWriter, r *http.Request, rel string) {
	if !h.requireAdminMutation(w, r) {
		return
	}
	if h.usageStore != nil {
		rest := strings.TrimPrefix(rel, "/api/client-api-keys/")
		rotate := strings.HasSuffix(rest, "/rotate")
		id := strings.ToLower(strings.Trim(strings.TrimSuffix(rest, "/rotate"), "/"))
		if id == "" || id == config.ReservedClientAPIKeyID {
			writeError(w, 400, "invalid client API key id")
			return
		}
		if rotate && r.Method == http.MethodPost {
			secret, hash, err := generateClientKey()
			if err != nil {
				writeError(w, 500, "generate client API key")
				return
			}
			if err := h.usageStore.RotateClientAPIKey(r.Context(), id, hash, time.Now().UTC()); err != nil {
				writeError(w, 404, "client API key not found")
				return
			}
			if err := h.refreshClientKeyRuntime(r.Context()); err != nil {
				writeError(w, 500, "activate client API keys")
				return
			}
			writeJSON(w, 200, map[string]any{"id": id, "api_key": secret, "message": "Copy this API key now. It cannot be displayed again."})
			return
		}
		if !rotate && r.Method == http.MethodPatch {
			var in updateClientKeyRequest
			if !decodeAdminJSON(w, r, &in) || in.Enabled == nil {
				return
			}
			if err := h.usageStore.SetClientAPIKeyEnabled(r.Context(), id, *in.Enabled); err != nil {
				writeError(w, 404, "client API key not found")
				return
			}
			if err := h.refreshClientKeyRuntime(r.Context()); err != nil {
				writeError(w, 500, "activate client API keys")
				return
			}
			writeJSON(w, 200, clientKeyView{ID: id, Enabled: *in.Enabled})
			return
		}
		if !rotate && r.Method == http.MethodDelete {
			if err := h.usageStore.DeleteClientAPIKey(r.Context(), id); err != nil {
				writeError(w, 404, "client API key not found")
				return
			}
			if err := h.refreshClientKeyRuntime(r.Context()); err != nil {
				writeError(w, 500, "activate client API keys")
				return
			}
			if h.runtime != nil {
				if err := archive.RemoveAPIKeyScope(h.runtime.ConfigSnapshot().InteractionDir, id); err != nil {
					writeError(w, http.StatusInternalServerError, "remove client interaction archives")
					return
				}
			}
			w.WriteHeader(204)
			return
		}
	}
	writeError(w, http.StatusServiceUnavailable, "client API key store unavailable")
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
func (h *Handler) refreshClientKeyRuntime(ctx context.Context) error {
	records, err := h.usageStore.ListClientAPIKeys(ctx)
	if err != nil {
		return err
	}
	cfg := h.runtime.ConfigSnapshot()
	_ = records // UpdateConfig reloads the authoritative records from DuckDB.
	return h.runtime.UpdateConfig(cfg)
}
