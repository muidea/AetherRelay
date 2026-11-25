package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aetherrelay/internal/pkg/aetherrelayarchive"
	"aetherrelay/internal/pkg/aetherrelaycodex"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelaymetrics"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

func TestNativeHTTPResponses400RecordsStructuredUpstreamError(t *testing.T) {
	const upstreamMsg = "Invalid schema for response_format 'workorch_runtime_candidate': schema must have type 'object' and not have 'oneOf'/'anyOf'/'allOf'/'enum'/'not' at the top level. Found 'anyOf'."
	const upstreamBody = `{"error":{"type":"invalid_request_error","code":"invalid_json_schema","param":"text.format.schema","message":"` + upstreamMsg + `"}}`

	for _, fullContent := range []bool{false, true} {
		t.Run(fmt.Sprintf("archive_full_content=%v", fullContent), func(t *testing.T) {
			tmpDir := t.TempDir()
			interactionDir := filepath.Join(tmpDir, "interactions")
			recorder, err := archive.NewRecorderOptions(interactionDir, archive.RecorderOptions{
				MaxRounds:   10,
				FullContent: fullContent,
			})
			if err != nil {
				t.Fatal(err)
			}
			cfg := mustHandlerConfig(config.Config{
				ListenAddr:         ":0",
				InteractionDir:     interactionDir,
				ArchiveFullContent: fullContent,
				Providers: map[string]config.Provider{
					"deepseek": {
						Name:      "deepseek",
						Protocol:  "openai",
						BaseURL:   "https://api.deepseek.com",
						APIKey:    "sk-test-deepseek",
						Models:    []string{"deepseek-flash"},
						Endpoints: []string{config.ProviderEndpointResponses},
					},
				},
			})
			reg := metrics.NewRegistry()
			h := newTestProxyHandler(cfg, usage.NewMemoryStore(), recorder, reg)
			h.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return testResponse(http.StatusBadRequest, "application/json", upstreamBody), nil
			})

			req := newRequest(http.MethodPost, "/v1/responses", `{"model":"deepseek-flash","stream":true,"input":"hello"}`)
			rec := newResponseRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest || rec.Body.String() != upstreamBody {
				t.Fatalf("unexpected client response: status=%d body=%s", rec.Code, rec.Body.String())
			}

			events := usageEvents(t, h.usageStore)
			if len(events) != 1 {
				t.Fatalf("events=%+v", events)
			}
			e := events[0]
			wantUpstream := aetherrelaycodex.UpstreamError{
				UpstreamErrorType:    "invalid_request_error",
				UpstreamErrorCode:    "invalid_json_schema",
				UpstreamErrorParam:   "text.format.schema",
				UpstreamErrorMessage: upstreamMsg,
			}
			if e.Outcome != "error" || e.ErrorCode != ErrorCodeInvalidRequest || e.FailureClass != "" || e.UpstreamError != wantUpstream {
				t.Fatalf("unexpected usage event: %+v", e)
			}

			tr, err := h.usageStore.ExecutionTrace(context.Background(), e.EventID)
			if err != nil || tr == nil || len(tr.Attempts) != 1 {
				t.Fatalf("trace=%+v err=%v", tr, err)
			}
			attempt := tr.Attempts[0]
			if !attempt.Failed || !attempt.Finished || attempt.UpstreamError != wantUpstream || attempt.ErrorCode != ErrorCodeInvalidRequest {
				t.Fatalf("unexpected trace attempt: %+v", attempt)
			}
			if summary := tr.Summary(); summary.PrimaryCause != "invalid_json_schema" {
				t.Fatalf("primary_cause=%q, want invalid_json_schema", summary.PrimaryCause)
			}
			detail := usage.BuildTraceDetail(e, tr)
			foundUpstreamEvidence := false
			for _, field := range detail.Evidence {
				if field.Field == "upstream_error" {
					foundUpstreamEvidence = true
					if field.State != "saved" {
						t.Fatalf("upstream_error evidence state=%q, want saved", field.State)
					}
				}
			}
			if !foundUpstreamEvidence {
				t.Fatalf("missing upstream_error evidence in %+v", detail.Evidence)
			}

			roundDir := filepath.Join(interactionDir, "test-client", "000001")
			metaRaw, err := os.ReadFile(filepath.Join(roundDir, "metadata.json"))
			if err != nil {
				t.Fatal(err)
			}
			var meta map[string]any
			if err := json.Unmarshal(metaRaw, &meta); err != nil {
				t.Fatal(err)
			}
			if meta["error_code"] != ErrorCodeInvalidRequest ||
				meta["upstream_error_type"] != "invalid_request_error" ||
				meta["upstream_error_code"] != "invalid_json_schema" ||
				meta["upstream_error_param"] != "text.format.schema" ||
				meta["upstream_error_message"] != upstreamMsg {
				t.Fatalf("unexpected metadata.json: %s", metaRaw)
			}

			upRespRaw, err := os.ReadFile(filepath.Join(roundDir, "upstream_response.json"))
			if err != nil {
				t.Fatal(err)
			}
			var upResp map[string]any
			if err := json.Unmarshal(upRespRaw, &upResp); err != nil {
				t.Fatal(err)
			}
			if upResp["upstream_error_type"] != "invalid_request_error" ||
				upResp["upstream_error_code"] != "invalid_json_schema" ||
				upResp["upstream_error_param"] != "text.format.schema" ||
				upResp["upstream_error_message"] != upstreamMsg {
				t.Fatalf("unexpected upstream_response.json: %s", upRespRaw)
			}

			_, respErr := os.Stat(filepath.Join(roundDir, "response.json"))
			if !fullContent && !os.IsNotExist(respErr) {
				t.Fatalf("response.json should not be created when archive_full_content=false")
			}
			if fullContent && respErr != nil {
				t.Fatalf("expected response.json when archive_full_content=true: %v", respErr)
			}

			statsPayload, err := reg.StatsJSON()
			if err != nil {
				t.Fatal(err)
			}
			var snap metrics.StatsJSON
			if err := json.Unmarshal(statsPayload, &snap); err != nil {
				t.Fatal(err)
			}
			if snap.Errors.ByStatusCode["400"] != 1 || snap.Errors.ByStatusCode["-2"] != 0 {
				t.Fatalf("unexpected error metrics: %+v", snap.Errors.ByStatusCode)
			}
			metricsBytes, err := h.metricsRegistry.Prometheus()
			if err != nil {
				t.Fatal(err)
			}
			metricsText := string(metricsBytes)
			if strings.Contains(metricsText, "aetherrelay_provider_admission_denials_total{") {
				t.Fatalf("unexpected admission denial recorded in prometheus metrics:\n%s", metricsText)
			}
		})
	}
}

