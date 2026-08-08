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

	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"ai-proxy/internal/pkg/aiproxyarchive"
	"ai-proxy/internal/pkg/aiproxyclientaccess"
	"ai-proxy/internal/pkg/aiproxyclientauth"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxyusage"
)

type providerAccessDTO struct {
	Mode        string   `json:"mode"`
	ProviderIDs []string `json:"provider_ids"`
}

type clientKeyView struct {
	ID                     string            `json:"id"`
	Enabled                bool              `json:"enabled"`
	CreatedAt              string            `json:"created_at,omitempty"`
	LastUsedAt             string            `json:"last_used_at,omitempty"`
	LastRotatedAt          string            `json:"last_rotated_at,omitempty"`
	RevokedAt              string            `json:"revoked_at,omitempty"`
	ProviderAccess         providerAccessDTO `json:"provider_access"`
	EffectiveProviderIDs   []string          `json:"effective_provider_ids"`
	UnavailableProviderIDs []string          `json:"unavailable_provider_ids"`
	EffectiveModelCount    int               `json:"effective_model_count"`
}

type createClientKeyRequest struct {
	ID             string             `json:"id"`
	Enabled        *bool              `json:"enabled"`
	ProviderAccess *providerAccessDTO `json:"provider_access"`
}

type updateClientKeyRequest struct {
	Enabled *bool `json:"enabled"`
}

type adminKeyModel struct {
	ID                  string   `json:"id"`
	ProviderIDs         []string `json:"provider_ids"`
	SupportedEndpoints  []string `json:"supported_endpoints"`
	ContextWindowTokens int      `json:"context_window_tokens,omitempty"`
	MaxOutputTokens     int      `json:"max_output_tokens,omitempty"`
}

func (h *Handler) listClientAPIKeys(w http.ResponseWriter) {
	records, err := h.clientKeyRecords(context.Background())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "client API key store unavailable")
		return
	}
	snapshot := h.clientKeyCatalog()
	keys := make([]clientKeyView, 0, len(records))
	for _, record := range records {
		keys = append(keys, clientKeyViewFor(record, snapshot))
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"client_api_keys": keys, "writable": true, "hot_reload": true})
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
	if id == "" || id == config.ReservedClientAPIKeyID {
		writeError(w, http.StatusBadRequest, "invalid client API key id")
		return
	}
	if input.ProviderAccess == nil {
		writeError(w, http.StatusBadRequest, "provider_access is required")
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	policy, err := h.validateProviderAccess(*input.ProviderAccess)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
	record := usage.ClientAPIKeyRecord{ID: id, Hash: hash, Enabled: enabled, CreatedAt: time.Now().UTC(), ProviderAccess: policy}

	records, err := h.clientKeyRecords(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "client API key store unavailable")
		return
	}
	if _, exists := records[id]; exists {
		writeError(w, http.StatusConflict, "client API key id already exists")
		return
	}
	records[id] = record
	index, err := h.prepareClientKeyIndex(records)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "prepare client API keys")
		return
	}
	if err := h.usageStore.CreateClientAPIKey(r.Context(), record); err != nil {
		writeError(w, http.StatusConflict, "client API key id already exists")
		return
	}
	h.activateClientKeyIndex(index)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "enabled": enabled, "provider_access": providerAccessView(policy), "api_key": secret, "message": "Copy this API key now. It cannot be displayed again."})
}

func (h *Handler) clientAPIKeyAction(w http.ResponseWriter, r *http.Request, rel string) {
	rest := strings.Trim(strings.TrimPrefix(rel, "/api/client-api-keys/"), "/")
	parts := strings.Split(rest, "/")
	id := strings.ToLower(strings.TrimSpace(parts[0]))
	if id == "" || id == config.ReservedClientAPIKeyID || len(parts) > 2 {
		writeError(w, http.StatusBadRequest, "invalid client API key id")
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if action == "models" && r.Method == http.MethodGet {
		h.getClientAPIKeyModels(w, r.Context(), id)
		return
	}
	if !h.requireAdminMutation(w, r) {
		return
	}

	switch {
	case action == "rotate" && r.Method == http.MethodPost:
		h.rotateClientAPIKey(w, r, id)
	case action == "provider-access" && r.Method == http.MethodPut:
		h.updateClientAPIKeyProviderAccess(w, r, id)
	case action == "" && r.Method == http.MethodPatch:
		h.updateClientAPIKeyEnabled(w, r, id)
	case action == "" && r.Method == http.MethodDelete:
		h.deleteClientAPIKey(w, r, id)
	default:
		writeError(w, http.StatusNotFound, "client API key action not found")
	}
}

func (h *Handler) rotateClientAPIKey(w http.ResponseWriter, r *http.Request, id string) {
	secret, hash, err := generateClientKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate client API key")
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	records, record, ok := h.loadClientKeyForMutation(w, r.Context(), id)
	if !ok {
		return
	}
	now := time.Now().UTC()
	record.Hash, record.Enabled, record.LastRotatedAt, record.RevokedAt = hash, true, &now, nil
	records[id] = record
	index, err := h.prepareClientKeyIndex(records)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "prepare client API keys")
		return
	}
	if err := h.usageStore.RotateClientAPIKey(r.Context(), id, hash, now); err != nil {
		writeError(w, http.StatusNotFound, "client API key not found")
		return
	}
	h.activateClientKeyIndex(index)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "api_key": secret, "message": "Copy this API key now. It cannot be displayed again."})
}

