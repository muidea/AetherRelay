// Package oauth tests account-pool OAuth flows.
package oauth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefreshPostsFormAndKeepsExistingRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("request=%s headers=%v", req.Method, req.Header)
		}
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), "grant_type=refresh_token") || !strings.Contains(string(body), "refresh_token=refresh-old") {
			t.Fatalf("body=%s", body)
		}
		_, _ = res.Write([]byte(`{"access_token":"access-new","id_token":"id-new"}`))
	}))
	defer server.Close()

	client := &Client{endpoint: server.URL}
	result, err := client.Refresh(context.Background(), Request{RefreshToken: "refresh-old"})
	if err != nil {
		t.Fatal(err)
	}
	if result.AccessToken != "access-new" || result.RefreshToken != "refresh-old" || result.IDToken != "id-new" {
		t.Fatalf("result=%+v", result)
	}
}

func TestRefreshRejectsNonSuccessWithoutBodyLeak(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(res http.ResponseWriter, _ *http.Request) {
		res.WriteHeader(http.StatusUnauthorized)
		_, _ = res.Write([]byte("sensitive upstream diagnostic"))
	}))
	defer server.Close()
	_, err := (&Client{endpoint: server.URL}).Refresh(context.Background(), Request{RefreshToken: "refresh"})
	if err == nil || err.Error() != "oauth refresh: HTTP 401" {
		t.Fatalf("err=%v", err)
	}
}
