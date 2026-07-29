package client

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

type fakeDoer struct{ requests []*http.Request }

func (d *fakeDoer) Do(request *http.Request) (*http.Response, error) {
	d.requests = append(d.requests, request)
	body := `{}`
	switch request.URL.Path {
	case "/":
		body = `<html></html>`
	case "/backend-api/me":
		body = `{"email":"person@example.com"}`
	case "/backend-api/conversation/init":
		body = `{"limits_progress":[{"feature_name":"image_gen","remaining":3,"reset_after":"tomorrow"}]}`
	case "/backend-api/accounts/check/v4-2023-04-27":
		body = `{"accounts":{"default":{"account":{"plan_type":"plus"}}}}`
	default:
		return nil, fmt.Errorf("unexpected path %s", request.URL.Path)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}, nil
}

func TestGetUserInfoUsesAuthenticatedWebSequence(t *testing.T) {
	doer := &fakeDoer{}
	client := newWithDoer(Config{AccessToken: "secret"}, "https://chatgpt.com", doer)
	info, err := client.GetUserInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Email != "person@example.com" || info.PlanType != "plus" || info.Quota != 3 || info.RestoreAt != "tomorrow" {
		t.Fatalf("info=%+v", info)
	}
	if len(doer.requests) != 4 {
		t.Fatalf("requests=%d", len(doer.requests))
	}
	bootstrap := doer.requests[0]
	if bootstrap.Header.Get("Sec-Fetch-Dest") != "document" || bootstrap.Header.Get("Sec-Fetch-Mode") != "navigate" || bootstrap.Header.Get("Oai-Device-Id") == "" || bootstrap.Header.Get("Oai-Session-Id") == "" || bootstrap.Header.Get("X-Openai-Target-Path") != "" {
		t.Fatalf("bootstrap headers=%v", bootstrap.Header)
	}
	request := doer.requests[1]
	if request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("Cookie") != "" || request.Header.Get("X-Openai-Target-Path") != "/backend-api/me" || request.Header.Get("Sec-Fetch-Mode") != "cors" {
		t.Fatalf("headers=%v", request.Header)
	}
}

func TestResolveProxyURLPrefersAccountProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://environment-proxy.invalid:8080")
	proxyURL, err := resolveProxyURL("http://account-proxy.invalid:8080")
	if err != nil || proxyURL != "http://account-proxy.invalid:8080" {
		t.Fatalf("proxy_url=%q err=%v", proxyURL, err)
	}
}

func TestResolveProxyURLFallsBackToEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://environment-proxy.invalid:8080")
	proxyURL, err := resolveProxyURL("")
	if err != nil || proxyURL != "http://environment-proxy.invalid:8080" {
		t.Fatalf("proxy_url=%q err=%v", proxyURL, err)
	}
}

func TestResolveProxyURLRejectsUnsupportedScheme(t *testing.T) {
	proxyURL, err := resolveProxyURL("socks5://127.0.0.1:1080")
	if err == nil || proxyURL != "" {
		t.Fatalf("proxy_url=%q err=%v", proxyURL, err)
	}
}

func TestClassifyStatus(t *testing.T) {
	for status, want := range map[int]ErrorClass{401: InvalidToken, 403: Upstream, 429: RateLimit, 500: Upstream} {
		got := classifyStatus("test", status).(*Error)
		if got.Class != want {
			t.Fatalf("status=%d class=%s", status, got.Class)
		}
	}
}

type requirementsDoer struct {
	finalizeBody string
	prepareBody  string
}

func (d *requirementsDoer) Do(request *http.Request) (*http.Response, error) {
	body := "{}"
	switch request.URL.Path {
	case "/":
		body = `<html data-build="c/build/_"><script src="https://cdn.example/c/build/_next.js"></script></html>`
	case "/backend-api/sentinel/chat-requirements/prepare":
		body = d.prepareBody
		if body == "" {
			body = `{"prepare_token":"prepare","proofofwork":{"required":true,"seed":"seed","difficulty":"ff"}}`
		}
	case "/backend-api/sentinel/chat-requirements/finalize":
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		d.finalizeBody = string(payload)
		body = `{"token":"requirements","so_token":"so"}`
	default:
		return nil, fmt.Errorf("unexpected path %s", request.URL.Path)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}, nil
}

