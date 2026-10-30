package routeregistry

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPendingRequestsAreObservedWithoutWrappingResponseWriter(t *testing.T) {
	r := New()
	w := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses?private=query", nil)
	r.trackRequests(http.HandlerFunc(func(got http.ResponseWriter, req *http.Request) {
		if got != w {
			t.Error("response writer capabilities were changed")
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.requests) != 1 {
			t.Fatal("request not tracked")
		}
		for _, pending := range r.requests {
			if pending.Path != "/v1/responses" || pending.Method != "POST" || pending.StartedAt.IsZero() {
				t.Fatalf("bad pending request: %+v", pending)
			}
		}
	})).ServeHTTP(w, request)
	if len(r.requests) != 0 {
		t.Fatal("finished request leaked")
	}
}