func (h *Handler) updateClientAPIKeyEnabled(w http.ResponseWriter, r *http.Request, id string) {
	var input updateClientKeyRequest
	if !decodeAdminJSON(w, r, &input) || input.Enabled == nil {
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	records, record, ok := h.loadClientKeyForMutation(w, r.Context(), id)
	if !ok {
		return
	}
	record.Enabled = *input.Enabled
	records[id] = record
	index, err := h.prepareClientKeyIndex(records)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "prepare client API keys")
		return
	}
	if err := h.usageStore.SetClientAPIKeyEnabled(r.Context(), id, *input.Enabled); err != nil {
		writeError(w, http.StatusNotFound, "client API key not found")
		return
	}
	h.activateClientKeyIndex(index)
	writeJSON(w, http.StatusOK, clientKeyViewFor(record, h.clientKeyCatalog()))
}

func (h *Handler) updateClientAPIKeyProviderAccess(w http.ResponseWriter, r *http.Request, id string) {
	var input providerAccessDTO
	if !decodeAdminJSON(w, r, &input) {
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	policy, err := h.validateProviderAccess(input)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	records, record, ok := h.loadClientKeyForMutation(w, r.Context(), id)
	if !ok {
		return
	}
	record.ProviderAccess = policy
	records[id] = record
	index, err := h.prepareClientKeyIndex(records)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "prepare client API keys")
		return
	}
	if err := h.usageStore.SetClientAPIKeyProviderAccess(r.Context(), id, policy); err != nil {
		writeError(w, http.StatusNotFound, "client API key not found")
		return
	}
	h.activateClientKeyIndex(index)
	writeJSON(w, http.StatusOK, clientKeyViewFor(record, h.clientKeyCatalog()))
}

func (h *Handler) deleteClientAPIKey(w http.ResponseWriter, r *http.Request, id string) {
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	records, _, ok := h.loadClientKeyForMutation(w, r.Context(), id)
	if !ok {
		return
	}
	delete(records, id)
	index, err := h.prepareClientKeyIndex(records)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "prepare client API keys")
		return
	}
	if h.runtime != nil {
		if err := archive.RemoveAPIKeyScope(h.runtime.ConfigSnapshot().InteractionDir, id); err != nil {
			writeError(w, http.StatusInternalServerError, "remove client interaction archives")
			return
		}
	}
	if err := h.usageStore.DeleteClientAPIKey(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "client API key not found")
		return
	}
	h.activateClientKeyIndex(index)
	if runtime, ok := h.chatGPT.(chatGPTImageRuntime); ok {
		if err := runtime.DeleteChatGPTImageTaskScope(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, "remove client image tasks")
			return
		}
		if err := runtime.DeleteChatGPTImageScope(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, "remove client image assets")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) getClientAPIKeyModels(w http.ResponseWriter, ctx context.Context, id string) {
	records, err := h.clientKeyRecords(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "client API key store unavailable")
		return
	}
	record, ok := records[id]
	if !ok {
		writeError(w, http.StatusNotFound, "client API key not found")
		return
	}
	snapshot := h.clientKeyCatalog()
	models := make([]adminKeyModel, 0)
	for _, modelID := range snapshot.SortedModelIDsForAccess(record.ProviderAccess) {
		candidates := snapshot.CandidatesForAccess(modelID, record.ProviderAccess)
		if len(candidates) == 0 {
			continue
		}
		providerSet, endpointSet := map[string]struct{}{}, map[string]struct{}{}
		for _, candidate := range candidates {
			providerSet[candidate.RouteOwner] = struct{}{}
			for _, endpoint := range candidate.SupportedEndpoints {
				endpointSet[endpoint] = struct{}{}
			}
		}
		models = append(models, adminKeyModel{
			ID: modelID, ProviderIDs: sortedSet(providerSet), SupportedEndpoints: sortedSet(endpointSet),
			ContextWindowTokens: candidates[0].ContextWindowTokens, MaxOutputTokens: candidates[0].MaxOutputTokens,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func (h *Handler) clientKeyRecords(ctx context.Context) (map[string]usage.ClientAPIKeyRecord, error) {
	if h.usageStore == nil {
		return nil, fmt.Errorf("client API key store unavailable")
	}
	records, err := h.usageStore.ListClientAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string]usage.ClientAPIKeyRecord, len(records))
	for id, record := range records {
		record.ProviderAccess = clientaccess.Clone(record.ProviderAccess)
		result[id] = record
	}
	return result, nil
}

func (h *Handler) requireExistingClientAPIKeyID(w http.ResponseWriter, ctx context.Context, value string) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(value))
	if id == "" || id == config.ReservedClientAPIKeyID {
		writeError(w, http.StatusBadRequest, "api_key_id is required")
		return "", false
	}
	records, err := h.clientKeyRecords(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "client API key store unavailable")
		return "", false
	}
	if _, exists := records[id]; !exists {
		writeError(w, http.StatusNotFound, "client API key not found")
		return "", false
	}
	return id, true
}