func TestChatRequirementsPrepareAndFinalize(t *testing.T) {
	doer := &requirementsDoer{}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", doer)
	requirements, err := client.ChatRequirements()
	if err != nil {
		t.Fatal(err)
	}
	if requirements.Token != "requirements" || requirements.SOToken != "so" || requirements.ProofToken == "" {
		t.Fatalf("requirements=%+v", requirements)
	}
	if !strings.Contains(doer.finalizeBody, `"prepare_token":"prepare"`) || !strings.Contains(doer.finalizeBody, `"proof_token":"gAAAAAB`) {
		t.Fatalf("finalize=%s", doer.finalizeBody)
	}
}

func TestChatRequirementsContinuesWhenTurnstileProgramHasNoToken(t *testing.T) {
	doer := &requirementsDoer{prepareBody: `{"prepare_token":"prepare","turnstile":{"required":true,"dx":"not-base64"}}`}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", doer)
	// The test doer always returns a valid finalize result. A browser-only
	// Turnstile program must not prevent reaching that recoverable step.
	requirements, err := client.ChatRequirements()
	if err != nil {
		t.Fatal(err)
	}
	if requirements.Token != "requirements" || requirements.TurnstileToken != "" {
		t.Fatalf("requirements=%+v", requirements)
	}
}

type imageDoer struct {
	prepareRequest *http.Request
	streamRequest  *http.Request
	prepareBody    string
	streamBody     string
}

func (d *imageDoer) Do(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	switch request.URL.Path {
	case "/backend-api/f/conversation/prepare":
		d.prepareRequest, d.prepareBody = request, string(body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"conduit_token":"conduit"}`)), Header: make(http.Header)}, nil
	case "/backend-api/f/conversation":
		d.streamRequest, d.streamBody = request, string(body)
		stream := "event: message\n" +
			"data: {\"conversation_id\":\"conversation-1\",\n" +
			"data: \"asset_pointer\":\"file-service://file_123\"}\n\n" +
			"data: {\"metadata\":{\"attachment_id\":\"sediment_456\"},\"revised_prompt\":\"A better prompt\"}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(stream)), Header: make(http.Header)}, nil
	default:
		return nil, fmt.Errorf("unexpected path %s", request.URL.Path)
	}
}

func TestImageConversationPrepareAndSSE(t *testing.T) {
	doer := &imageDoer{}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", doer)
	request := ImageRequest{Prompt: "draw a mountain", Model: "gpt-image-2", Size: "1024x1024", Quality: "high"}
	requirements := Requirements{Token: "requirements", ProofToken: "proof"}
	conduit, err := client.PrepareImageConversation(request, requirements)
	if err != nil {
		t.Fatal(err)
	}
	if conduit != "conduit" || doer.prepareRequest.Header.Get("Openai-Sentinel-Chat-Requirements-Token") != "requirements" || doer.prepareRequest.Header.Get("Openai-Sentinel-Proof-Token") != "proof" || doer.prepareRequest.Header.Get("Oai-Device-Id") == "" || doer.prepareRequest.Header.Get("Sec-Fetch-Mode") != "cors" {
		t.Fatalf("conduit=%q headers=%v", conduit, doer.prepareRequest.Header)
	}
	if !strings.Contains(doer.prepareBody, `"model":"gpt-5-3"`) || !strings.Contains(doer.prepareBody, `"system_hints":["picture_v2"]`) || !strings.Contains(doer.prepareBody, "输出图片尺寸为 1024x1024。输出图片质量为 high。") {
		t.Fatalf("prepare payload=%s", doer.prepareBody)
	}
	result, err := client.StartImageConversation(request, requirements, conduit)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Done || result.ConversationID != "conversation-1" || len(result.FileIDs) != 1 || result.FileIDs[0] != "file_123" || len(result.SedimentIDs) != 1 || result.SedimentIDs[0] != "sediment_456" || result.RevisedPrompt != "A better prompt" {
		t.Fatalf("result=%+v", result)
	}
	if doer.streamRequest.Header.Get("X-Conduit-Token") != "conduit" || doer.streamRequest.Header.Get("X-Oai-Turn-Trace-Id") == "" || doer.streamRequest.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("stream headers=%v", doer.streamRequest.Header)
	}
	if !strings.Contains(doer.streamBody, `"client_prepare_state":"sent"`) || !strings.Contains(doer.streamBody, `"content_type":"text"`) || !strings.Contains(doer.streamBody, "输出图片尺寸为 1024x1024。输出图片质量为 high。") {
		t.Fatalf("stream payload=%s", doer.streamBody)
	}
}

func TestImagePromptPreservesOpenAISizeAndQualityIntent(t *testing.T) {
	if got := imagePrompt("draw", "1024x1024", "auto"); got != "draw\n\n输出图片尺寸为 1024x1024。输出图片质量为 auto。" {
		t.Fatalf("prompt=%q", got)
	}
	if got := imagePrompt(" draw ", "", ""); got != "draw" {
		t.Fatalf("prompt without hints=%q", got)
	}
}

func TestParseImageSSEIgnoresNonJSONFrames(t *testing.T) {
	result, err := ParseImageSSE(strings.NewReader("data: keepalive\n\ndata: {\"url\":\"https://cdn.example/image.png\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Done || len(result.URLs) != 1 || result.URLs[0] != "https://cdn.example/image.png" {
		t.Fatalf("result=%+v", result)
	}
}

type uploadDoer struct {
	requests []*http.Request
}

func (d *uploadDoer) Do(request *http.Request) (*http.Response, error) {
	d.requests = append(d.requests, request)
	switch {
	case request.URL.Path == "/backend-api/files":
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"file_id":"file_123","upload_url":"https://blob.example/upload"}`)), Header: make(http.Header)}, nil
	case request.URL.Host == "blob.example" && request.URL.Path == "/upload":
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	case request.URL.Path == "/backend-api/files/file_123/uploaded":
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	default:
		return nil, fmt.Errorf("unexpected path %s", request.URL.String())
	}
}

