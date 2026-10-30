package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
)

func TestServiceShutdownOutcomeBeforeAndAfterOutput(t *testing.T) {
	failure := codexresponses.NewFailure(codexresponses.KindServiceShutdown, 0, nil)
	failure.UnavailableReason = "service_shutdown"
	retryable := false
	failure.Retryable = &retryable
	for _, convert := range []func(error) *streamFail{streamFailFromCodexError, streamFailFromCodexAfterOutput} {
		f := convert(failure)
		if string(f.Kind) != "service_shutdown" || f.CountUpstream || f.Retryable == nil || *f.Retryable || f.FailureClass != "service_shutdown" {
			t.Fatalf("wrong shutdown classification: %+v", f)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	(&Handler{}).writeCodexResponsesError(w, r, nil, time.Now(), "codexoauth", "model", true, failure)
	var body APIErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 503 || body.Error.Code != "service_shutdown" || body.Error.Retryable == nil || *body.Error.Retryable || body.Error.FailureClass != "service_shutdown" {
		t.Fatalf("HTTP %d body=%+v", w.Code, body)
	}
}
