// Package client implements the authenticated, browser-fingerprint-preserving
// ChatGPT Web transport owned by the upstream Block.
package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/google/uuid"
)

const (
	defaultBaseURL   = "https://chatgpt.com"
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
)

type Config struct {
	BaseURL     string
	AccessToken string
	Proxy       string
}

type UserInfo struct {
	Email     string
	PlanType  string
	Quota     int
	RestoreAt string
}

type doer interface {
	Do(*http.Request) (*http.Response, error)
}

type Client struct {
	doer          doer
	baseURL       string
	accessToken   string
	cookie        string
	userAgent     string
	deviceID      string
	sessionID     string
	scriptSources []string
	dataBuild     string
}

func New(config Config) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		return nil, fmt.Errorf("invalid upstream base URL: %w", err)
	}
	if strings.TrimSpace(config.AccessToken) == "" {
		return nil, fmt.Errorf("access token is required")
	}
	proxyURL, err := resolveProxyURL(config.Proxy)
	if err != nil {
		return nil, err
	}
	options := []tlsclient.HttpClientOption{
		// A Web image conversation may keep its SSE response open for several
		// minutes; shorter endpoint-specific reads are still bounded by callers.
		tlsclient.WithTimeoutSeconds(300),
		tlsclient.WithClientProfile(profiles.Chrome_133),
		tlsclient.WithRandomTLSExtensionOrder(),
		tlsclient.WithCookieJar(tlsclient.NewCookieJar()),
	}
	if proxyURL != "" {
		options = append(options, tlsclient.WithProxyUrl(proxyURL))
	}
	doer, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, fmt.Errorf("create TLS client: %w", err)
	}
	return newWithDoer(config, baseURL, doer), nil
}

// resolveProxyURL makes the account-owned proxy authoritative, then falls
// back to conventional process proxy variables. tls-client does not apply
// those variables automatically.
func resolveProxyURL(accountProxy string) (string, error) {
	proxyURL := strings.TrimSpace(accountProxy)
	if proxyURL == "" {
		for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
			if value := strings.TrimSpace(os.Getenv(name)); value != "" {
				proxyURL = value
				break
			}
		}
	}
	if proxyURL == "" {
		return "", nil
	}
	parsed, err := url.ParseRequestURI(proxyURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid ChatGPT Web proxy URL")
	}
	return proxyURL, nil
}

func newWithDoer(config Config, baseURL string, doer doer) *Client {
	return &Client{
		doer:        doer,
		baseURL:     strings.TrimRight(baseURL, "/"),
		accessToken: strings.TrimSpace(config.AccessToken),
		userAgent:   defaultUserAgent,
		deviceID:    uuid.NewString(),
		sessionID:   uuid.NewString(),
	}
}

// Bootstrap validates the browser TLS path, primes the cookie jar, and records
// the current page's PoW resource hints for chat-requirements.
func (c *Client) Bootstrap() error {
	body, err := c.get("/", "bootstrap")
	if err != nil {
		return err
	}
	c.scriptSources, c.dataBuild = parsePoWResources(string(body))
	return nil
}

// GetUserInfo mirrors the three authenticated Web endpoints used by Python.
// It is production transport code; live acceptance still requires a token.
func (c *Client) GetUserInfo() (UserInfo, error) {
	if err := c.Bootstrap(); err != nil {
		return UserInfo{}, err
	}
	meBody, err := c.get("/backend-api/me", "get_me")
	if err != nil {
		return UserInfo{}, err
	}
	initBody, err := c.postJSON("/backend-api/conversation/init", `{"gizmo_id":null,"requested_default_model":null,"conversation_id":null,"timezone_offset_min":-480}`, "conversation_init")
	if err != nil {
		return UserInfo{}, err
	}
	accountBody, err := c.get("/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=-480", "account_check")
	if err != nil {
		return UserInfo{}, err
	}

	var me struct {
		Email string `json:"email"`
	}
	var init struct {
		LimitsProgress []struct {
			FeatureName string `json:"feature_name"`
			Remaining   int    `json:"remaining"`
			ResetAfter  string `json:"reset_after"`
		} `json:"limits_progress"`
	}
	var accounts struct {
		Accounts struct {
			Default struct {
				Account struct {
					PlanType string `json:"plan_type"`
				} `json:"account"`
			} `json:"default"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(meBody, &me); err != nil {
		return UserInfo{}, fmt.Errorf("decode get_me: %w", err)
	}
	if err := json.Unmarshal(initBody, &init); err != nil {
		return UserInfo{}, fmt.Errorf("decode conversation_init: %w", err)
	}
	if err := json.Unmarshal(accountBody, &accounts); err != nil {
		return UserInfo{}, fmt.Errorf("decode account_check: %w", err)
	}
	info := UserInfo{Email: me.Email, PlanType: accounts.Accounts.Default.Account.PlanType}
	if info.PlanType == "" {
		info.PlanType = "free"
	}
	for _, limit := range init.LimitsProgress {
		if limit.FeatureName == "image_gen" {
			info.Quota, info.RestoreAt = limit.Remaining, limit.ResetAfter
			break
		}
	}
	return info, nil
}

func (c *Client) get(path, operation string) ([]byte, error) {
	return c.request(http.MethodGet, path, nil, operation)
}
func (c *Client) postJSON(path, payload, operation string) ([]byte, error) {
	return c.request(http.MethodPost, path, strings.NewReader(payload), operation)
}

func (c *Client) request(method, path string, body io.Reader, operation string) ([]byte, error) {
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("%s request: %w", operation, err)
	}
	req.Header.Set("authorization", "Bearer "+c.accessToken)
	req.Header.Set("origin", c.baseURL)
	req.Header.Set("referer", c.baseURL+"/")
	req.Header.Set("user-agent", c.userAgent)
	req.Header.Set("accept-language", "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7")
	req.Header.Set("cache-control", "no-cache")
	req.Header.Set("pragma", "no-cache")
	req.Header.Set("priority", "u=1, i")
	req.Header.Set("sec-ch-ua", `"Google Chrome";v="133", "Chromium";v="133", "Not_A Brand";v="24"`)
	req.Header.Set("sec-ch-ua-arch", `"x86"`)
	req.Header.Set("sec-ch-ua-bitness", `"64"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("oai-device-id", c.deviceID)
	req.Header.Set("oai-session-id", c.sessionID)
	req.Header.Set("oai-language", "zh-CN")
	if operation == "bootstrap" {
		req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
		req.Header.Set("sec-fetch-dest", "document")
		req.Header.Set("sec-fetch-mode", "navigate")
		req.Header.Set("sec-fetch-site", "none")
		req.Header.Set("sec-fetch-user", "?1")
		req.Header.Set("upgrade-insecure-requests", "1")
	} else {
		req.Header.Set("accept", "application/json, text/plain, */*")
		req.Header.Set("sec-fetch-dest", "empty")
		req.Header.Set("sec-fetch-mode", "cors")
		req.Header.Set("x-openai-target-path", path)
		req.Header.Set("x-openai-target-route", strings.Split(path, "?")[0])
	}
	if c.cookie != "" {
		req.Header.Set("cookie", c.cookie)
	}
	if method == http.MethodPost {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := c.doer.Do(req)
	if err != nil {
		return nil, classifyTransport(operation, err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, classifyTransport(operation, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classifyStatus(operation, resp.StatusCode)
	}
	return bodyBytes, nil
}
