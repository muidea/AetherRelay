// Package oauth owns the narrow refresh-token exchange used by the account
// Block. It has no account persistence or EventHub dependency.
// Package oauth implements account-pool OAuth flows.
package oauth

import (
	"bytes"
	"context"
	"encoding/json"
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
	response, err := newHTTPClient().Do(httpRequest)
	if err != nil {
		return Result{}, fmt.Errorf("oauth refresh request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Result{}, fmt.Errorf("read oauth refresh response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("oauth refresh: HTTP %d", response.StatusCode)
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Result{}, fmt.Errorf("decode oauth refresh response: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return Result{}, fmt.Errorf("oauth refresh response missing access_token")
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
	response, err := newHTTPClient().Do(httpRequest)
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

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}
