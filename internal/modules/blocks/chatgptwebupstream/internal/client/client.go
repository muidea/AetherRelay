// Package client implements the authenticated, browser-fingerprint-preserving
// ChatGPT Web transport owned by the upstream Block.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

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
	// newSearchClient is deliberately kept private to the transport owner. A
	// retried search_prepare must use a new browser TLS client rather than a
	// connection that may have been closed by the peer or account proxy.
	newSearchClient func() (*Client, error)
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
	client := newWithDoer(config, baseURL, doer)
	client.newSearchClient = func() (*Client, error) {
		return New(config)
	}
	return client, nil
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
	return c.BootstrapContext(context.Background())
}

func (c *Client) BootstrapContext(ctx context.Context) error {
	body, err := c.getContext(ctx, "/", "bootstrap")
	if err != nil {
		return err
	}
	c.scriptSources, c.dataBuild = parsePoWResources(string(body))
	return nil
}

// GetUserInfo mirrors the three authenticated Web endpoints used by Python.
// It is production transport code; live acceptance still requires a token.
func (c *Client) GetUserInfo() (UserInfo, error) {
	return c.GetUserInfoContext(context.Background())
}

func (c *Client) GetUserInfoContext(ctx context.Context) (UserInfo, error) {
	if err := c.BootstrapContext(ctx); err != nil {
		return UserInfo{}, err
	}
	meBody, err := c.getContext(ctx, "/backend-api/me", "get_me")
	if err != nil {
		return UserInfo{}, err
	}
	initBody, err := c.postJSONContext(ctx, "/backend-api/conversation/init", `{"gizmo_id":null,"requested_default_model":null,"conversation_id":null,"timezone_offset_min":-480}`, "conversation_init")
	if err != nil {
		return UserInfo{}, err
	}
	accountBody, err := c.getContext(ctx, "/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=-480", "account_check")
	if err != nil {
		return UserInfo{}, err
	}

	var me struct {
		Email string `json:"email"`
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
	if err := json.Unmarshal(accountBody, &accounts); err != nil {
		return UserInfo{}, fmt.Errorf("decode account_check: %w", err)
	}
	info := UserInfo{Email: me.Email, PlanType: accounts.Accounts.Default.Account.PlanType}
	quota, restoreAt, initPlan, err := parseConversationInit(initBody, time.Now().UTC())
	if err != nil {
		return UserInfo{}, err
	}
	info.Quota, info.RestoreAt = quota, restoreAt
	if info.PlanType == "" {
		info.PlanType = initPlan
	}
	if info.PlanType == "" {
		info.PlanType = "free"
	}
	return info, nil
}

func parseConversationInit(body []byte, observedAt time.Time) (int, string, string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return 0, "", "", fmt.Errorf("decode conversation_init: %w", err)
	}
	planType := stringValue(firstValue(root, "plan_type", "planType", "subscription_plan"))
	blocked, _ := firstValue(root, "blocked_features", "blockedFeatures").([]any)
	imageBlocked := false
	for _, feature := range blocked {
		if isImageQuotaFeature(stringValue(feature)) {
			imageBlocked = true
			break
		}
	}
	limits, _ := firstValue(root, "limits_progress", "limitsProgress").([]any)
	for _, raw := range limits {
		limit, ok := raw.(map[string]any)
		if !ok || !isImageQuotaFeature(stringValue(firstValue(limit, "feature_name", "featureName", "feature", "name"))) {
			continue
		}
		remaining, found := numericValue(firstValue(limit, "remaining", "remaining_value", "remainingValue", "remaining_count", "remainingCount"))
		if !found {
			total, totalOK := numericValue(firstValue(limit, "max_value", "maxValue", "cap", "total", "limit", "quota", "usage_limit", "usageLimit"))
			used, usedOK := numericValue(firstValue(limit, "used", "used_value", "usedValue", "consumed", "current_usage", "currentUsage"))
			if totalOK && usedOK {
				remaining, found = total-used, true
			}
		}
		if !found {
			if imageBlocked {
				return 0, normalizeReset(firstValue(limit, "reset_at", "resetAt", "next_reset_at", "nextResetAt", "reset_after", "resetAfter"), observedAt), planType, nil
			}
			return 0, "", planType, fmt.Errorf("conversation_init image quota is missing remaining capacity")
		}
		return normalizeQuota(remaining), normalizeReset(firstValue(limit, "reset_at", "resetAt", "next_reset_at", "nextResetAt", "reset_after", "resetAfter"), observedAt), planType, nil
	}
	if imageBlocked {
		return 0, "", planType, nil
	}
	return 0, "", planType, fmt.Errorf("conversation_init did not provide image quota")
}

func firstValue(object map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := object[key]; ok {
			return value
		}
	}
	return nil
}

func isImageQuotaFeature(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "image_gen", "image_generation", "image_edit", "img_gen":
		return true
	default:
		return false
	}
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

func numericValue(value any) (float64, bool) {
	text := stringValue(value)
	if text == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(text, 64)
	return parsed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
}

func normalizeQuota(value float64) int {
	if value <= 0 {
		return 0
	}
	if value >= float64(math.MaxInt) {
		return math.MaxInt
	}
	return int(math.Floor(value))
}

func normalizeReset(value any, observedAt time.Time) string {
	text := stringValue(value)
	if text == "" {
		return ""
	}
	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil || parsed <= 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return text
	}
	var reset time.Time
	switch {
	case parsed > 1_000_000_000_000:
		reset = time.UnixMilli(int64(parsed))
	case parsed > 1_000_000_000:
		reset = time.Unix(int64(parsed), 0)
	default:
		reset = observedAt.Add(time.Duration(parsed * float64(time.Second)))
	}
	return reset.UTC().Format(time.RFC3339)
}

func (c *Client) get(path, operation string) ([]byte, error) {
	return c.getContext(context.Background(), path, operation)
}
func (c *Client) getContext(ctx context.Context, path, operation string) ([]byte, error) {
	return c.requestContext(ctx, http.MethodGet, path, nil, operation)
}
func (c *Client) postJSON(path, payload, operation string) ([]byte, error) {
	return c.postJSONContext(context.Background(), path, payload, operation)
}
func (c *Client) postJSONContext(ctx context.Context, path, payload, operation string) ([]byte, error) {
	return c.requestContext(ctx, http.MethodPost, path, strings.NewReader(payload), operation)
}

func (c *Client) request(method, path string, body io.Reader, operation string) ([]byte, error) {
	return c.requestContext(context.Background(), method, path, body, operation)
}

func (c *Client) requestContext(ctx context.Context, method, path string, body io.Reader, operation string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("%s request: %w", operation, err)
	}
	req = req.WithContext(ctx)
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
