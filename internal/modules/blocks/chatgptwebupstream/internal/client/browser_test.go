package client

import (
	"context"
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testDeviceID = "95818a20-7d73-4b89-81f2-d6c769abe74c"

func TestBrowserEnvironmentUsesSingaporeTimeWithoutShiftingEpoch(t *testing.T) {
	instant := time.Date(2026, 10, 28, 20, 3, 4, 0, time.UTC)
	if got := browserDate(instant); got != "Thu Oct 29 2026 04:03:04 GMT+0800 (Singapore Standard Time)" {
		t.Fatalf("date=%s", got)
	}
	for _, month := range []time.Month{time.January, time.July} {
		local := time.Date(2026, month, 1, 0, 0, 0, 0, time.UTC).In(browserLocation)
		_, offset := local.Zone()
		if offset != 8*3600 || -offset/60 != browserTimezoneOffsetMinutes {
			t.Fatalf("offset=%d", offset)
		}
	}
	before := time.Now().UnixMilli()
	config := requirementsConfig(defaultUserAgent, nil, "build")
	after := time.Now().UnixMilli()
	if config[7] != browserLanguage || config[8] != browserLanguages || !strings.Contains(config[1].(string), "GMT+0800 (Singapore Standard Time)") {
		t.Fatalf("environment=%v", config)
	}
	for _, index := range []int{13, 17} {
		epoch := int64(config[index].(float64))
		if epoch < before || epoch > after {
			t.Fatalf("epoch at %d was shifted: %d", index, epoch)
		}
	}
}

func TestIndependentClientsKeepOnlyTheirOwnNavigationCookies(t *testing.T) {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, "")
	}
	requests := make(chan *stdhttp.Request, 12)
	server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		requests <- r.Clone(context.Background())
		if r.URL.Path == "/" {
			// Observe without injecting, overwriting, or adopting a server device cookie.
			stdhttp.SetCookie(w, &stdhttp.Cookie{Name: "oai-did", Value: "server-cookie", Path: "/"})
			_, _ = w.Write([]byte("<html></html>"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	first, err := New(Config{BaseURL: server.URL, AccessToken: "secret", DeviceID: testDeviceID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(Config{BaseURL: server.URL, AccessToken: "secret", DeviceID: testDeviceID})
	if err != nil {
		t.Fatal(err)
	}
	if first.sessionID == second.sessionID {
		t.Fatal("independent clients share a session")
	}
	if err := first.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	navigation := <-requests
	for _, key := range []string{"Authorization", "Origin", "Referer", "Oai-Device-Id", "Oai-Session-Id", "Oai-Language", "Cookie", "Pragma", "X-Openai-Target-Path"} {
		if navigation.Header.Get(key) != "" {
			t.Fatalf("navigation unexpectedly has %s", key)
		}
	}
	if navigation.Header.Get("Priority") != "u=0, i" || navigation.Header.Get("Sec-Fetch-Site") != "none" {
		t.Fatal("invalid direct navigation semantics")
	}
	if _, err := first.get("/backend-api/me", "me"); err != nil {
		t.Fatal(err)
	}
	api := <-requests
	if api.Header.Get("Cookie") != "oai-did=server-cookie" || api.Header.Get("Authorization") != "Bearer secret" || api.Header.Get("Oai-Device-Id") != testDeviceID || api.Header.Get("Origin") != "" || api.Header.Get("Accept") != "*/*" {
		t.Fatalf("API headers=%v", api.Header)
	}
	if _, err := first.postJSON("/backend-api/conversation/init", "{}", "conversation_init"); err != nil {
		t.Fatal(err)
	}
	post := <-requests
	if post.Header.Get("Origin") != server.URL || post.Header.Get("Content-Type") != "application/json" || post.Header.Get("Oai-Session-Id") != first.sessionID {
		t.Fatal("POST identity or origin changed")
	}
	if err := second.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if (<-requests).Header.Get("Cookie") != "" {
		t.Fatal("navigation reused another operation's cookies")
	}
	if first.deviceID != testDeviceID || second.deviceID != testDeviceID {
		t.Fatal("server cookie replaced persistent identity")
	}
}

func TestClientRequiresPersistentUUIDv4Identity(t *testing.T) {
	for _, value := range []string{"", "not-a-device", "00000000-0000-0000-0000-000000000000", "95818A20-7D73-4B89-81F2-D6C769ABE74C"} {
		if _, err := New(Config{AccessToken: "secret", DeviceID: value}); err == nil {
			t.Fatalf("accepted invalid identity %q", value)
		}
	}
}

func TestAccountRefreshUsesSingaporeOffsetInBothRequests(t *testing.T) {
	doer := &fakeDoer{}
	client := newWithDoer(Config{AccessToken: "secret", DeviceID: testDeviceID}, "https://chatgpt.com", doer)
	if _, err := client.GetUserInfo(); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Offset int `json:"timezone_offset_min"`
	}
	if err := json.NewDecoder(doer.requests[2].Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Offset != browserTimezoneOffsetMinutes || doer.requests[3].URL.Query().Get("timezone_offset_min") != "-480" {
		t.Fatal("inconsistent Singapore offsets")
	}
}

func assertSingaporePayload(t *testing.T, payload any) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var environment struct {
		Timezone string `json:"timezone"`
		Offset   int    `json:"timezone_offset_min"`
	}
	if err := json.Unmarshal(data, &environment); err != nil {
		t.Fatal(err)
	}
	if environment.Timezone != "Asia/Singapore" || environment.Offset != -480 {
		t.Fatalf("incorrect browser timezone: %s", data)
	}
}

func TestTextImageAndSearchPayloadsUseSingaporeEnvironment(t *testing.T) {
	assertSingaporePayload(t, textConversationPayload(TextRequest{Model: "auto"}, nil))
	assertSingaporePayload(t, imageConversationPayload(ImageRequest{Prompt: "draw"}))
	imageTransport := &imageDoer{}
	imageClient := newWithDoer(Config{AccessToken: "token", DeviceID: testDeviceID}, "https://chatgpt.com", imageTransport)
	if _, err := imageClient.PrepareImageConversation(ImageRequest{Prompt: "draw"}, Requirements{Token: "requirements"}); err != nil {
		t.Fatal(err)
	}
	var imagePrepare any
	if err := json.Unmarshal([]byte(imageTransport.prepareBody), &imagePrepare); err != nil {
		t.Fatal(err)
	}
	assertSingaporePayload(t, imagePrepare)
	searchTransport := &searchDoer{}
	searchClient := newWithDoer(Config{AccessToken: "token", DeviceID: testDeviceID}, "https://chatgpt.com", searchTransport)
	if _, err := searchClient.Search(context.Background(), SearchRequest{Model: "auto", Query: "test"}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{searchTransport.prepareBody, searchTransport.runBody} {
		var payload any
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatal(err)
		}
		assertSingaporePayload(t, payload)
	}
}
