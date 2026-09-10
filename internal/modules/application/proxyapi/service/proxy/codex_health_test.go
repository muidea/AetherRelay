package proxy

import (
	clientaccess "aetherrelay/internal/pkg/aetherrelayclientaccess"
	clientauth "aetherrelay/internal/pkg/aetherrelayclientauth"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	metrics "aetherrelay/internal/pkg/aetherrelaymetrics"
	"aetherrelay/internal/pkg/aetherrelaymetricsport"
)

func TestCodexPermanentAuthenticationFailureContract(t *testing.T) {
	h := &Handler{}
	retryable := false
	failure := codexresponses.NewFailure(codexresponses.KindAuthentication, 0, nil)
	failure.UnavailableReason = "credential_permanently_invalid"
	failure.Retryable = &retryable
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	h.writeCodexResponsesError(w, r, nil, time.Now(), "codexoauth", "gpt-test", true, failure)
	var response APIErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusServiceUnavailable || response.Error.Code != ErrorCodeUpstreamAuthRequired || response.Error.Type != "authentication_error" || response.Error.Retryable == nil || *response.Error.Retryable || response.Error.FailureClass != "credential_permanently_invalid" {
		t.Fatalf("auth terminal status=%d response=%+v", w.Code, response)
	}

	messageWriter := httptest.NewRecorder()
	messageRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	h.writeCodexResponsesError(messageWriter, messageRequest, nil, time.Now(), "codexoauth", "gpt-test", true, failure)
	var anthropic AnthropicErrorResponse
	if err := json.Unmarshal(messageWriter.Body.Bytes(), &anthropic); err != nil {
		t.Fatal(err)
	}
	if messageWriter.Code != http.StatusServiceUnavailable || anthropic.Error.Type != "authentication_error" || !strings.Contains(anthropic.Error.Message, ErrorCodeUpstreamAuthRequired) {
		t.Fatalf("Anthropic auth terminal status=%d response=%+v", messageWriter.Code, anthropic)
	}

	payload := codexWebsocketErrorPayload("authentication_error", ErrorCodeUpstreamAuthRequired, "reauthenticate", &retryable)
	var websocketEvent struct {
		Error APIError `json:"error"`
	}
	if err := json.Unmarshal(payload, &websocketEvent); err != nil {
		t.Fatal(err)
	}
	if websocketEvent.Error.Code != ErrorCodeUpstreamAuthRequired || websocketEvent.Error.Type != "authentication_error" || websocketEvent.Error.Retryable == nil || *websocketEvent.Error.Retryable {
		t.Fatalf("WebSocket auth terminal=%s", payload)
	}
}

func TestCodexAdmissionAndLocalFailuresDoNotOpenProviderCircuit(t *testing.T) {
	// CP-FAIL-019: exercise the HTTP error writer, not just the metrics helper.
	registry := metrics.NewRegistry()
	h := &Handler{metricsRegistry: metricsport.AsPort(registry)}
	registry.RecordRequestPlan("codexoauth", "gpt-test", "responses", 502, time.Second, "upstream_failed", "", "", "", "")
	for _, kind := range []codexresponses.ErrorKind{codexresponses.KindProviderUnavailable, codexresponses.KindStreamLifetime, codexresponses.KindClientCanceled, codexresponses.KindClientWrite} {
		for range 4 {
			f := codexresponses.NewFailure(kind, 0, nil)
			if kind == codexresponses.KindProviderUnavailable {
				f.RetryAfterSeconds = 17
				f.UnavailableReason = "accounts_cooling"
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/v1/responses", nil)
			h.writeCodexResponsesError(w, r, nil, time.Now(), "codexoauth", "gpt-test", true, f)
			if kind == codexresponses.KindProviderUnavailable && (w.Code != 503 || w.Header().Get("Retry-After") != "17") {
				t.Fatalf("HTTP=%d header=%v", w.Code, w.Header())
			}
			if kind == codexresponses.KindStreamLifetime && w.Code != 504 {
				t.Fatalf("lifetime HTTP=%d", w.Code)
			}
		}
	}
	health := registry.ProviderHealthSnapshot()["codexoauth"]
	if health.Failures != 1 || health.ConsecutiveFailures != 1 || health.CircuitState != "closed" {
		t.Fatalf("local errors poisoned health: %+v", health)
	}
	if model, _ := registry.ProviderModelHealth("codexoauth", "gpt-test"); model.Failures != 1 {
		t.Fatalf("model health=%+v", model)
	}
	registry.RecordRequestPlan("codexoauth", "gpt-test", "responses", 200, time.Second, "success", "", "", "", "")
	if got := registry.ProviderHealthSnapshot()["codexoauth"]; got.ConsecutiveFailures != 0 {
		t.Fatalf("recovery=%+v", got)
	}
}

func TestActiveCircuitProvidesRetryAfter(t *testing.T) {
	// CP-FAIL-019: a fast routing denial still reports the known recovery time.
	cfg := mustHandlerConfig(config.Config{ModelMetadata: map[string]config.ModelMetadata{"gpt-test": {ID: "gpt-test"}}, Providers: map[string]config.Provider{
		"p": {Name: "p", Protocol: "openai", BaseURL: "https://example.invalid", APIKey: "test", Models: []string{"gpt-test"}, Endpoints: []string{config.ProviderEndpointResponses}},
	}})
	registry := metrics.NewRegistry()
	h := NewHandler(cfg, usage.NewMemoryStore(), nil, registry)
	for range 3 {
		registry.RecordRequestPlan("p", "gpt-test", "responses", 502, time.Second, "upstream_failed", "", "", "", "")
	}
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req = req.WithContext(clientauth.WithClientIdentity(req.Context(), clientauth.ClientIdentity{KeyID: "test", ProviderAccess: clientaccess.All()}))
	plans, failure := h.resolveTransportPlans(req, "gpt-test")
	if len(plans) != 0 || failure == nil || failure.FailureClass != "circuit_open" || failure.RetryAfterSeconds < 25 || failure.RetryAfterSeconds > 30 {
		t.Fatalf("plans=%v failure=%+v", plans, failure)
	}
	w := httptest.NewRecorder()
	writeClientProtocolError(w, http.StatusServiceUnavailable, ClientProtocolOpenAI, *failure)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing retry header")
	}
}