func (h *Handler) loadClientKeyForMutation(w http.ResponseWriter, ctx context.Context, id string) (map[string]usage.ClientAPIKeyRecord, usage.ClientAPIKeyRecord, bool) {
	records, err := h.clientKeyRecords(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "client API key store unavailable")
		return nil, usage.ClientAPIKeyRecord{}, false
	}
	record, ok := records[id]
	if !ok {
		writeError(w, http.StatusNotFound, "client API key not found")
		return nil, usage.ClientAPIKeyRecord{}, false
	}
	return records, record, true
}

func (h *Handler) prepareClientKeyIndex(records map[string]usage.ClientAPIKeyRecord) (*clientauth.Index, error) {
	runtime, ok := h.runtime.(clientKeyRuntime)
	if !ok {
		return nil, fmt.Errorf("client API key runtime unavailable")
	}
	return runtime.PrepareClientKeyIndex(records)
}

func (h *Handler) activateClientKeyIndex(index *clientauth.Index) {
	h.runtime.(clientKeyRuntime).ActivateClientKeyIndex(index)
}

func (h *Handler) clientKeyCatalog() effectivecatalog.Snapshot {
	if runtime, ok := h.runtime.(effectiveCatalogRuntime); ok {
		return runtime.EffectiveCatalogSnapshot()
	}
	return effectivecatalog.Snapshot{}
}

func (h *Handler) validateProviderAccess(input providerAccessDTO) (clientaccess.Policy, error) {
	policy, err := clientaccess.Normalize(clientaccess.Policy{Mode: clientaccess.Mode(input.Mode), ProviderIDs: input.ProviderIDs})
	if err != nil {
		return clientaccess.Policy{}, err
	}
	known := map[string]struct{}{effectivecatalog.BuiltinProviderID: {}, effectivecatalog.CodexOAuthProviderID: {}}
	if h.runtime != nil {
		for id := range h.runtime.ConfigSnapshot().Providers {
			known[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
		}
	}
	for _, id := range policy.ProviderIDs {
		if _, ok := known[id]; !ok {
			return clientaccess.Policy{}, fmt.Errorf("unknown provider %q", id)
		}
	}
	return policy, nil
}

func clientKeyViewFor(record usage.ClientAPIKeyRecord, snapshot effectivecatalog.Snapshot) clientKeyView {
	view := clientKeyView{ID: record.ID, Enabled: record.Enabled, CreatedAt: record.CreatedAt.UTC().Format(time.RFC3339), ProviderAccess: providerAccessView(record.ProviderAccess)}
	if record.LastUsedAt != nil {
		view.LastUsedAt = record.LastUsedAt.UTC().Format(time.RFC3339)
	}
	if record.LastRotatedAt != nil {
		view.LastRotatedAt = record.LastRotatedAt.UTC().Format(time.RFC3339)
	}
	if record.RevokedAt != nil {
		view.RevokedAt = record.RevokedAt.UTC().Format(time.RFC3339)
	}
	view.EffectiveProviderIDs = snapshot.ProviderIDsForAccess(record.ProviderAccess)
	view.EffectiveModelCount = len(snapshot.SortedModelIDsForAccess(record.ProviderAccess))
	if record.ProviderAccess.Mode == clientaccess.ModeSelected {
		effective := make(map[string]struct{}, len(view.EffectiveProviderIDs))
		for _, id := range view.EffectiveProviderIDs {
			effective[id] = struct{}{}
		}
		for _, id := range record.ProviderAccess.ProviderIDs {
			if _, ok := effective[id]; !ok {
				view.UnavailableProviderIDs = append(view.UnavailableProviderIDs, id)
			}
		}
	}
	return view
}

func providerAccessView(policy clientaccess.Policy) providerAccessDTO {
	return providerAccessDTO{Mode: string(policy.Mode), ProviderIDs: append([]string(nil), policy.ProviderIDs...)}
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return false
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request: multiple JSON values")
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
