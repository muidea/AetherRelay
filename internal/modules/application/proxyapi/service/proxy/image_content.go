package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptimage"
	"aetherrelay/internal/pkg/aetherrelayclientauth"
	"aetherrelay/internal/pkg/chatgptimageoutput"
)

const (
	imageURLTTL                   = time.Hour
	imageURLClockSkew             = 30 * time.Second
	imageFormatFallbackRequestKey = "X-AetherRelay-Allow-Image-Format-Fallback"
	imageFormatFallbackResultKey  = "X-AetherRelay-Image-Response-Format"
)

var errImageCredentialUnavailable = errors.New("client credential is unavailable")

func allowsInlineImageFallback(header http.Header) bool {
	for _, value := range strings.Split(header.Get(imageFormatFallbackRequestKey), ",") {
		if strings.EqualFold(strings.TrimSpace(value), "b64_json") {
			return true
		}
	}
	return false
}

func isImageContentPath(value string) bool {
	return strings.HasPrefix(value, "/images/")
}

// signImageResultURLs replaces local image-store URLs with short-lived signed
// URLs. Native upstream URLs remain under their provider's lifecycle contract.
func (h *Handler) signImageResultURLs(result *chatgptimage.Result, apiKeyID, baseURL string, now time.Time) error {
	if result == nil {
		return nil
	}
	signedURLs := make(map[int]string, len(result.Data))
	for index := range result.Data {
		raw := strings.TrimSpace(result.Data[index].URL)
		if raw == "" {
			continue
		}
		signed, local, err := h.signLocalImageURL(raw, apiKeyID, baseURL, now)
		if err != nil {
			return fmt.Errorf("sign image URL: %w", err)
		}
		if local {
			signedURLs[index] = signed
		}
	}
	for index, signed := range signedURLs {
		result.Data[index].URL = signed
	}
	return nil
}

func (h *Handler) signLocalImageURL(raw, apiKeyID, baseURL string, now time.Time) (string, bool, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false, err
	}
	if !isImageContentPath(parsed.Path) {
		return raw, false, nil
	}
	if parsed.IsAbs() || parsed.Host != "" {
		expected, expectedErr := url.Parse(baseURL)
		if expectedErr != nil || parsed.User != nil || expected.Scheme == "" || expected.Host == "" || !strings.EqualFold(parsed.Scheme, expected.Scheme) || !strings.EqualFold(parsed.Host, expected.Host) {
			return raw, false, nil
		}
	}
	if parsed.RawPath != "" || parsed.EscapedPath() != parsed.Path {
		return "", true, fmt.Errorf("encoded image path is not allowed")
	}
	scope, _, ok := imageRelativePath(parsed.Path)
	if !ok || scope != strings.TrimSpace(apiKeyID) {
		return "", true, fmt.Errorf("invalid image path")
	}
	if now.IsZero() {
		now = time.Now()
	}
	expiresAt := now.Add(imageURLTTL).Unix()
	signingKey := h.imageSigningKey()
	if len(signingKey) != 32 {
		return "", true, fmt.Errorf("image signing key is unavailable")
	}
	signature, ok := clientauth.SignResourceURL(h.clientKeyIndex.Load(), signingKey, apiKeyID, parsed.EscapedPath(), expiresAt)
	if !ok {
		return "", true, errImageCredentialUnavailable
	}
	// Stored image URLs do not carry application parameters. Rebuild the query
	// from scratch so stale or injected values cannot make the signed URL fail
	// the reader's exact-query contract.
	query := make(url.Values, 3)
	query.Set("key_id", strings.TrimSpace(apiKeyID))
	query.Set("expires", strconv.FormatInt(expiresAt, 10))
	query.Set("signature", signature)
	parsed.RawQuery = query.Encode()
	return parsed.String(), true, nil
}

