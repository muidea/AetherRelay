package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

type fakeRuntime struct {
	cfg config.Config
}

func (f *fakeRuntime) ConfigSnapshot() config.Config { return f.cfg }
func (f *fakeRuntime) UpdateConfig(cfg config.Config) error {
	f.cfg = cfg
	return nil
}

func TestUsageDashboardAndEventsLoopback(t *testing.T) {
	store := usage.NewMemoryStore()
	now := time.Now().UTC().Add(-time.Minute)
	_ = store.Start(context.Background(), usage.StartRecord{
		EventID: "e1", StartedAt: now, APIKeyID: "codex",
		Operation: "chat_completions", ClientEndpoint: "/v1/chat/completions", ClientProtocol: "openai",
	})
	_ = store.Complete(context.Background(), usage.CompleteRecord{
		EventID: "e1", CompletedAt: now.Add(time.Second), Provider: "openai", Model: "gpt-4o",
		InputTokens: 10, OutputTokens: 5, HTTPStatus: 200, Outcome: "success",
	})

	rt := &fakeRuntime{cfg: config.Config{}}
	h := NewHandlerWithUsage("", rt, store)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/usage/dashboard?range=30d", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	summary := body["summary"].(map[string]any)
	if int(summary["requests"].(float64)) != 1 {
		t.Fatalf("requests=%v", summary["requests"])
	}
	if _, ok := body["store"]; ok {
		t.Fatal("dashboard must not expose a storage health status")
	}

	// remote forbidden
	req = httptest.NewRequest(http.MethodGet, "/admin/api/usage/dashboard", nil)
	req.RemoteAddr = "8.8.8.8:99"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote status=%d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/api/usage/events?page_size=10&range=30d", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("events status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/api/usage/export.csv?range=30d", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("export endpoint status=%d, want %d", rec.Code, http.StatusNotFound)
	}

	h = NewHandler("", rt)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("export endpoint without usage store status=%d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestUsageFilterOptionsMergeAndLoopback(t *testing.T) {
	store := usage.NewMemoryStore()
	now := time.Now().UTC().Add(-time.Minute)
	_ = store.CreateClientAPIKey(context.Background(), usage.ClientAPIKeyRecord{ID: "codex", Hash: "sha256:codex", Enabled: true, CreatedAt: now, ProviderAccess: clientaccess.All()})
	_ = store.CreateClientAPIKey(context.Background(), usage.ClientAPIKeyRecord{ID: "unused-key", Hash: "sha256:unused", Enabled: false, CreatedAt: now, ProviderAccess: clientaccess.All()})
	_ = store.CreateClientAPIKey(context.Background(), usage.ClientAPIKeyRecord{ID: "retired", Hash: "sha256:retired", Enabled: true, CreatedAt: now, ProviderAccess: clientaccess.All()})
	_ = store.Start(context.Background(), usage.StartRecord{
		EventID: "e1", StartedAt: now, APIKeyID: "codex", Provider: "openai", Model: "gpt-4o",
	})
	_ = store.Complete(context.Background(), usage.CompleteRecord{
		EventID: "e1", CompletedAt: now.Add(time.Second), Provider: "openai", Model: "gpt-4o",
		InputTokens: 1, OutputTokens: 1, HTTPStatus: 200, Outcome: "success",
	})
	// historical key/provider/model no longer in config
	_ = store.Start(context.Background(), usage.StartRecord{
		EventID: "e2", StartedAt: now, APIKeyID: "retired", Provider: "old-p", Model: "legacy-m",
	})
	_ = store.Complete(context.Background(), usage.CompleteRecord{
		EventID: "e2", CompletedAt: now.Add(time.Second), Provider: "old-p", Model: "legacy-m",
		InputTokens: 1, OutputTokens: 1, HTTPStatus: 200, Outcome: "success",
	})

	rt := &fakeRuntime{cfg: config.Config{
		Providers: map[string]config.Provider{
			"openai": {Name: "openai", Protocol: "openai", Models: []string{"gpt-4o"}},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"gpt-4o":       {ID: "gpt-4o"},
			"catalog-only": {ID: "catalog-only"},
		},
	}}
	h := NewHandlerWithUsage("", rt, store)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/usage/filter-options?range=30d", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "secret-should-not-leak") {
		t.Fatalf("leaked client api key secret")
	}
	var payload struct {
		APIKeyIDs []struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			InConfig bool   `json:"in_config"`
			InUsage  bool   `json:"in_usage"`
		} `json:"api_key_ids"`
		Providers []struct {
			ID       string `json:"id"`
			InConfig bool   `json:"in_config"`
			InUsage  bool   `json:"in_usage"`
		} `json:"providers"`
		Models []struct {
			ID       string `json:"id"`
			InConfig bool   `json:"in_config"`
			InUsage  bool   `json:"in_usage"`
		} `json:"models"`
		Outcomes       []string `json:"outcomes"`
		HistoryQueryOK bool     `json:"history_query_ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["store"]; ok {
		t.Fatal("filter options must not expose a storage health status")
	}
	if !payload.HistoryQueryOK {
		t.Fatalf("expected history_query_ok")
	}
	keyByID := map[string]struct {
		Status   string
		InConfig bool
		InUsage  bool
	}{}
	for _, k := range payload.APIKeyIDs {
		keyByID[k.ID] = struct {
			Status   string
			InConfig bool
			InUsage  bool
		}{k.Status, k.InConfig, k.InUsage}
	}
	for _, id := range []string{"codex", "unused-key", "retired"} {
		if _, ok := keyByID[id]; !ok {
			t.Fatalf("missing key %s in %#v", id, keyByID)
		}
	}
	if keyByID["codex"].Status != "active" || !keyByID["codex"].InConfig || !keyByID["codex"].InUsage {
		t.Fatalf("codex = %#v", keyByID["codex"])
	}
	if keyByID["unused-key"].Status != "disabled" || !keyByID["unused-key"].InConfig || keyByID["unused-key"].InUsage {
		t.Fatalf("unused-key = %#v", keyByID["unused-key"])
	}
	if keyByID["retired"].Status != "active" || !keyByID["retired"].InConfig || !keyByID["retired"].InUsage {
		t.Fatalf("retired = %#v", keyByID["retired"])
	}

	provByID := map[string]struct{ InConfig, InUsage bool }{}
	for _, p := range payload.Providers {
		provByID[p.ID] = struct{ InConfig, InUsage bool }{p.InConfig, p.InUsage}
	}
	if !provByID["openai"].InConfig || !provByID["openai"].InUsage {
		t.Fatalf("openai provider = %#v", provByID["openai"])
	}
	if provByID["old-p"].InConfig || !provByID["old-p"].InUsage {
		t.Fatalf("old-p = %#v", provByID["old-p"])
	}

	modelByID := map[string]struct{ InConfig, InUsage bool }{}
	for _, m := range payload.Models {
		modelByID[m.ID] = struct{ InConfig, InUsage bool }{m.InConfig, m.InUsage}
	}
	if !modelByID["gpt-4o"].InConfig || !modelByID["gpt-4o"].InUsage {
		t.Fatalf("gpt-4o = %#v", modelByID["gpt-4o"])
	}
	if _, ok := modelByID["catalog-only"]; ok {
		t.Fatalf("metadata-only model must not be listed: %#v", modelByID["catalog-only"])
	}
	if modelByID["legacy-m"].InConfig || !modelByID["legacy-m"].InUsage {
		t.Fatalf("legacy-m = %#v", modelByID["legacy-m"])
	}

	foundSuccess, foundUpstreamTruncated := false, false
	for _, o := range payload.Outcomes {
		if o == "success" {
			foundSuccess = true
		}
		if o == "upstream_truncated" {
			foundUpstreamTruncated = true
		}
	}
	if !foundSuccess {
		t.Fatalf("outcomes missing success: %#v", payload.Outcomes)
	}
	if !foundUpstreamTruncated {
		t.Fatalf("outcomes missing upstream_truncated: %#v", payload.Outcomes)
	}

	// remote forbidden
	req = httptest.NewRequest(http.MethodGet, "/admin/api/usage/filter-options", nil)
	req.RemoteAddr = "8.8.8.8:99"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote status=%d", rec.Code)
	}
}

func TestUsageFilterOptionsStoreFailureDegrades(t *testing.T) {
	store := usage.NewMemoryStore()
	_ = store.CreateClientAPIKey(context.Background(), usage.ClientAPIKeyRecord{ID: "codex", Hash: "sha256:codex", Enabled: true, CreatedAt: time.Now().UTC(), ProviderAccess: clientaccess.All()})
	_ = store.Close()
	rt := &fakeRuntime{cfg: config.Config{
		Providers: map[string]config.Provider{
			"openai": {Name: "openai"},
		},
		ModelMetadata: map[string]config.ModelMetadata{
			"gpt-4o": {ID: "gpt-4o"},
		},
	}}
	h := NewHandlerWithUsage("", rt, store)
	req := httptest.NewRequest(http.MethodGet, "/admin/api/usage/filter-options?range=7d", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		APIKeyIDs []struct {
			ID string `json:"id"`
		} `json:"api_key_ids"`
		HistoryQueryOK bool `json:"history_query_ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.HistoryQueryOK {
		t.Fatalf("expected history_query_ok=false")
	}
	ids := map[string]bool{}
	for _, k := range payload.APIKeyIDs {
		ids[k.ID] = true
	}
	if !ids["codex"] {
		t.Fatalf("config keys missing on degrade: %#v", ids)
	}
}
