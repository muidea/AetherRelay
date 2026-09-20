package biz

import (
	"net/http"
	"testing"
	"time"
)

func TestHTTPResponseObservationPreservesTransportFacts(t *testing.T) {
	for _, length := range []int64{-1, 0, 100} {
		got := observedHTTPResponse(200, length, []string{"chunked"}, http.Header{}, time.Now().Add(-time.Second), false)
		if !got.Observed || got.Status != 200 || got.ContentLength != length || got.TransferEncoding != "chunked" || len(got.Headers) != 0 || got.DurationMS < 1000 {
			t.Fatalf("observation=%+v", got)
		}
	}
}
