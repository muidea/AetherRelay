// Package oauth owns the narrow refresh-token exchange used by the account
// Block. It has no account persistence or EventHub dependency.
// Package oauth implements account-pool OAuth flows.
package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTokenEndpoint       = "https://auth.openai.com/oauth/token"
	authorizationTokenEndpoint = "https://auth.openai.com/api/accounts/oauth/token"
	oauthClientID              = "app_2SKx67EdpoN0G6j64rFvigXD"
	oauthAuth0Client           = "eyJuYW1lIjoiYXV0aDAtc3BhLWpzIiwidmVyc2lvbiI6IjEuMjEuMCJ9"
	oauthUserAgent             = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
)

type Request struct {
	RefreshToken string
	Proxy        string
}

type Result struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
}

type AuthorizationCodeRequest struct {
	Code         string
	CodeVerifier string
	RedirectURI  string
}

type Client struct {
	endpoint string
}

// Error is a bounded OAuth refresh failure. Response bodies are intentionally
// excluded so callers can make state decisions without leaking credentials or
// upstream diagnostics.
type Error struct {
	Class      string
	StatusCode int
	Retryable  bool
	Cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("oauth refresh: HTTP %d", e.StatusCode)
	}
	if e.Cause != nil {
		return fmt.Sprintf("oauth refresh: %s: %v", e.Class, e.Cause)
	}
	return "oauth refresh: " + e.Class
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// IsRetryable reports whether a failed refresh should preserve the account for
// a later retry instead of treating the refresh credential as revoked.
func IsRetryable(err error) bool {
	var oauthErr *Error
	return errors.As(err, &oauthErr) && oauthErr.Retryable
}

// FailureClass returns a bounded string suitable for account state projection.
func FailureClass(err error) string {
	var oauthErr *Error
	if errors.As(err, &oauthErr) && oauthErr.Class != "" {
		return oauthErr.Class
	}
	return "unavailable"
}

func NewClient() *Client { return &Client{endpoint: defaultTokenEndpoint} }

func (c *Client) Refresh(ctx context.Context, request Request) (Result, error) {
	if strings.TrimSpace(request.RefreshToken) == "" {
		return Result{}, fmt.Errorf("oauth refresh token is required")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {request.RefreshToken},
		"client_id":     {oauthClientID},
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return Result{}, fmt.Errorf("create oauth refresh request: %w", err)
	}
	httpRequest.Header.Set("accept", "application/json")
	httpRequest.Header.Set("content-type", "application/x-www-form-urlencoded")
	httpRequest.Header.Set("user-agent", oauthUserAgent)
	httpClient, err := newHTTPClient(request.Proxy)
	if err != nil {
		return Result{}, err
	}
	response, err := httpClient.Do(httpRequest)
	if err != nil {
		return Result{}, &Error{Class: "transport", Retryable: true, Cause: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Result{}, &Error{Class: "transport", Retryable: true, Cause: err}
	}
	if response.StatusCode != http.StatusOK {
		return Result{}, refreshHTTPError(response.StatusCode, body)
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Result{}, &Error{Class: "decode", Retryable: true, Cause: err}
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return Result{}, &Error{Class: "missing_access_token", Retryable: true}
	}
	if strings.TrimSpace(payload.RefreshToken) == "" {
		payload.RefreshToken = request.RefreshToken
	}
	return Result{AccessToken: payload.AccessToken, RefreshToken: payload.RefreshToken, IDToken: payload.IDToken}, nil
}

// ExchangeAuthorizationCode performs the PKCE code exchange used by the
// operator-assisted OAuth bridge. Callers must keep the verifier private.
func (c *Client) ExchangeAuthorizationCode(ctx context.Context, request AuthorizationCodeRequest) (Result, error) {
	if strings.TrimSpace(request.Code) == "" || strings.TrimSpace(request.CodeVerifier) == "" {
		return Result{}, fmt.Errorf("oauth authorization code and verifier are required")
	}
	payload, err := json.Marshal(struct {
		ClientID     string `json:"client_id"`
		CodeVerifier string `json:"code_verifier"`
		GrantType    string `json:"grant_type"`
		Code         string `json:"code"`
		RedirectURI  string `json:"redirect_uri"`
	}{oauthClientID, request.CodeVerifier, "authorization_code", request.Code, request.RedirectURI})
	if err != nil {
		return Result{}, fmt.Errorf("encode oauth authorization exchange: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, authorizationTokenEndpoint, bytes.NewReader(payload))
	if err != nil {
		return Result{}, fmt.Errorf("create oauth authorization exchange: %w", err)
	}
	httpRequest.Header.Set("accept", "application/json")
	httpRequest.Header.Set("content-type", "application/json")
	httpRequest.Header.Set("origin", "https://auth.openai.com")
	httpRequest.Header.Set("referer", "https://platform.openai.com/")
	httpRequest.Header.Set("auth0-client", oauthAuth0Client)
	httpRequest.Header.Set("user-agent", oauthUserAgent)
	httpClient, err := newHTTPClient("")
	if err != nil {
		return Result{}, err
	}
	response, err := httpClient.Do(httpRequest)
	if err != nil {
		return Result{}, fmt.Errorf("oauth authorization exchange: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Result{}, fmt.Errorf("read oauth authorization response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("oauth authorization exchange: HTTP %d", response.StatusCode)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Result{}, fmt.Errorf("decode oauth authorization response: %w", err)
	}
	if strings.TrimSpace(out.AccessToken) == "" || strings.TrimSpace(out.RefreshToken) == "" {
		return Result{}, fmt.Errorf("oauth authorization response missing token")
	}
	return Result{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken, IDToken: out.IDToken}, nil
}

func refreshHTTPError(status int, body []byte) error {
	class := "rejected"
	retryable := status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && strings.EqualFold(strings.TrimSpace(payload.Error), "invalid_grant") {
		class = "invalid_grant"
		retryable = false
	} else if status == http.StatusUnauthorized || status == http.StatusForbidden {
		class = "unauthorized"
		retryable = false
	} else if status == http.StatusRequestTimeout {
		class = "timeout"
	} else if status == http.StatusTooManyRequests {
		class = "rate_limit"
	} else if status >= http.StatusInternalServerError {
		class = "upstream"
	}
	return &Error{Class: class, StatusCode: status, Retryable: retryable}
}

func newHTTPClient(accountProxy string) (*http.Client, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return client, nil
	}
	cloned := transport.Clone()
	if proxy := strings.TrimSpace(accountProxy); proxy != "" {
		parsed, err := url.ParseRequestURI(proxy)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("invalid ChatGPT OAuth proxy URL")
		}
		cloned.Proxy = http.ProxyURL(parsed)
	}
	client.Transport = cloned
	return client, nil
}
