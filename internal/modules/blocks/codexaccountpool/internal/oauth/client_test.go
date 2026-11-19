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

func TestRefreshUsesSharedClientIdentity(t *testing.T) {
	userAgent := "codex-tui/0.159.2 (Ubuntu 24.4.0; x86_64) gnome-terminal (codex-tui; 0.159.2)"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != userAgent || r.Header.Get("Originator") != "codex-tui" || r.Header.Get("Version") != "" {
			t.Fatalf("selected credential identity headers=%v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer server.Close()

	result, err := refresh(context.Background(), Request{RefreshToken: "refresh-secret", UserAgent: userAgent, Originator: "codex-tui"}, server.URL)
	if err != nil || result.AccessToken != "access-new" {
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
		{name: "standard invalid grant", status: http.StatusBadRequest, body: `{"error":"invalid_grant"}`, class: "invalid_grant", permanent: true},
		{name: "revoked", status: http.StatusBadRequest, body: `{"error":{"code":"refresh_token_revoked"}}`, class: "refresh_token_revoked", permanent: true},
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

// CP-HDR-021/025: code exchange uses the same selected maximum as refresh.
func TestAuthorizationCodeExchangeUsesSharedClientIdentity(t *testing.T) {
	ua := "codex-tui/0.162.1 (Ubuntu; x86_64) terminal (codex-tui; 0.162.1)"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != ua || r.Header.Get("Originator") != "codex-tui" || r.Header.Get("Version") != "" {
			t.Errorf("credential identity=%v", r.Header)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "test-code" || r.Form.Get("code_verifier") != "test-verifier" {
			t.Errorf("form=%v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
	}))
	defer server.Close()
	result, err := exchangeAuthorizationCode(context.Background(), AuthorizationCodeRequest{Code: "test-code", CodeVerifier: "test-verifier", UserAgent: ua, Originator: "codex-tui"}, server.URL)
	if err != nil || result.AccessToken != "new-access" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRefreshValidatesExpiresInAndMarksPreSendErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiresIn string
		wantExp   bool
		wantErr   bool
	}{
		{name: "integer", expiresIn: `3600`, wantExp: true},
		{name: "float zero fraction", expiresIn: `3600.0`, wantExp: true},
		{name: "scientific notation", expiresIn: `3.6e3`, wantExp: true},
		{name: "numeric string", expiresIn: `"3600"`, wantExp: true},
		{name: "null", expiresIn: `null`},
		{name: "fractional", expiresIn: `3600.5`, wantErr: true},
		{name: "overflow", expiresIn: `1e20`, wantErr: true},
		{name: "infinity string", expiresIn: `"Inf"`, wantErr: true},
		{name: "zero", expiresIn: `0`, wantErr: true},
		{name: "negative", expiresIn: `-10`, wantErr: true},
		{name: "object", expiresIn: `{}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"access-new","refresh_token":"refresh-new","expires_in":` + tc.expiresIn + `}`))
			}))
			defer server.Close()
			res, err := refresh(context.Background(), Request{RefreshToken: "refresh-secret"}, server.URL)
			if tc.wantErr {
				oauthErr, ok := err.(*Error)
				if err == nil || !ok || oauthErr.PreSend {
					t.Fatalf("expected post-send error, got res=%+v err=%#v", res, err)
				}
				return
			}
			if err != nil || (tc.wantExp && res.Expired == "") || (!tc.wantExp && res.Expired != "") {
				t.Fatalf("res=%+v err=%v", res, err)
			}
		})
	}

	_, err := refresh(context.Background(), Request{RefreshToken: "refresh-secret", Proxy: "://bad-proxy"}, "https://auth.openai.com/oauth/token")
	if oauthErr, ok := err.(*Error); !ok || !oauthErr.PreSend {
		t.Fatalf("expected pre-send error on invalid proxy, got %#v", err)
	}
	_, err = refresh(context.Background(), Request{RefreshToken: ""}, "https://auth.openai.com/oauth/token")
	if oauthErr, ok := err.(*Error); !ok || !oauthErr.PreSend {
		t.Fatalf("expected pre-send error on empty refresh token, got %#v", err)
	}
}
