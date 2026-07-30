// Package oauth implements the narrow, official Codex OAuth token contract.
package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	AuthorizeURL = "https://auth.openai.com/oauth/authorize"
	TokenURL     = "https://auth.openai.com/oauth/token"
	ClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	RedirectURI  = "http://localhost:1455/auth/callback"
)

type Request struct {
	RefreshToken string
	Proxy        string
}

type AuthorizationCodeRequest struct {
	Code         string
	CodeVerifier string
	Proxy        string
}

type Result struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	AccountID    string
	Email        string
	Expired      string
}

type Error struct {
	Permanent bool
	Class     string
	Cause     error
}

func (e *Error) Error() string { return e.Cause.Error() }
func (e *Error) Unwrap() error { return e.Cause }

func Refresh(ctx context.Context, request Request) (Result, error) {
	form := url.Values{"client_id": {ClientID}, "grant_type": {"refresh_token"}, "refresh_token": {strings.TrimSpace(request.RefreshToken)}, "scope": {"openid profile email"}}
	return exchange(ctx, form, request.Proxy)
}

func ExchangeAuthorizationCode(ctx context.Context, request AuthorizationCodeRequest) (Result, error) {
	form := url.Values{"client_id": {ClientID}, "grant_type": {"authorization_code"}, "code": {strings.TrimSpace(request.Code)}, "redirect_uri": {RedirectURI}, "code_verifier": {strings.TrimSpace(request.CodeVerifier)}}
	return exchange(ctx, form, request.Proxy)
}

func exchange(ctx context.Context, form url.Values, proxy string) (Result, error) {
	if strings.TrimSpace(form.Get("refresh_token")) == "" && strings.TrimSpace(form.Get("code")) == "" {
		return Result{}, &Error{Permanent: true, Class: "invalid_token", Cause: fmt.Errorf("OAuth credential is required")}
	}
	client, err := newHTTPClient(proxy)
	if err != nil {
		return Result{}, &Error{Permanent: true, Class: "invalid_request", Cause: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Result{}, &Error{Class: "upstream", Cause: fmt.Errorf("create OAuth request: %w", err)}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, &Error{Class: classifyTransport(err), Cause: fmt.Errorf("OAuth request failed: %w", err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{}, &Error{Class: "network", Cause: fmt.Errorf("read OAuth response: %w", err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		class, permanent := "upstream", false
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
			class, permanent = "invalid_token", true
		case http.StatusTooManyRequests:
			class = "rate_limit"
		case http.StatusRequestTimeout, http.StatusGatewayTimeout:
			class = "timeout"
		}
		return Result{}, &Error{Permanent: permanent, Class: class, Cause: fmt.Errorf("OAuth token endpoint returned HTTP %d", resp.StatusCode)}
	}
	var decoded struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return Result{}, &Error{Class: "upstream", Cause: fmt.Errorf("decode OAuth response: %w", err)}
	}
	if strings.TrimSpace(decoded.AccessToken) == "" {
		return Result{}, &Error{Class: "upstream", Cause: fmt.Errorf("OAuth response has no access token")}
	}
	accountID, email := claims(decoded.IDToken)
	result := Result{AccessToken: decoded.AccessToken, RefreshToken: decoded.RefreshToken, IDToken: decoded.IDToken, AccountID: accountID, Email: email}
	if decoded.ExpiresIn > 0 {
		result.Expired = time.Now().UTC().Add(time.Duration(decoded.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	return result, nil
}

func newHTTPClient(rawProxy string) (*http.Client, error) {
	transport, _ := http.DefaultTransport.(*http.Transport)
	cloned := transport.Clone()
	if rawProxy = strings.TrimSpace(rawProxy); rawProxy != "" {
		proxyURL, err := url.ParseRequestURI(rawProxy)
		if err != nil || proxyURL.Host == "" || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https") {
			return nil, fmt.Errorf("invalid account proxy URL")
		}
		cloned.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: cloned}, nil
}

func classifyTransport(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "timeout"
	}
	return "network"
}

func claims(idToken string) (string, string) {
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var decoded struct {
		Email string `json:"email"`
		Auth  struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &decoded) != nil {
		return "", ""
	}
	return strings.TrimSpace(decoded.Auth.AccountID), strings.TrimSpace(decoded.Email)
}

func CodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
