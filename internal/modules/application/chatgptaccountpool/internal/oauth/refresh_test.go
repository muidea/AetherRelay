// Package oauth tests account-pool OAuth flows.
package oauth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestRefreshClassifiesTransientAndCredentialFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		status    int
		body      string
		retryable bool
		class     string
	}{
		"rate_limit":    {status: http.StatusTooManyRequests, retryable: true, class: "rate_limit"},
		"invalid_grant": {status: http.StatusBadRequest, body: `{"error":"invalid_grant"}`, retryable: false, class: "invalid_grant"},
		"forbidden":     {status: http.StatusForbidden, retryable: false, class: "unauthorized"},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(res http.ResponseWriter, _ *http.Request) {
				res.WriteHeader(tc.status)
				_, _ = res.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, err := (&Client{endpoint: server.URL}).Refresh(context.Background(), Request{RefreshToken: "refresh"})
			if err == nil || IsRetryable(err) != tc.retryable || FailureClass(err) != tc.class {
				t.Fatalf("err=%v retryable=%v class=%s", err, IsRetryable(err), FailureClass(err))
			}
		})
	}
}

func TestNewHTTPClientUsesAccountProxy(t *testing.T) {
	client, err := newHTTPClient("http://account-proxy.invalid:8080")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatalf("transport=%T has_proxy=%t", client.Transport, ok && transport.Proxy != nil)
	}
	target, _ := url.Parse("https://auth.openai.com/oauth/token")
	proxyURL, err := transport.Proxy(&http.Request{URL: target})
	if err != nil || proxyURL.String() != "http://account-proxy.invalid:8080" {
		t.Fatalf("proxy=%v err=%v", proxyURL, err)
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
