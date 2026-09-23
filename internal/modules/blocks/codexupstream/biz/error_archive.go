package biz

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
)

const maxArchivedErrorBytes = 64 << 10

func readArchivedErrorObservation(response *http.Response, attempt *events.HTTPAttempt, profile codexRequestProfile) ([]byte, events.RateLimitObservation, int, events.SafeError) {
	if response == nil || response.Body == nil {
		return nil, events.RateLimitObservation{}, 0, events.SafeError{}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxArchivedErrorBytes+1))
	truncated := len(body) > maxArchivedErrorBytes
	if truncated {
		body = body[:maxArchivedErrorBytes]
	}
	_, rate, retry, safe := readErrorObservationParts(response.Header, bytes.NewReader(body))
	attempt.Response.ErrorBodyFormat = "non_json"
	if len(body) == 0 {
		attempt.Response.ErrorBodyFormat = "empty"
	} else if json.Valid(body) {
		attempt.Response.ErrorBodyFormat = "json"
	}
	attempt.Response.ErrorBodyTruncated = truncated
	attempt.Response.ErrorBodyReadFailed = err != nil
	if profile.archiveFullContent {
		if profile.archiveUnredacted {
			attempt.Response.ErrorBody = bytes.Clone(body)
		} else {
			// Unknown JSON shapes, HTML, and echoed request fields must not leak
			// through redacted archives. This is explicitly a safe projection.
			attempt.Response.ErrorBody, _ = json.Marshal(map[string]any{"redacted": true, "error": map[string]string{"type": safe.Type, "code": safe.Code, "param": safe.Param, "message": safe.Message}})
		}
	}
	return body, rate, retry, safe
}
