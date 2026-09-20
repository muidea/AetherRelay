package biz

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestArchiveBodyIsFinalWireBodyAndOptIn(t *testing.T) {
	for _, capture := range []bool{false, true} {
		var wire []byte
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { wire, _ = io.ReadAll(r.Body); w.WriteHeader(200) }))
		profile := codexRequestProfile{sessionHash: "session", archiveFullContent: capture}
		resp, attempt, _, _, err := performURL(context.Background(), server.URL, "text/event-stream", "token", "account", "", []byte(`{"model":"test","input":[]}`), profile)
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		server.Close()
		if capture {
			if !bytes.Equal(wire, attempt.Request.Body) || !bytes.Contains(wire, []byte(`"client_metadata"`)) {
				t.Fatal("archive does not match identity-injected wire body")
			}
		} else if len(attempt.Request.Body) != 0 {
			t.Fatal("body captured without opt-in")
		}
		if attempt.Request.BodyBytes != len(wire) {
			t.Fatal("incorrect wire length")
		}
	}
}
