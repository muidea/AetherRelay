package biz

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
)

func TestErrorArchivePolicy(t *testing.T) {
	for _, full := range []bool{false, true} {
		for _, unredacted := range []bool{false, true} {
			wire := `{"detail":"Invalid input role","request":{"secret":"private payload"}}`
			response := &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wire))}
			var attempt events.HTTPAttempt
			_, _, _, safe := readArchivedErrorObservation(response, &attempt, codexRequestProfile{archiveFullContent: full, archiveUnredacted: unredacted})
			if safe.Message != "Invalid input role" || attempt.Response.ErrorBodyFormat != "json" {
				t.Fatal("error detail lost")
			}
			if !full && len(attempt.Response.ErrorBody) != 0 {
				t.Fatal("body captured with archive disabled")
			}
			if full && unredacted && string(attempt.Response.ErrorBody) != wire {
				t.Fatal("raw archive changed")
			}
			if !unredacted && bytes.Contains(attempt.Response.ErrorBody, []byte("private payload")) {
				t.Fatal("redaction leaked unknown fields")
			}
		}
	}
}

func TestErrorArchiveMalformedAndTruncated(t *testing.T) {
	var failed events.HTTPAttempt
	readArchivedErrorObservation(&http.Response{Header: make(http.Header), Body: io.NopCloser(iotest.ErrReader(errors.New("read failed")))}, &failed, codexRequestProfile{})
	if !failed.Response.ErrorBodyReadFailed || failed.Response.ErrorBodyFormat != "empty" {
		t.Fatal("read failure hidden")
	}
	for _, wire := range []string{"", "<html>bad gateway</html>", strings.Repeat("x", maxArchivedErrorBytes+100)} {
		var attempt events.HTTPAttempt
		_, _, _, _ = readArchivedErrorObservation(&http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wire))}, &attempt, codexRequestProfile{archiveFullContent: true, archiveUnredacted: true})
		if attempt.Response.ErrorBodyTruncated != (len(wire) > maxArchivedErrorBytes) || len(attempt.Response.ErrorBody) > maxArchivedErrorBytes {
			t.Fatal("incorrect truncation")
		}
		if attempt.Response.ErrorBodyFormat == "json" {
			t.Fatal("malformed payload treated as JSON")
		}
	}
	if got := safeUpstreamError([]byte(`{"detail":"Authorization bearer secret"}`)); got.Message != "" {
		t.Fatal("unsafe message leaked")
	}
	if got := safeUpstreamError([]byte(`{"error":"Invalid role"}`)); got.Message != "Invalid role" {
		t.Fatal("string error lost")
	}
}