func TestUploadImageRunsFileServiceProtocol(t *testing.T) {
	imageBytes := bytes.NewBuffer(nil)
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.RGBA{R: 1, A: 255})
	if err := png.Encode(imageBytes, img); err != nil {
		t.Fatal(err)
	}
	doer := &uploadDoer{}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", doer)
	reference, err := client.UploadImage(imageBytes.Bytes(), "source.png")
	if err != nil {
		t.Fatal(err)
	}
	if reference.FileID != "file_123" || reference.FileName != "source.png" || reference.MIMEType != "image/png" || reference.Width != 2 || reference.Height != 3 || reference.FileSize != int64(imageBytes.Len()) {
		t.Fatalf("reference=%+v", reference)
	}
	if len(doer.requests) != 3 || doer.requests[1].Method != http.MethodPut || doer.requests[1].Header.Get("X-Ms-Blob-Type") != "BlockBlob" || doer.requests[2].Header.Get("X-Openai-Target-Path") != "/backend-api/files/file_123/uploaded" {
		t.Fatalf("requests=%v", doer.requests)
	}
}

func TestUploadImageRejectsUnsupportedContent(t *testing.T) {
	if _, err := inspectUploadImage([]byte("not an image"), "broken.bin"); err == nil {
		t.Fatal("inspectUploadImage succeeded for invalid image")
	}
}

type pollDownloadDoer struct {
	requests []*http.Request
}

func (d *pollDownloadDoer) Do(request *http.Request) (*http.Response, error) {
	d.requests = append(d.requests, request)
	switch {
	case request.URL.Path == "/backend-api/conversation/conversation-1":
		body := `{"mapping":{"user":{"message":{"author":{"role":"user"},"content":{"parts":[{"content_type":"image_asset_pointer","asset_pointer":"file-service://file-source"}]}}},"result":{"message":{"author":{"role":"assistant"},"metadata":{"async_task_type":"image_gen"},"content":{"parts":[{"content_type":"image_asset_pointer","asset_pointer":"file-service://file_result"}]}}}}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	case request.URL.Path == "/backend-api/files/file_result/download":
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"download_url":"https://cdn.example/result.png"}`)), Header: make(http.Header)}, nil
	case request.URL.Host == "cdn.example" && request.URL.Path == "/result.png":
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("image-bytes")), Header: make(http.Header)}, nil
	default:
		return nil, fmt.Errorf("unexpected path %s", request.URL.String())
	}
}

