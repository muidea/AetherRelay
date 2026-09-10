package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptimage"
	clientauth "aetherrelay/internal/pkg/aetherrelayclientauth"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

type chatGPTImageContentReaderStub struct {
	open func(context.Context, string, string) (chatgptimage.Content, error)
}

func (s chatGPTImageContentReaderStub) OpenImage(ctx context.Context, apiKeyID, relativePath string) (chatgptimage.Content, error) {
	if s.open != nil {
		return s.open(ctx, apiKeyID, relativePath)
	}
	return chatgptimage.Content{}, chatgptimage.ErrContentNotFound
}

type readSeekCloser struct{ *bytes.Reader }

func (readSeekCloser) Close() error { return nil }

type countingReadSeekCloser struct {
	*bytes.Reader
	bytesRead int
}

func (r *countingReadSeekCloser) Read(payload []byte) (int, error) {
	n, err := r.Reader.Read(payload)
	r.bytesRead += n
	return n, err
}

func (*countingReadSeekCloser) Close() error { return nil }

var testImageURLSigningKey = bytes.Repeat([]byte{0x5a}, 32)

func imageContent(payload []byte) chatgptimage.Content {
	return chatgptimage.Content{Reader: readSeekCloser{Reader: bytes.NewReader(payload)}, Name: "result.png"}
}

