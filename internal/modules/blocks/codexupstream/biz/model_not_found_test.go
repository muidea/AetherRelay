package biz

import (
	"context"
	"io"
	"strings"
	"testing"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
)

func TestModelNotFoundClassification(t *testing.T) {
	// CP-FAIL-018: structured code only, not generic 404 or message matching.
	for _, tc := range []struct {
		status int
		body   string
		want   events.ErrorClass
	}{
		{404, `{"error":{"code":"model_not_found","type":"invalid_request_error"}}`, events.ErrorModelNotFound},
		{404, `{"error":{"code":"not_found","message":"model_not_found"}}`, events.ErrorInvalidRequest},
		{404, `<html>model_not_found</html>`, events.ErrorInvalidRequest},
		{400, `{"error":{"code":"model_not_found"}}`, events.ErrorInvalidRequest},
		{404, `{"error":{"code":42}}`, events.ErrorInvalidRequest},
	} {
		if got := errorClassWithBody(tc.status, []byte(tc.body), events.RateLimitObservation{}); got != tc.want {
			t.Fatalf("%d %s: %s", tc.status, tc.body, got)
		}
	}
	for _, payload := range []string{
		`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"model_not_found","message":"model unavailable"}}}`,
		`{"type":"error","error":{"code":"model_not_found"}}`,
	} {
		class, _, safe := codexTerminalFailure([]byte(payload))
		if class != events.ErrorModelNotFound || safe.Code != "model_not_found" {
			t.Fatalf("terminal = %s %+v", class, safe)
		}
		stream := &responseStream{updates: make(chan streamUpdate, 16)}
		(&Upstream{}).runStream(context.Background(), "test", stream, io.NopCloser(strings.NewReader("data: "+payload+"\n\n")), 1<<20)
		for update := range stream.updates {
			if len(update.data) != 0 {
				t.Fatalf("pre-output failure leaked: %s", update.data)
			}
			if !update.done || update.errorClass != events.ErrorModelNotFound || update.safeError.Code != "model_not_found" {
				t.Fatalf("typed stream = %+v", update)
			}
		}
	}
}
