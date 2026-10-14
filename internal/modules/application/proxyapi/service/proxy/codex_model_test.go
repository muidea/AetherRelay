package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/usagetarget"
	archive "aetherrelay/internal/pkg/aetherrelayarchive"
	codexmodel "aetherrelay/internal/pkg/aetherrelaycodexmodel"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

func TestCodexModelAndAccountSurviveConversionWithoutBodyArchival(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			observation := codexmodel.New("gpt-5.2-codex", "gpt-6-luna")
			attempt := codexArchiveTestAttempt()
			attempt.Response.Observation = observation
			executor := codexResponsesExecutorStub{complete: func(ctx context.Context, req codexresponses.Request) (codexresponses.Result, error) {
				usagetarget.Record(ctx, "codexoauth", "local-slot", "operator@example.com")
				usagetarget.SetCodexModels(ctx, observation)
				return codexresponses.Result{Body: []byte(`{"object":"response","id":"r","model":"gpt-6-luna","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":7,"output_tokens":4}}`), Attempt: attempt}, nil
			}}
			handler, dir := newArchivedCodexResponsesHandler(t, executor)
			body := `{"model":"gpt-5.2-codex","input":"hello"}`
			if path == "/v1/messages" {
				body = `{"model":"gpt-5.2-codex","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`
			}
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
			req.Header.Set("Authorization", "Bearer test-client-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			page, err := handler.usageStore.Events(context.Background(), usage.EventFilter{UsageFilter: usage.UsageFilter{AllTime: true}, PageSize: 10})
			if err != nil || len(page.Events) != 1 {
				t.Fatal(page, err)
			}
			if got := page.Events[0]; !got.ModelSubstituted || got.AccountLabel != "operator@example.com" || got.Outcome != "success" || got.ResponseModel != "gpt-6-luna" {
				t.Fatal(got)
			}
			data, err := os.ReadFile(filepath.Join(dir, "test-client", "000001", "metadata.json"))
			if err != nil {
				t.Fatal(err)
			}
			var meta archive.Metadata
			if err := json.Unmarshal(data, &meta); err != nil {
				t.Fatal(err)
			}
			if !meta.ModelSubstituted || meta.AccountID != "local-slot" || meta.AccountLabel != "operator@example.com" || meta.FullContentEnabled {
				t.Fatal(meta)
			}
			data, err = os.ReadFile(filepath.Join(dir, "test-client", "000001", "upstream_response_001.json"))
			if err != nil {
				t.Fatal(err)
			}
			var upstream upstreamResponseDebugInfo
			if err := json.Unmarshal(data, &upstream); err != nil || !upstream.ModelSubstituted || upstream.ResponseModel != "gpt-6-luna" {
				t.Fatal(string(data), err)
			}
		})
	}
}
