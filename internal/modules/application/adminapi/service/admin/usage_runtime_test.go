package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metrics "aetherrelay/internal/pkg/aetherrelaymetrics"
	metricsport "aetherrelay/internal/pkg/aetherrelaymetricsport"
)

func TestUsageRuntimeUsesLiveMetricsWithoutUsageStore(t *testing.T) {
	registry := metrics.NewRegistry()
	h := NewHandler("", &fakeRuntime{}).WithMetrics(registry)
	check := func(want int64) {
		t.Helper()
		// Historical filters must not change the global live value.
		req := httptest.NewRequest(http.MethodGet, "/admin/api/usage/runtime?range=30d&api_key_id=other&provider=other", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("runtime = %d %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 2 || body["scope"] != "process" || body["inflight"] != float64(want) {
			t.Fatalf("runtime = %v, want inflight %d", body, want)
		}
	}
	check(0)
	if err := registry.BeginRequest(); err != nil {
		t.Fatal(err)
	}
	if err := registry.BeginRequest(); err != nil {
		t.Fatal(err)
	}
	check(2)
	if err := registry.EndRequest(); err != nil {
		t.Fatal(err)
	}
	check(1)
	if err := registry.EndRequest(); err != nil {
		t.Fatal(err)
	}
	check(0)
}

type runtimeSnapshotPort struct {
	metricsport.Port
	data []byte
	err  error
}

func (p runtimeSnapshotPort) StatsJSON() ([]byte, error) { return p.data, p.err }

func TestUsageRuntimeUnavailableDoesNotReportZero(t *testing.T) {
	for _, source := range []any{
		nil,
		runtimeSnapshotPort{err: fmt.Errorf("offline")},
		runtimeSnapshotPort{data: []byte(`null`)},
		runtimeSnapshotPort{data: []byte(`{"requests":{}}`)},
		runtimeSnapshotPort{data: []byte(`{"requests":{"inflight":-1}}`)},
		runtimeSnapshotPort{data: []byte(`invalid`)},
	} {
		h := NewHandler("", &fakeRuntime{}).WithMetrics(source)
		req := httptest.NewRequest(http.MethodGet, "/admin/api/usage/runtime", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
}

func TestUsageRuntimeRequiresAdminSessionWithCustomBasePath(t *testing.T) {
	h := newAuthHandler(t, enabledAuthConfig(t, "ops-admin", "s3cret-pass")).WithMetrics(metrics.NewRegistry())
	query := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/ops/AetherRelay/api/usage/runtime", nil)
		req.RemoteAddr = "203.0.113.8:9"
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := query(nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/ops/AetherRelay/api/auth/login", strings.NewReader(`{"username":"ops-admin","password":"s3cret-pass"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.8:9"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if rec.Code != http.StatusOK || len(cookies) == 0 {
		t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
	}
	if rec := query(cookies[0]); rec.Code != http.StatusOK {
		t.Fatalf("authenticated runtime = %d %s", rec.Code, rec.Body.String())
	}
}