func TestNativeHTTPUpstreamErrorPreservesDiagnosticTextAndSkipsHTML(t *testing.T) {
	t.Run("message mentioning Bearer/sk-/api_key is preserved in archive and usage", func(t *testing.T) {
		tmpDir := t.TempDir()
		h := testHandler("https://upstream.test", tmpDir, "openai")
		const msg = "bad Bearer sk-secret-token and api_key"
		const rawBody = `{"error":{"type":"invalid_request_error","code":"invalid_key","message":"` + msg + `"}}`
		h.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return testResponse(http.StatusBadRequest, "application/json", rawBody), nil
		})
		rec := newResponseRecorder()
		h.ServeHTTP(rec, newRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-test","input":"hi"}`))
		if rec.Code != http.StatusBadRequest || rec.Body.String() != rawBody {
			t.Fatalf("client response altered: %d %s", rec.Code, rec.Body.String())
		}
		events := usageEvents(t, h.usageStore)
		if len(events) != 1 || events[0].UpstreamErrorCode != "invalid_key" || events[0].UpstreamErrorMessage != msg {
			t.Fatalf("unexpected diagnostic preservation in usage event: %+v", events)
		}
	})

	t.Run("non-JSON HTML 502 keeps UpstreamError empty and passes through", func(t *testing.T) {
		tmpDir := t.TempDir()
		h := testHandler("https://upstream.test", tmpDir, "openai")
		htmlBody := `<html><body>502 Bad Gateway</body></html>`
		h.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return testResponse(http.StatusBadGateway, "text/html", htmlBody), nil
		})
		rec := newResponseRecorder()
		h.ServeHTTP(rec, newRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-test","input":"hi"}`))
		if rec.Code != http.StatusBadGateway || rec.Body.String() != htmlBody {
			t.Fatalf("client response altered: %d %s", rec.Code, rec.Body.String())
		}
		events := usageEvents(t, h.usageStore)
		if len(events) != 1 || events[0].ErrorCode != ErrorCodeUpstreamUnavailable || !events[0].UpstreamError.Empty() {
			t.Fatalf("unexpected HTML 502 usage event: %+v", events)
		}
	})
}