func signedImageHandler(h *Handler) *Handler {
	return h.WithImageURLSigningKey(testImageURLSigningKey)
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	payload, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLz4QAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestImageURLIsSignedAndServedFromClientScope(t *testing.T) {
	store := usage.NewMemoryStore()
	executor := chatGPTImageExecutorStub{generate: func(context.Context, chatgptimage.Request) (chatgptimage.Result, error) {
		return chatgptimage.Result{Created: 1, Data: []chatgptimage.Data{{URL: "http://relay.test/images/test-client/2026/09/10/result.png"}}}, nil
	}}
	var gotKeyID, gotPath string
	reader := chatGPTImageContentReaderStub{open: func(_ context.Context, apiKeyID, relativePath string) (chatgptimage.Content, error) {
		gotKeyID, gotPath = apiKeyID, relativePath
		return imageContent(testPNG(t)), nil
	}}
	h := signedImageHandler(newChatGPTImageHandler(t, store, executor)).WithChatGPTImageContentReader(reader)

	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"a dot","response_format":"url"}`))
	request.Host = "relay.test"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("generation status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("generation cache-control=%q", response.Header().Get("Cache-Control"))
	}
	var result chatgptimage.Result
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Data) != 1 {
		t.Fatalf("decode result=%+v err=%v", result, err)
	}
	imageURL, err := url.Parse(result.Data[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	if imageURL.Query().Get("key_id") != "test-client" || imageURL.Query().Get("expires") == "" || imageURL.Query().Get("signature") == "" {
		t.Fatalf("unsigned image URL=%s", result.Data[0].URL)
	}
	if strings.Contains(result.Data[0].URL, "test-client-key") {
		t.Fatal("image URL leaked raw client credential")
	}

	readRequest := httptest.NewRequest(http.MethodGet, imageURL.RequestURI(), nil)
	readResponse := httptest.NewRecorder()
	h.ServeHTTP(readResponse, readRequest)
	if readResponse.Code != http.StatusOK || readResponse.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("read status=%d content-type=%q body=%q", readResponse.Code, readResponse.Header().Get("Content-Type"), readResponse.Body.String())
	}
	if gotKeyID != "test-client" || gotPath != "2026/09/10/result.png" {
		t.Fatalf("scoped read key=%q path=%q", gotKeyID, gotPath)
	}
	if readResponse.Header().Get("Cache-Control") != "private, no-store" || readResponse.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("image response headers=%v", readResponse.Header())
	}
	if len(usageEvents(t, store)) != 1 {
		t.Fatal("image content reads must not create model usage events")
	}

	h.ActivateClientKeyIndex(nil)
	disabledResponse := httptest.NewRecorder()
	h.ServeHTTP(disabledResponse, httptest.NewRequest(http.MethodGet, imageURL.RequestURI(), nil))
	if disabledResponse.Code != http.StatusNotFound {
		t.Fatalf("disabled credential image status=%d", disabledResponse.Code)
	}
}

func TestImageURLSigningDropsExistingQueryAndLeavesExternalURLUnchanged(t *testing.T) {
	h := signedImageHandler(newChatGPTImageHandler(t, usage.NewMemoryStore(), chatGPTImageExecutorStub{}))
	signed, local, err := h.signLocalImageURL("http://relay.test/images/test-client/2026/09/10/result.png?stale=value", "test-client", "http://relay.test", time.Now())
	if err != nil || !local {
		t.Fatalf("sign local=%t err=%v", local, err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	if !exactImageURLQuery(parsed.Query()) || parsed.Query().Has("stale") {
		t.Fatalf("signed query=%v", parsed.Query())
	}

	external := "https://provider.test/images/provider/result.png?token=upstream"
	got, local, err := h.signLocalImageURL(external, "test-client", "http://relay.test", time.Now())
	if err != nil || local || got != external {
		t.Fatalf("external URL got=%q local=%t err=%v", got, local, err)
	}
}

func TestImageURLSigningRejectsMismatchedPathScope(t *testing.T) {
	h := signedImageHandler(newChatGPTImageHandler(t, usage.NewMemoryStore(), chatGPTImageExecutorStub{}))
	if _, local, err := h.signLocalImageURL("http://relay.test/images/other-client/2026/09/10/result.png", "test-client", "http://relay.test", time.Now()); err == nil || !local {
		t.Fatalf("mismatched scope local=%t err=%v", local, err)
	}

	path := "/images/other-client/2026/09/10/result.png"
	expiresAt := time.Now().Add(imageURLTTL).Unix()
	signature, ok := clientauth.SignResourceURL(h.clientKeyIndex.Load(), h.imageSigningKey(), "test-client", path, expiresAt)
	if !ok {
		t.Fatal("failed to construct mismatched signed URL")
	}
	called := false
	h.WithChatGPTImageContentReader(chatGPTImageContentReaderStub{open: func(context.Context, string, string) (chatgptimage.Content, error) {
		called = true
		return imageContent(testPNG(t)), nil
	}})
	requestURL := path + "?key_id=test-client&expires=" + strconv.FormatInt(expiresAt, 10) + "&signature=" + url.QueryEscape(signature)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, requestURL, nil))
	if response.Code != http.StatusNotFound || called {
		t.Fatalf("mismatched scope status=%d reader_called=%t", response.Code, called)
	}
}

func TestCompletedImageFallsBackToInlineWhenCredentialIsDisabledDuringGeneration(t *testing.T) {
	store := usage.NewMemoryStore()
	var h *Handler
	executor := chatGPTImageExecutorStub{generate: func(context.Context, chatgptimage.Request) (chatgptimage.Result, error) {
		h.ActivateClientKeyIndex(nil)
		return chatgptimage.Result{Created: 1, Data: []chatgptimage.Data{
			{URL: "http://relay.test/images/test-client/2026/09/10/result.png"},
			{URL: "https://provider.test/images/provider/result.png?token=upstream"},
		}}, nil
	}}
	h = signedImageHandler(newChatGPTImageHandler(t, store, executor)).WithChatGPTImageContentReader(chatGPTImageContentReaderStub{open: func(context.Context, string, string) (chatgptimage.Content, error) {
		return imageContent(testPNG(t)), nil
	}})
	h.cfgMu.Lock()
	h.cfg.MaxUpstreamResponseBytes = 1 << 20
	h.cfgMu.Unlock()
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"a dot","response_format":"url"}`))
	request.Host = "relay.test"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer test-client-key")
	request.Header.Set(imageFormatFallbackRequestKey, "b64_json")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("generation status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get(imageFormatFallbackResultKey); got != "b64_json" {
		t.Fatalf("fallback response format header=%q", got)
	}
	var result chatgptimage.Result
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Data) != 2 {
		t.Fatalf("decode result=%+v err=%v", result, err)
	}
	if result.Data[0].URL != "" || result.Data[0].B64JSON == "" {
		t.Fatalf("disabled credential result=%+v", result.Data[0])
	}
	if result.ResponseFormat != "b64_json" {
		t.Fatalf("fallback response format=%q", result.ResponseFormat)
	}
	if result.Data[1].URL != "https://provider.test/images/provider/result.png?token=upstream" || result.Data[1].B64JSON != "" {
		t.Fatalf("external provider result changed=%+v", result.Data[1])
	}
}

