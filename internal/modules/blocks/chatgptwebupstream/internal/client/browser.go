package client

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // Keep the fixed IANA environment available in minimal containers.

	http "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

const (
	browserLanguage       = "zh-CN"
	browserLanguages      = "zh-CN,zh,en,en-US"
	browserAcceptLanguage = "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7"
	browserTimezone       = "Asia/Singapore"
	// JavaScript Date.getTimezoneOffset(): UTC minus local time, in minutes.
	browserTimezoneOffsetMinutes = -480
)

var browserLocation = func() *time.Location {
	location, err := time.LoadLocation(browserTimezone)
	if err != nil {
		panic(err)
	} // tzdata is embedded, so this is a build invariant.
	return location
}()

func validateDeviceID(value string) error {
	id, err := uuid.Parse(value)
	if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || id.String() != value {
		return fmt.Errorf("a canonical UUIDv4 device ID is required")
	}
	return nil
}

func browserDate(now time.Time) string {
	return now.In(browserLocation).Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Singapore Standard Time)"
}

// A new Jar models a direct navigation. API headers are applied only after
// navigation; Cookie handling remains exclusively owned by this client's Jar.
func (c *Client) setRequestHeaders(req *http.Request, path string, navigation bool) {
	req.Header.Set("user-agent", c.userAgent)
	req.Header.Set("accept-language", browserAcceptLanguage)
	req.Header.Set("sec-ch-ua", `"Google Chrome";v="133", "Chromium";v="133", "Not_A Brand";v="24"`)
	req.Header.Set("sec-ch-ua-arch", `"x86"`)
	req.Header.Set("sec-ch-ua-bitness", `"64"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	if navigation {
		req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
		req.Header.Set("cache-control", "max-age=0")
		req.Header.Set("priority", "u=0, i")
		req.Header.Set("sec-fetch-dest", "document")
		req.Header.Set("sec-fetch-mode", "navigate")
		req.Header.Set("sec-fetch-site", "none")
		req.Header.Set("sec-fetch-user", "?1")
		req.Header.Set("upgrade-insecure-requests", "1")
	} else {
		req.Header.Set("authorization", "Bearer "+c.accessToken)
		req.Header.Set("referer", c.baseURL+"/")
		// Same-origin GETs do not need an Origin header. Other methods retain it.
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			req.Header.Set("origin", c.baseURL)
		}
		req.Header.Set("accept", "*/*")
		req.Header.Set("cache-control", "no-cache")
		req.Header.Set("pragma", "no-cache")
		req.Header.Set("priority", "u=1, i")
		req.Header.Set("sec-fetch-dest", "empty")
		req.Header.Set("sec-fetch-mode", "cors")
		req.Header.Set("sec-fetch-site", "same-origin")
		req.Header.Set("oai-device-id", c.deviceID)
		req.Header.Set("oai-session-id", c.sessionID)
		req.Header.Set("oai-language", browserLanguage)
		req.Header.Set("x-openai-target-path", path)
		req.Header.Set("x-openai-target-route", strings.Split(path, "?")[0])
		if req.Method == http.MethodPost {
			req.Header.Set("content-type", "application/json")
		}
	}
	if c.cookie != "" {
		req.Header.Set("cookie", c.cookie)
	}
}