func (h *Handler) handleImageContent(w http.ResponseWriter, r *http.Request) {
	setImageContentHeaders(w.Header())
	pathScope, relativePath, ok := imageRelativePath(r.URL.Path)
	if !ok || r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path {
		http.NotFound(w, r)
		return
	}
	query := r.URL.Query()
	if !exactImageURLQuery(query) {
		http.NotFound(w, r)
		return
	}
	apiKeyID := strings.TrimSpace(query.Get("key_id"))
	if pathScope != apiKeyID {
		http.NotFound(w, r)
		return
	}
	expiresAt, err := strconv.ParseInt(query.Get("expires"), 10, 64)
	now := time.Now()
	if err != nil || expiresAt > now.Add(imageURLTTL+imageURLClockSkew).Unix() || !clientauth.VerifyResourceURL(h.clientKeyIndex.Load(), h.imageSigningKey(), apiKeyID, r.URL.EscapedPath(), expiresAt, query.Get("signature"), now.Add(-imageURLClockSkew)) {
		http.NotFound(w, r)
		return
	}

	h.cfgMu.RLock()
	reader := h.chatGPTImageContent
	h.cfgMu.RUnlock()
	if reader == nil {
		http.Error(w, "image service unavailable", http.StatusServiceUnavailable)
		return
	}
	content, err := reader.OpenImage(r.Context(), apiKeyID, relativePath)
	if errors.Is(err, chatgptimage.ErrContentNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil || content.Reader == nil {
		http.Error(w, "image service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer content.Reader.Close()
	info, err := chatgptimageoutput.DecodeRasterInfoReader(content.Reader)
	if err != nil {
		http.Error(w, "image service unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, err := content.Reader.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "image service unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", imageContentType(info.Format))
	name := strings.TrimSpace(content.Name)
	if name == "" {
		name = path.Base(relativePath)
	}
	http.ServeContent(imageContentResponseWriter{ResponseWriter: w}, r, name, time.Time{}, content.Reader)
}

// inlineLocalImageResult preserves an already completed generation when its
// client credential is disabled or revoked before URL signing. No delegated
// URL is issued after revocation; the completed image is returned inline.
func (h *Handler) inlineLocalImageResult(ctx context.Context, result *chatgptimage.Result, apiKeyID, baseURL string) error {
	if result == nil {
		return nil
	}
	h.cfgMu.RLock()
	reader := h.chatGPTImageContent
	limit := h.cfg.MaxUpstreamResponseBytes
	h.cfgMu.RUnlock()
	if reader == nil {
		return fmt.Errorf("image content reader is unavailable")
	}
	if limit <= 0 {
		return fmt.Errorf("image content size limit is unavailable")
	}
	type candidate struct {
		index        int
		relativePath string
	}
	candidates := make([]candidate, 0, len(result.Data))
	for index := range result.Data {
		raw := strings.TrimSpace(result.Data[index].URL)
		if raw == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil || !isImageContentPath(parsed.Path) {
			continue
		}
		if parsed.IsAbs() || parsed.Host != "" {
			expected, expectedErr := url.Parse(baseURL)
			if expectedErr != nil || parsed.User != nil || expected.Scheme == "" || expected.Host == "" || !strings.EqualFold(parsed.Scheme, expected.Scheme) || !strings.EqualFold(parsed.Host, expected.Host) {
				continue
			}
		}
		if parsed.RawPath != "" || parsed.EscapedPath() != parsed.Path {
			return fmt.Errorf("encoded image path is not allowed")
		}
		scope, relativePath, ok := imageRelativePath(parsed.Path)
		if !ok || scope != strings.TrimSpace(apiKeyID) {
			return fmt.Errorf("invalid image path")
		}
		candidates = append(candidates, candidate{index: index, relativePath: relativePath})
	}
	if len(candidates) == 0 {
		return nil
	}

	// Account for the complete JSON envelope before reading image bytes. A
	// one-character placeholder preserves each b64_json field's exact structural
	// overhead; Base64 itself never needs JSON escaping.
	budgetResult := *result
	budgetResult.Data = append([]chatgptimage.Data(nil), result.Data...)
	for _, item := range candidates {
		budgetResult.Data[item.index].URL = ""
		budgetResult.Data[item.index].B64JSON = "x"
	}
	basePayload, err := json.Marshal(budgetResult)
	if err != nil {
		return fmt.Errorf("size completed image response: %w", err)
	}
	fixedBytes := int64(len(basePayload)-len(candidates)) + 1 // Encoder appends a newline.
	remaining := limit - fixedBytes
	if remaining < 0 {
		return fmt.Errorf("completed image response exceeds response size limit")
	}
	for _, item := range candidates {
		// Every four encoded bytes carry at most three raw bytes. Sharing this
		// budget across all candidates bounds both the response and retained
		// Base64 strings for the whole request.
		maxRaw := (remaining / 4) * 3
		content, err := reader.OpenImage(ctx, apiKeyID, item.relativePath)
		if err != nil {
			return fmt.Errorf("open completed image: %w", err)
		}
		if content.Reader == nil {
			return fmt.Errorf("open completed image: empty reader")
		}
		payload, readErr := io.ReadAll(io.LimitReader(content.Reader, maxRaw+1))
		_ = content.Reader.Close()
		if readErr != nil {
			return fmt.Errorf("read completed image: %w", readErr)
		}
		if int64(len(payload)) > maxRaw {
			return fmt.Errorf("completed image response exceeds response size limit")
		}
		encodedBytes := int64(base64.StdEncoding.EncodedLen(len(payload)))
		if encodedBytes > remaining {
			return fmt.Errorf("completed image response exceeds response size limit")
		}
		result.Data[item.index].URL = ""
		result.Data[item.index].B64JSON = base64.StdEncoding.EncodeToString(payload)
		remaining -= encodedBytes
	}
	return nil
}

type imageContentResponseWriter struct{ http.ResponseWriter }

func (w imageContentResponseWriter) WriteHeader(status int) {
	setImageContentHeaders(w.Header())
	w.ResponseWriter.WriteHeader(status)
}

func (w imageContentResponseWriter) Write(payload []byte) (int, error) {
	setImageContentHeaders(w.Header())
	return w.ResponseWriter.Write(payload)
}

func setImageContentHeaders(header http.Header) {
	header.Set("Cache-Control", "private, no-store")
	header.Set("Pragma", "no-cache")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
}

func (h *Handler) imageSigningKey() []byte {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	return append([]byte(nil), h.imageURLSigningKey...)
}

func exactImageURLQuery(query url.Values) bool {
	if len(query) != 3 {
		return false
	}
	for _, name := range []string{"key_id", "expires", "signature"} {
		values, ok := query[name]
		if !ok || len(values) != 1 || strings.TrimSpace(values[0]) == "" {
			return false
		}
	}
	return true
}

func imageRelativePath(value string) (string, string, bool) {
	if !isImageContentPath(value) {
		return "", "", false
	}
	rest := strings.TrimPrefix(value, "/images/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || !validImageScope(parts[0]) {
		return "", "", false
	}
	relativePath := parts[1]
	if relativePath == "" || strings.Contains(relativePath, "\\") || path.Clean("/"+relativePath) != "/"+relativePath {
		return "", "", false
	}
	return parts[0], relativePath, true
}

func validImageScope(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, ch := range value {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' {
			continue
		}
		return false
	}
	return true
}

func imageContentType(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "png":
		return "image/png"
	case "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	default:
		return "application/octet-stream"
	}
}
