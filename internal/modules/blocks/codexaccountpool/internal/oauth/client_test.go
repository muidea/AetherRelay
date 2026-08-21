package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	codexidentity "aetherrelay/internal/pkg/aetherrelaycodexidentity"
)

// CP-HDR-021: credential requests share UA/originator and omit Version.
func TestRefreshUsesCurrentCodexJSONContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("request method=%q content-type=%q", r.Method, r.Header.Get("Content-Type"))
		}
		identity := codexidentity.Current()
		if r.Header.Get("User-Agent") != identity.UserAgent || r.Header.Get("Originator") != identity.Originator || r.Header.Get("Version") != "" {
			t.Fatalf("credential identity headers=%v", r.Header)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 3 || body["client_id"] != ClientID || body["grant_type"] != "refresh_token" || body["refresh_token"] != "refresh-secret" {
			t.Fatalf("request body=%#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer server.Close()

	result, err := refresh(context.Background(), Request{RefreshToken: " refresh-secret "}, server.URL)
	if err != nil || result.AccessToken != "access-new" || result.RefreshToken != "refresh-new" || result.Expired == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRefreshClassifiesOnlyKnownCredentialFailuresAsPermanent(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		class     string
		permanent bool
	}{
		{name: "expired", status: http.StatusBadRequest, body: `{"error":{"code":"refresh_token_expired"}}`, class: "refresh_token_expired", permanent: true},
		{name: "reused", status: http.StatusUnauthorized, body: `{"error":{"code":"refresh_token_reused"}}`, class: "refresh_token_reused", permanent: true},
		{name: "invalid request", status: http.StatusBadRequest, body: `{"error":"invalid_request"}`, class: "invalid_request", permanent: false},
		{name: "unknown bad request", status: http.StatusBadRequest, body: `{"error":"unexpected_backend_code"}`, class: "upstream", permanent: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			_, err := refresh(context.Background(), Request{RefreshToken: "refresh-secret"}, server.URL)
			oauthErr, ok := err.(*Error)
			if !ok || oauthErr.Class != test.class || oauthErr.Permanent != test.permanent {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}
