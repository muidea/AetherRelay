package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	aetherrelayarchive "aetherrelay/internal/pkg/aetherrelayarchive"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

const (
	// archiveClientSecret 必须等于测试夹具注册的客户端 Key：认证先于归档，
	// 未注册的凭据会在写归档之前就返回 401。
	archiveClientSecret   = "test-client-key"
	archiveCookieSecret   = "cookie-session-secret"
	archiveSessionSecret  = "session-identity-secret"
	archiveUpstreamSecret = "Bearer upstream-access-token"
	archiveTurnState      = "state-secret-value"
)

// newArchiveFidelityHandler 构造开启归档的 Codex handler，并按 CP-OBS-009 的取值
// 切换 header 保真。
func newArchiveFidelityHandler(t *testing.T, unredacted bool, executor codexresponses.Executor) (*Handler, string) {
	t.Helper()
	root := t.TempDir()
	cfg := mustHandlerConfig(config.Config{
		InteractionDir:           filepath.Join(root, "interactions"),
		VerboseLogging:           true,
		ArchiveInteractions:      true,
		ArchiveUnredactedHeaders: unredacted,
		ArchiveFullContent:       true,
		CodexOAuth:               config.CodexOAuthConfig{},
	})
	recorder, err := aetherrelayarchive.NewRecorderOptions(cfg.InteractionDir, aetherrelayarchive.RecorderOptions{MaxRounds: 10, ScopeByAPIKey: true})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(cfg, usage.NewMemoryStore(), recorder, nil).WithCodexResponsesExecutor(executor)
	handler.ReplaceEffectiveCatalog(effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: "gpt-5.2-codex"}}}))
	return handler, cfg.InteractionDir
}

func archiveFidelityExecutor() codexResponsesExecutorStub {
	return codexResponsesExecutorStub{complete: func(context.Context, codexresponses.Request) (codexresponses.Result, error) {
		now := time.Now()
		return codexresponses.Result{
			Body: []byte(`{"object":"response","id":"resp_fidelity","usage":{"input_tokens":7,"output_tokens":4}}`),
			Attempt: codexresponses.HTTPAttempt{
				Request: codexresponses.HTTPRequestObservation{
					At: now, Method: http.MethodPost, URL: "https://chatgpt.com/backend-api/codex/responses", BodyBytes: 128,
					Headers: []codexresponses.Header{
						{Name: "Authorization", Value: archiveUpstreamSecret},
						{Name: "ChatGPT-Account-ID", Value: "account-identity"},
						{Name: "X-Codex-Turn-State", Value: archiveTurnState},
					},
				},
				Response: codexresponses.HTTPResponseObservation{
					Observed: true, At: now, Status: http.StatusOK, ContentLength: -1, DurationMS: 12,
					Headers: []codexresponses.Header{
						{Name: "Content-Type", Value: "text/event-stream"},
						{Name: "X-Codex-Turn-State", Value: archiveTurnState},
					},
				},
			},
		}, nil
	}}
}

func serveArchivedCodexRequest(t *testing.T, handler *Handler) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.2-codex","input":"hello"}`))
	request.Header.Set("Authorization", "Bearer "+archiveClientSecret)
	request.Header.Set("Cookie", "aetherrelay_session="+archiveCookieSecret)
	request.Header.Set("X-Codex-Turn-State", archiveTurnState)
	request.Header.Set("Session-Id", archiveSessionSecret)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

// CP-OBS-009: 开关开启后，客户端请求、上游请求、上游响应、客户端响应四个方向的
// header 全部按原值落盘，而运行日志仍必须是脱敏投影。
func TestArchiveUnredactedHeadersKeepsFourDirectionsVerbatim(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	handler, interactionDir := newArchiveFidelityHandler(t, true, archiveFidelityExecutor())
	serveArchivedCodexRequest(t, handler)

	round := filepath.Join(interactionDir, "test-client", "000001")
	for name, values := range map[string][]string{
		"request.meta.json":      {archiveClientSecret, archiveCookieSecret, archiveTurnState, archiveSessionSecret},
		"upstream_request.json":  {archiveUpstreamSecret, "account-identity", archiveTurnState},
		"upstream_response.json": {archiveTurnState},
		"response.meta.json":     nil,
	} {
		body, err := os.ReadFile(filepath.Join(round, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(body), "<redacted>") {
			t.Fatalf("CP-OBS-009 %s still redacted: %s", name, body)
		}
		for _, value := range values {
			if !strings.Contains(string(body), value) {
				t.Fatalf("CP-OBS-009 %s lost %q: %s", name, value, body)
			}
		}
	}

	logged := logs.String()
	if logged == "" {
		t.Fatal("expected verbose archive logging to produce records")
	}
	for _, secret := range []string{archiveClientSecret, archiveUpstreamSecret, archiveTurnState} {
		if strings.Contains(logged, secret) {
			t.Fatalf("CP-OBS-009 credential %q leaked into logs: %s", secret, logged)
		}
	}
	if !strings.Contains(logged, "<redacted>") {
		t.Fatalf("expected the log projection to stay redacted: %s", logged)
	}
}

// CP-OBS-009: 默认取值下四类信息仍走同一脱敏名单。
func TestArchiveHeadersStayRedactedByDefault(t *testing.T) {
	handler, interactionDir := newArchiveFidelityHandler(t, false, archiveFidelityExecutor())
	serveArchivedCodexRequest(t, handler)

	round := filepath.Join(interactionDir, "test-client", "000001")
	for _, name := range []string{"request.meta.json", "upstream_request.json", "upstream_response.json", "response.meta.json"} {
		body, err := os.ReadFile(filepath.Join(round, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, secret := range []string{archiveClientSecret, archiveUpstreamSecret, archiveTurnState} {
			if strings.Contains(string(body), secret) {
				t.Fatalf("CP-OBS-009 %s leaked %q: %s", name, secret, body)
			}
		}
	}
	assertFileContains(t, filepath.Join(round, "request.meta.json"), `"<redacted>"`)
	assertFileContains(t, filepath.Join(round, "upstream_request.json"), `"<redacted>"`)
}