func TestPollAndDownloadImageResults(t *testing.T) {
	doer := &pollDownloadDoer{}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", doer)
	result, err := client.PollImageResults(context.Background(), "conversation-1", ImageStreamResult{}, ImagePollOptions{Timeout: time.Second, Interval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if result.ConversationID != "conversation-1" || len(result.FileIDs) != 1 || result.FileIDs[0] != "file_result" {
		t.Fatalf("poll result=%+v", result)
	}
	images, err := client.DownloadImageResults(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || string(images[0].Bytes) != "image-bytes" || images[0].URL != "https://cdn.example/result.png" {
		t.Fatalf("images=%+v", images)
	}
	var downloadRequest *http.Request
	for _, request := range doer.requests {
		if request.URL.Host == "cdn.example" {
			downloadRequest = request
			break
		}
	}
	if downloadRequest == nil || downloadRequest.Header.Get("Authorization") != "Bearer token" || downloadRequest.Header.Get("Oai-Device-Id") == "" || downloadRequest.Header.Get("Sec-Fetch-Mode") != "cors" {
		t.Fatalf("download headers=%v", downloadRequest)
	}
}

func TestDownloadImageResultsPrefersStableReferencesOverDirectURL(t *testing.T) {
	doer := &pollDownloadDoer{}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", doer)
	images, err := client.DownloadImageResults(ImageStreamResult{
		ConversationID: "conversation-1",
		FileIDs:        []string{"file_result"},
		URLs:           []string{"https://cdn.example/expired.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || string(images[0].Bytes) != "image-bytes" || images[0].URL != "https://cdn.example/result.png" {
		t.Fatalf("images=%+v", images)
	}
	for _, request := range doer.requests {
		if request.URL.Path == "/expired.png" {
			t.Fatalf("attempted incidental direct URL: %s", request.URL.String())
		}
	}
}

func TestResumeImagePollsExistingConversation(t *testing.T) {
	doer := &pollDownloadDoer{}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", doer)
	result, err := client.ResumeImage(context.Background(), "conversation-1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.ConversationID != "conversation-1" || len(result.Images) != 1 || string(result.Images[0].Bytes) != "image-bytes" {
		t.Fatalf("result=%+v", result)
	}
}

type generationDoer struct {
	paths []string
}

func (d *generationDoer) Do(request *http.Request) (*http.Response, error) {
	d.paths = append(d.paths, request.URL.Host+request.URL.Path)
	body := "{}"
	switch {
	case request.URL.Path == "/":
		body = "<html></html>"
	case request.URL.Path == "/backend-api/sentinel/chat-requirements/prepare":
		body = `{"prepare_token":"prepare"}`
	case request.URL.Path == "/backend-api/sentinel/chat-requirements/finalize":
		body = `{"token":"requirements"}`
	case request.URL.Path == "/backend-api/f/conversation/prepare":
		body = `{"conduit_token":"conduit"}`
	case request.URL.Path == "/backend-api/f/conversation":
		body = "data: {\"conversation_id\":\"conversation-1\"}\n\ndata: [DONE]\n\n"
	case request.URL.Path == "/backend-api/conversation/conversation-1":
		body = `{"mapping":{"node":{"message":{"author":{"role":"assistant"},"metadata":{"async_task_type":"image_gen"},"content":{"parts":[{"asset_pointer":"file-service://file_result"}]}}}}}`
	case request.URL.Path == "/backend-api/files/file_result/download":
		body = `{"url":"https://cdn.example/result.png"}`
	case request.URL.Host == "cdn.example" && request.URL.Path == "/result.png":
		body = "image-bytes"
	default:
		return nil, fmt.Errorf("unexpected path %s", request.URL.String())
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func TestGenerateImageRunsFullConversationLifecycle(t *testing.T) {
	doer := &generationDoer{}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", doer)
	result, err := client.GenerateImage(context.Background(), ImageRequest{Prompt: "draw a mountain", Model: "gpt-image-2"}, ImagePollOptions{Timeout: time.Second, Interval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if result.ConversationID != "conversation-1" || len(result.Images) != 1 || string(result.Images[0].Bytes) != "image-bytes" {
		t.Fatalf("result=%+v", result)
	}
	if len(doer.paths) != 8 {
		t.Fatalf("paths=%v", doer.paths)
	}
}