func TestNativeAnthropicAndConversionUpstream400CaptureStructuredError(t *testing.T) {
	t.Run("native /v1/messages", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfg := mustHandlerConfig(config.Config{
			ListenAddr:     ":0",
			InteractionDir: filepath.Join(tmpDir, "interactions"),
			Providers: map[string]config.Provider{
				"anthropic": {
					Name:      "anthropic",
					Protocol:  "anthropic",
					BaseURL:   "https://api.anthropic.test",
					APIKey:    "sk-ant",
					Models:    []string{"claude-test"},
					Endpoints: []string{config.ProviderEndpointMessages},
				},
			},
		})
		recorder, err := archive.NewRecorderOptions(cfg.InteractionDir, archive.RecorderOptions{MaxRounds: 10, FullContent: false})
		if err != nil {
			t.Fatal(err)
		}
		h := newTestProxyHandler(cfg, usage.NewMemoryStore(), recorder, metrics.NewRegistry())
		const rawBody = `{"type":"error","error":{"type":"invalid_request_error","message":"bad anthropic input"}}`
		h.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return testResponse(http.StatusBadRequest, "application/json", rawBody), nil
		})
		rec := httptest.NewRecorder()
		req := newRequest(http.MethodPost, "/v1/messages", `{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || rec.Body.String() != rawBody {
			t.Fatalf("unexpected response: %d %s", rec.Code, rec.Body.String())
		}
		events := usageEvents(t, h.usageStore)
		if len(events) != 1 {
			t.Fatal(events)
		}
		e := events[0]
		if e.ErrorCode != ErrorCodeInvalidRequest || e.UpstreamErrorType != "invalid_request_error" || e.UpstreamErrorMessage != "bad anthropic input" {
			t.Fatalf("unexpected anthropic usage event: %+v", e)
		}
	})

	t.Run("conversion responses->anthropic", func(t *testing.T) {
		cfg := mustHandlerConfig(config.Config{
			ListenAddr: ":0",
			Providers: map[string]config.Provider{
				"anthropic": {
					Name:      "anthropic",
					Protocol:  "anthropic",
					BaseURL:   "https://api.anthropic.test",
					APIKey:    "sk-ant",
					Models:    []string{"claude-test"},
					Endpoints: []string{config.ProviderEndpointMessages},
				},
			},
			ModelMetadata: map[string]config.ModelMetadata{
				"claude-test": {
					ID:                  "claude-test",
					ContextWindowTokens: 128000,
					MaxOutputTokens:     4096,
					ConversionCapabilities: map[string]config.ConversionCapability{
						config.ProviderEndpointMessages: {Level: 1, Text: true},
					},
				},
			},
		})
		h := newTestProxyHandler(cfg, usage.NewMemoryStore(), nil, metrics.NewRegistry())
		h.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return testResponse(http.StatusBadRequest, "application/json", `{"type":"error","error":{"type":"invalid_request_error","message":"converted upstream rejected"}}`), nil
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newRequest(http.MethodPost, "/v1/responses", `{"model":"claude-test","input":"hi"}`))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "converted upstream rejected") {
			t.Fatalf("unexpected converted error response: %d %s", rec.Code, rec.Body.String())
		}
		events := usageEvents(t, h.usageStore)
		if len(events) != 1 {
			t.Fatal(events)
		}
		e := events[0]
		if e.ErrorCode != ErrorCodeInvalidRequest || e.UpstreamErrorType != "invalid_request_error" || e.UpstreamErrorMessage != "converted upstream rejected" {
			t.Fatalf("unexpected conversion usage event: %+v", e)
		}
	})
}

func TestHandleBufferedResponse429RetryAfterAndSSEFailed(t *testing.T) {
	t.Run("handleBufferedResponse 429 with Retry-After", func(t *testing.T) {
		cfg := mustHandlerConfig(config.Config{
			ListenAddr: ":0",
			Providers: map[string]config.Provider{
				"anthropic": {
					Name:      "anthropic",
					Protocol:  "anthropic",
					BaseURL:   "https://anthropic.test",
					APIKey:    "k",
					Models:    []string{"shared-model"},
					Endpoints: []string{config.ProviderEndpointMessages},
					Priority:  200,
					Fallback:  true,
				},
				"openai": {
					Name:      "openai",
					Protocol:  "openai",
					BaseURL:   "https://openai.test",
					APIKey:    "k",
					Models:    []string{"shared-model"},
					Endpoints: []string{config.ProviderEndpointResponses},
					Priority:  100,
					Fallback:  true,
				},
			},
			ModelMetadata: map[string]config.ModelMetadata{
				"shared-model": {
					ID:                  "shared-model",
					ContextWindowTokens: 128000,
					MaxOutputTokens:     4096,
					ConversionCapabilities: map[string]config.ConversionCapability{
						config.ProviderEndpointMessages: {Level: 1, Text: true},
					},
				},
			},
		})
		h := newTestProxyHandler(cfg, usage.NewMemoryStore(), nil, metrics.NewRegistry())
		h.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp := testResponse(http.StatusTooManyRequests, "application/json", `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"too many requests"}}`)
			resp.Header.Set("Retry-After", "12")
			return resp, nil
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newRequest(http.MethodPost, "/v1/responses", `{"model":"shared-model","input":"hi"}`))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		events := usageEvents(t, h.usageStore)
		if len(events) != 1 {
			t.Fatal(events)
		}
		e := events[0]
		if e.ErrorCode != "rate_limit" || e.RetryAfterSeconds != 12 || e.UpstreamErrorCode != "rate_limit_exceeded" || e.UpstreamErrorType != "rate_limit_error" {
			t.Fatalf("unexpected 429 event: %+v", e)
		}
	})

	t.Run("SSE response.failed extracts UpstreamError and preserves upstream_failed outcome", func(t *testing.T) {
		tmpDir := t.TempDir()
		h := testHandler("https://upstream.test", tmpDir, "openai")
		h.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := strings.Join([]string{
				`data: {"type":"response.created","response":{"id":"resp_f","model":"gpt-test"}}`,
				"",
				`data: {"type":"response.failed","response":{"id":"resp_f","error":{"type":"server_error","code":"upstream_overloaded","param":"input","message":"provider down"}}}`,
				"",
			}, "\n")
			return sseResponse(body), nil
		})
		rec := newResponseRecorder()
		h.ServeHTTP(rec, newRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-test","stream":true,"input":"hi"}`))

		events := usageEvents(t, h.usageStore)
		if len(events) != 1 {
			t.Fatal(events)
		}
		e := events[0]
		if e.Outcome != "upstream_failed" || e.UpstreamErrorCode != "upstream_overloaded" || e.UpstreamErrorType != "server_error" || e.UpstreamErrorParam != "input" || e.UpstreamErrorMessage != "provider down" {
			t.Fatalf("unexpected SSE response.failed event: %+v", e)
		}
		statsPayload, err := h.metricsRegistry.StatsJSON()
		if err != nil {
			t.Fatal(err)
		}
		var snap metrics.StatsJSON
		if err := json.Unmarshal(statsPayload, &snap); err != nil {
			t.Fatal(err)
		}
		if snap.Errors.ByStatusCode["-2"] != 1 {
			t.Fatalf("expected single -2 upstream error count, got %+v", snap.Errors.ByStatusCode)
		}
	})
}