func TestCompletedImageURLFallbackRequiresExplicitOptIn(t *testing.T) {
	store := usage.NewMemoryStore()
	var h *Handler
	executor := chatGPTImageExecutorStub{generate: func(context.Context, chatgptimage.Request) (chatgptimage.Result, error) {
		h.ActivateClientKeyIndex(nil)
		return chatgptimage.Result{Created: 1, Data: []chatgptimage.Data{{URL: "http://relay.test/images/test-client/result.png"}}}, nil
	}}
	h = signedImageHandler(newChatGPTImageHandler(t, store, executor)).WithChatGPTImageContentReader(chatGPTImageContentReaderStub{open: func(context.Context, string, string) (chatgptimage.Content, error) {
		t.Fatal("image content must not be read without fallback opt-in")
		return chatgptimage.Content{}, nil
	}})
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"a dot","response_format":"url"}`))
	request.Host = "relay.test"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), ErrorCodeAuthenticationFailed) {
		t.Fatalf("generation status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get(imageFormatFallbackResultKey) != "" {
		t.Fatal("non-opt-in error advertised a fallback response")
	}
}

func TestInlineImageFallbackUsesAggregateResponseBudget(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 30)
	result := chatgptimage.Result{Created: 1, ResponseFormat: "b64_json", Data: []chatgptimage.Data{
		{URL: "http://relay.test/images/test-client/one.png"},
		{URL: "http://relay.test/images/test-client/two.png"},
	}}
	budgetShape := result
	budgetShape.Data = []chatgptimage.Data{{B64JSON: "x"}, {B64JSON: "x"}}
	shapeJSON, err := json.Marshal(budgetShape)
	if err != nil {
		t.Fatal(err)
	}
	fixedBytes := int64(len(shapeJSON)-2) + 1
	oneImageBytes := int64(base64.StdEncoding.EncodedLen(len(payload)))

	h := newChatGPTImageHandler(t, usage.NewMemoryStore(), chatGPTImageExecutorStub{}).WithChatGPTImageContentReader(chatGPTImageContentReaderStub{open: func(context.Context, string, string) (chatgptimage.Content, error) {
		return imageContent(payload), nil
	}})
	h.cfgMu.Lock()
	h.cfg.MaxUpstreamResponseBytes = fixedBytes + oneImageBytes
	h.cfgMu.Unlock()
	if err := h.inlineLocalImageResult(context.Background(), &result, "test-client", "http://relay.test"); err == nil {
		t.Fatal("aggregate image response unexpectedly fit a one-image budget")
	}
}

func TestClientImageDrainRejectsNewRequestsAndWaitsForInflight(t *testing.T) {
	h := newChatGPTImageHandler(t, usage.NewMemoryStore(), chatGPTImageExecutorStub{})
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer test-client-key")
	_, finish, err := h.resolveClientIdentity(headers, true)
	if err != nil {
		t.Fatal(err)
	}
	h.ActivateClientKeyIndex(nil)

	drained := make(chan error, 1)
	go func() { drained <- h.WaitClientRequests(context.Background(), "test-client") }()
	select {
	case err := <-drained:
		t.Fatalf("drain completed with request in flight: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if _, release, err := h.resolveClientIdentity(headers, true); err == nil {
		release()
		t.Fatal("request entered after client key index revocation")
	}
	finish()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("drain did not complete after request finished")
	}
}

func TestImageBaseURLUsesForwardedTLSProtocol(t *testing.T) {
	h := signedImageHandler(newChatGPTImageHandler(t, usage.NewMemoryStore(), chatGPTImageExecutorStub{}))
	request := httptest.NewRequest(http.MethodPost, "http://relay.test/v1/images/generations", nil)
	request.RemoteAddr = "192.0.2.10:43120"
	request.Header.Set("X-Forwarded-Proto", "https")
	if got := h.imageBaseURL(request); got != "http://relay.test" {
		t.Fatalf("untrusted proxy changed base URL: %q", got)
	}
	h.cfgMu.Lock()
	h.cfg.TrustedProxyCIDRs = []string{"192.0.2.0/24"}
	h.cfgMu.Unlock()
	if got := h.imageBaseURL(request); got != "https://relay.test" {
		t.Fatalf("base URL=%q", got)
	}
	request.Header.Set("X-Forwarded-Proto", "javascript")
	if got := h.imageBaseURL(request); got != "http://relay.test" {
		t.Fatalf("invalid forwarded protocol changed base URL: %q", got)
	}
}

func TestImageContentRejectsUnsignedTamperedAndExpiredURLs(t *testing.T) {
	h := signedImageHandler(newChatGPTImageHandler(t, usage.NewMemoryStore(), chatGPTImageExecutorStub{})).WithChatGPTImageContentReader(chatGPTImageContentReaderStub{open: func(context.Context, string, string) (chatgptimage.Content, error) {
		return imageContent(testPNG(t)), nil
	}})
	path := "/images/test-client/2026/09/10/result.png"

	unsigned := httptest.NewRecorder()
	h.ServeHTTP(unsigned, httptest.NewRequest(http.MethodGet, path, nil))
	if unsigned.Code != http.StatusNotFound {
		t.Fatalf("unsigned status=%d", unsigned.Code)
	}

	signed, _, err := h.signLocalImageURL("http://relay.test"+path, "test-client", "http://relay.test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(signed)
	tampered := *parsed
	tampered.Path += ".other"
	tamperedResponse := httptest.NewRecorder()
	h.ServeHTTP(tamperedResponse, httptest.NewRequest(http.MethodGet, tampered.RequestURI(), nil))
	if tamperedResponse.Code != http.StatusNotFound {
		t.Fatalf("tampered status=%d", tamperedResponse.Code)
	}

	expired, _, err := h.signLocalImageURL("http://relay.test"+path, "test-client", "http://relay.test", time.Now().Add(-2*imageURLTTL))
	if err != nil {
		t.Fatal(err)
	}
	expiredURL, _ := url.Parse(expired)
	expiredResponse := httptest.NewRecorder()
	h.ServeHTTP(expiredResponse, httptest.NewRequest(http.MethodGet, expiredURL.RequestURI(), nil))
	if expiredResponse.Code != http.StatusNotFound {
		t.Fatalf("expired status=%d", expiredResponse.Code)
	}

	clockSkew, _, err := h.signLocalImageURL("http://relay.test"+path, "test-client", "http://relay.test", time.Now().Add(-imageURLTTL-10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	clockSkewURL, _ := url.Parse(clockSkew)
	clockSkewResponse := httptest.NewRecorder()
	h.ServeHTTP(clockSkewResponse, httptest.NewRequest(http.MethodGet, clockSkewURL.RequestURI(), nil))
	if clockSkewResponse.Code != http.StatusOK {
		t.Fatalf("clock-skew status=%d", clockSkewResponse.Code)
	}

	tooLong, _, err := h.signLocalImageURL("http://relay.test"+path, "test-client", "http://relay.test", time.Now().Add(2*imageURLTTL))
	if err != nil {
		t.Fatal(err)
	}
	tooLongURL, _ := url.Parse(tooLong)
	tooLongResponse := httptest.NewRecorder()
	h.ServeHTTP(tooLongResponse, httptest.NewRequest(http.MethodGet, tooLongURL.RequestURI(), nil))
	if tooLongResponse.Code != http.StatusNotFound {
		t.Fatalf("overlong validity status=%d", tooLongResponse.Code)
	}
}

func TestImageContentSupportsHeadAndRange(t *testing.T) {
	payload := append(testPNG(t), make([]byte, 1<<20)...)
	var opened []*countingReadSeekCloser
	h := signedImageHandler(newChatGPTImageHandler(t, usage.NewMemoryStore(), chatGPTImageExecutorStub{})).WithChatGPTImageContentReader(chatGPTImageContentReaderStub{open: func(context.Context, string, string) (chatgptimage.Content, error) {
		reader := &countingReadSeekCloser{Reader: bytes.NewReader(payload)}
		opened = append(opened, reader)
		return chatgptimage.Content{Reader: reader, Name: "result.png"}, nil
	}})
	signed, _, err := h.signLocalImageURL("http://relay.test/images/test-client/2026/09/10/result.png", "test-client", "http://relay.test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	imageURL, _ := url.Parse(signed)

	headResponse := httptest.NewRecorder()
	h.ServeHTTP(headResponse, httptest.NewRequest(http.MethodHead, imageURL.RequestURI(), nil))
	if headResponse.Code != http.StatusOK || headResponse.Body.Len() != 0 {
		t.Fatalf("HEAD status=%d bytes=%d", headResponse.Code, headResponse.Body.Len())
	}

	rangeRequest := httptest.NewRequest(http.MethodGet, imageURL.RequestURI(), nil)
	rangeRequest.Header.Set("Range", "bytes=0-7")
	rangeResponse := httptest.NewRecorder()
	h.ServeHTTP(rangeResponse, rangeRequest)
	if rangeResponse.Code != http.StatusPartialContent || rangeResponse.Body.Len() != 8 {
		t.Fatalf("range status=%d bytes=%d", rangeResponse.Code, rangeResponse.Body.Len())
	}
	invalidRangeRequest := httptest.NewRequest(http.MethodGet, imageURL.RequestURI(), nil)
	invalidRangeRequest.Header.Set("Range", "bytes=invalid")
	invalidRangeResponse := httptest.NewRecorder()
	h.ServeHTTP(invalidRangeResponse, invalidRangeRequest)
	if invalidRangeResponse.Code != http.StatusRequestedRangeNotSatisfiable || invalidRangeResponse.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("invalid range status=%d cache-control=%q", invalidRangeResponse.Code, invalidRangeResponse.Header().Get("Cache-Control"))
	}

	if len(opened) != 3 {
		t.Fatalf("opened readers=%d", len(opened))
	}
	for index, reader := range opened {
		if reader.bytesRead >= len(payload)/2 {
			t.Fatalf("request %d read full image: read=%d size=%d", index, reader.bytesRead, len(payload))
		}
	}
}

func TestImageContentDistinguishesMissingAndStoreFailure(t *testing.T) {
	path := "http://relay.test/images/test-client/2026/09/10/result.png"
	for name, tc := range map[string]struct {
		openErr error
		status  int
	}{
		"missing": {openErr: chatgptimage.ErrContentNotFound, status: http.StatusNotFound},
		"failure": {openErr: errors.New("disk unavailable"), status: http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			h := signedImageHandler(newChatGPTImageHandler(t, usage.NewMemoryStore(), chatGPTImageExecutorStub{})).WithChatGPTImageContentReader(chatGPTImageContentReaderStub{open: func(context.Context, string, string) (chatgptimage.Content, error) {
				return chatgptimage.Content{}, tc.openErr
			}})
			signed, _, err := h.signLocalImageURL(path, "test-client", "http://relay.test", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			parsed, _ := url.Parse(signed)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, parsed.RequestURI(), nil))
			if response.Code != tc.status {
				t.Fatalf("status=%d want=%d", response.Code, tc.status)
			}
			if response.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatalf("cache control=%q", response.Header().Get("Cache-Control"))
			}
		})
	}
}

// A non-image request must remain in the deletion barrier through its final
// client write. Otherwise DELETE could remove usage/archive state too early.
func TestClientDrainIncludesNonImageHTTPRequests(t *testing.T) {
	h := newChatGPTImageHandler(t, usage.NewMemoryStore(), chatGPTImageExecutorStub{})
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-client-key")
	writer := &blockedClientWriter{ResponseRecorder: httptest.NewRecorder(), entered: entered, release: release}
	go func() { h.ServeHTTP(writer, req); close(finished) }()
	defer func() { close(release); <-finished }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not reach response write")
	}
	h.ActivateClientKeyIndex(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := h.WaitClientRequests(ctx, "test-client"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("non-image request escaped drain: %v", err)
	}
}

type blockedClientWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
}

func (w *blockedClientWriter) Write(p []byte) (int, error) {
	if w.entered != nil {
		close(w.entered)
		w.entered = nil
	}
	<-w.release
	return w.ResponseRecorder.Write(p)
}
