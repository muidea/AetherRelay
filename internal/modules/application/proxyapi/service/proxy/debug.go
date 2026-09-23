package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/pkg/aetherrelayarchive"
	"aetherrelay/internal/pkg/aetherrelayconfig"
)

type requestDebugInfo struct {
	RoundID       int                 `json:"round_id"`
	ReceivedAt    time.Time           `json:"received_at"`
	Method        string              `json:"method"`
	Path          string              `json:"path"`
	RawQuery      string              `json:"raw_query,omitempty"`
	RequestURI    string              `json:"request_uri"`
	Host          string              `json:"host"`
	RemoteAddr    string              `json:"remote_addr"`
	UserAgent     string              `json:"user_agent,omitempty"`
	ContentLength int64               `json:"content_length"`
	BodyBytes     int                 `json:"body_bytes"`
	Headers       map[string][]string `json:"headers"`
	BodyPath      string              `json:"body_path"`
}

type upstreamDebugInfo struct {
	Attempt   int                 `json:"attempt,omitempty"`
	BodyPath  string              `json:"body_path,omitempty"`
	RoundID   int                 `json:"round_id"`
	At        time.Time           `json:"at"`
	Provider  string              `json:"provider"`
	Protocol  string              `json:"protocol"`
	Method    string              `json:"method"`
	URL       string              `json:"url"`
	BodyBytes int                 `json:"body_bytes"`
	Headers   map[string][]string `json:"headers"`
}

type upstreamResponseDebugInfo struct {
	ErrorBodyPath        string              `json:"error_body_path,omitempty"`
	ErrorBodyFormat      string              `json:"error_body_format,omitempty"`
	ErrorBodyTruncated   bool                `json:"error_body_truncated,omitempty"`
	ErrorBodyReadFailed  bool                `json:"error_body_read_failed,omitempty"`
	UpstreamErrorType    string              `json:"upstream_error_type,omitempty"`
	UpstreamErrorParam   string              `json:"upstream_error_param,omitempty"`
	UpstreamErrorMessage string              `json:"upstream_error_message,omitempty"`
	TransferEncoding     string              `json:"transfer_encoding,omitempty"`
	FailureClass         string              `json:"failure_class,omitempty"`
	RetryAfterSeconds    int                 `json:"retry_after_seconds,omitempty"`
	UpstreamErrorCode    string              `json:"upstream_error_code,omitempty"`
	Attempt              int                 `json:"attempt,omitempty"`
	RoundID              int                 `json:"round_id"`
	At                   time.Time           `json:"at"`
	Provider             string              `json:"provider"`
	Protocol             string              `json:"protocol"`
	Status               int                 `json:"status"`
	DurationMS           int64               `json:"duration_ms"`
	ContentType          string              `json:"content_type,omitempty"`
	ContentLength        int64               `json:"content_length"`
	Headers              map[string][]string `json:"headers,omitempty"`
	Error                string              `json:"error,omitempty"`
}

func (h *Handler) debugfRound(round *archive.Round, r *http.Request, format string, args ...any) {
	if !h.cfg.VerboseLogging {
		return
	}
	requestID := requestIDFromContext(r.Context())
	if requestID == "" && round != nil {
		requestID = round.RequestID
	}
	attrs := []any{}
	if requestID != "" {
		attrs = append(attrs, slog.String("request_id", requestID))
	}
	if round != nil {
		attrs = append(attrs, slog.Int("round", round.ID))
	}
	attrs = append(attrs, slog.String("msg", fmt.Sprintf(format, args...)))
	slog.Debug("debug", attrs...)
}

func (h *Handler) archiveAndLogClientRequest(round *archive.Round, r *http.Request, bodyBytes int) {
	if round == nil {
		return
	}
	info := requestDebugInfo{
		RoundID:       round.ID,
		ReceivedAt:    round.StartedAt,
		Method:        r.Method,
		Path:          r.URL.Path,
		RawQuery:      r.URL.RawQuery,
		RequestURI:    r.RequestURI,
		Host:          r.Host,
		RemoteAddr:    r.RemoteAddr,
		UserAgent:     r.UserAgent(),
		ContentLength: r.ContentLength,
		BodyBytes:     bodyBytes,
		Headers:       h.archiveHeaders(r.Header),
	}
	if round.HasFile("request.json") {
		info.BodyPath = "request.json"
	}
	if err := round.WriteJSON("request.meta.json", info); err != nil {
		log.Printf("archive request metadata: %v", err)
	}
	h.debugfRound(round, r, "round=%06d client request method=%s path=%s query=%q remote=%s host=%s user_agent=%q body_bytes=%d headers=%s",
		round.ID,
		info.Method,
		info.Path,
		info.RawQuery,
		info.RemoteAddr,
		info.Host,
		info.UserAgent,
		bodyBytes,
		headerSummary(sanitizeHeaders(r.Header)),
	)
}

// archiveAndLogTransportPlan 记录 RouteOwner 选择与 TransportPlan 权威字段。
func (h *Handler) archiveAndLogTransportPlan(round *archive.Round, r *http.Request, plan TransportPlan, provider config.Provider, stream bool) {
	if r != nil {
		if trace, ok := r.Context().Value(featureExecutionTraceKey{}).(*featureExecutionTrace); ok && trace != nil {
			trace.provider = plan.RouteOwner
		}
	}
	if round != nil {
		round.SetTransportPlan(
			RouteLabel(r),
			plan.ClientEndpoint,
			plan.ClientProtocol,
			plan.UpstreamProtocol,
			plan.UpstreamEndpoint,
			plan.Mode,
		)
		if plan.IsConversion() {
			round.SetConversionLevel(plan.ConversionLevel)
		}
	}
	if round == nil {
		return
	}
	h.debugfRound(round, r, "round=%06d selected provider=%s protocol=%s model=%s stream=%t mode=%s client_endpoint=%s upstream_endpoint=%s base_url=%s",
		round.ID,
		plan.RouteOwner,
		plan.UpstreamProtocol,
		plan.ModelID,
		stream,
		plan.Mode,
		plan.ClientEndpoint,
		plan.UpstreamEndpoint,
		provider.BaseURL,
	)
}

func (h *Handler) archiveAndLogUpstreamRequest(round *archive.Round, r *http.Request, providerName string, provider config.Provider, req *http.Request, bodyBytes int) {
	if round == nil || req == nil {
		return
	}
	info := upstreamDebugInfo{
		RoundID:   round.ID,
		At:        time.Now(),
		Provider:  providerName,
		Protocol:  provider.Protocol,
		Method:    req.Method,
		URL:       req.URL.String(),
		BodyBytes: bodyBytes,
		Headers:   h.archiveHeaders(req.Header),
	}
	if round.FullContent() && req.GetBody != nil {
		reader, err := req.GetBody()
		if err == nil {
			body, readErr := io.ReadAll(reader)
			_ = reader.Close()
			if readErr == nil && h.writeArchiveResponse(round, "upstream_request_body.json", body) == nil {
				info.BodyPath = "upstream_request_body.json"
			}
		}
	}
	if err := round.WriteJSON("upstream_request.json", info); err != nil {
		log.Printf("archive upstream request metadata: %v", err)
	}
	h.debugfRound(round, r, "round=%06d upstream request provider=%s protocol=%s method=%s url=%s body_bytes=%d headers=%s",
		round.ID,
		providerName,
		provider.Protocol,
		req.Method,
		req.URL.String(),
		bodyBytes,
		headerSummary(sanitizeHeaders(req.Header)),
	)
}

func (h *Handler) archiveAndLogUpstreamResponse(round *archive.Round, r *http.Request, providerName string, provider config.Provider, resp *http.Response, duration time.Duration, err error) {
	if round == nil {
		return
	}
	info := upstreamResponseDebugInfo{
		RoundID:    round.ID,
		At:         time.Now(),
		Provider:   providerName,
		Protocol:   provider.Protocol,
		DurationMS: duration.Milliseconds(),
	}
	var logHeaders map[string][]string
	if resp != nil {
		info.Status = resp.StatusCode
		info.ContentType = resp.Header.Get("Content-Type")
		info.ContentLength = resp.ContentLength
		// 完整上游响应 header：x-ratelimit-* / retry-after / server 等对排查限流
		// 与上游行为关键，且无法从 Content-Type / Content-Length 还原。
		info.Headers = h.archiveHeaders(resp.Header)
		logHeaders = sanitizeHeaders(resp.Header)
	}
	if err != nil {
		info.Error = err.Error()
	}
	if writeErr := round.WriteJSON("upstream_response.json", info); writeErr != nil {
		log.Printf("archive upstream response metadata: %v", writeErr)
	}
	h.debugfRound(round, r, "round=%06d upstream response provider=%s protocol=%s status=%d duration=%s content_type=%q content_length=%d error=%q headers=%s",
		round.ID,
		providerName,
		provider.Protocol,
		info.Status,
		duration.Truncate(time.Millisecond),
		info.ContentType,
		info.ContentLength,
		info.Error,
		headerSummary(logHeaders),
	)
	h.logUpstreamAlert(round, providerName, provider.Protocol, info.Status, duration, info.Error)
}

// archiveCodexUpstreamAttempt persists the credential-safe HTTP observation
// emitted by the codexupstream Block. The Block owns the transport and performs
// first-pass redaction; the adapter sanitizes again before writing or logging.
func (h *Handler) archiveCodexUpstreamAttempt(round *archive.Round, r *http.Request, providerName string, attempt codexresponses.HTTPAttempt, attemptErr error) {
	if round == nil || strings.TrimSpace(attempt.Request.URL) == "" {
		return
	}
	requestAt := attempt.Request.At
	if requestAt.IsZero() {
		requestAt = round.StartedAt
	}
	requestHeaders := h.archiveHeaders(codexHeadersToHTTP(attempt.Request.Headers))
	if round.UpstreamAttempts == nil {
		round.UpstreamAttempts = make(map[string]int)
	}
	key := attempt.Request.At.Format(time.RFC3339Nano) + " " + attempt.Request.Method + " " + attempt.Request.URL
	index := round.UpstreamAttempts[key]
	newAttempt := index == 0
	if newAttempt {
		index = len(round.UpstreamAttempts) + 1
		round.UpstreamAttempts[key] = index
	}
	if index < len(round.UpstreamAttempts) {
		return
	}
	if round.UpstreamAttemptFailed[index] && attemptErr == nil {
		return // A late handshake callback cannot erase a terminal error.
	}
	if attemptErr != nil {
		if round.UpstreamAttemptFailed == nil {
			round.UpstreamAttemptFailed = make(map[int]bool)
		}
		round.UpstreamAttemptFailed[index] = true
	}
	if newAttempt {
		requestInfo := upstreamDebugInfo{
			Attempt: index,
			RoundID: round.ID, At: requestAt, Provider: providerName, Protocol: "codexoauth",
			Method: attempt.Request.Method, URL: attempt.Request.URL, BodyBytes: attempt.Request.BodyBytes, Headers: requestHeaders,
		}
		if round.FullContent() && len(attempt.Request.Body) > 0 {
			body := h.codexArchiveBody(attempt.Request.Body)
			name := fmt.Sprintf("upstream_request_%03d.body.json", index)
			if err := h.writeArchiveResponse(round, name, body); err != nil {
				log.Printf("archive Codex upstream body: %v", err)
			} else {
				requestInfo.BodyPath = name
			}
			if err := h.writeArchiveResponse(round, "upstream_request_body.json", body); err != nil {
				log.Printf("archive Codex upstream body snapshot: %v", err)
			}
		}
		if err := round.WriteJSON(fmt.Sprintf("upstream_request_%03d.json", index), requestInfo); err != nil {
			log.Printf("archive Codex attempt request: %v", err)
		}
		if err := round.WriteJSON("upstream_request.json", requestInfo); err != nil {
			log.Printf("archive Codex upstream request metadata: %v", err)
		}
		h.debugfRound(round, r, "round=%06d Codex upstream request provider=%s method=%s url=%s body_bytes=%d headers=%s",
			round.ID, providerName, requestInfo.Method, requestInfo.URL, requestInfo.BodyBytes, headerSummary(sanitizeHeaders(codexHeadersToHTTP(attempt.Request.Headers))))

	}
	responseAt := attempt.Response.At
	if responseAt.IsZero() {
		responseAt = requestAt
	}
	responseHeaders := h.archiveHeaders(codexHeadersToHTTP(attempt.Response.Headers))
	if attempt.Response.Observed {
		round.SetUpstreamHeaders(attempt.Response.Status, http.Header(responseHeaders).Get("Content-Type"), attempt.Response.ContentLength, attempt.Response.TransferEncoding, time.Duration(attempt.Response.DurationMS)*time.Millisecond)
	} else {
		// A later transport failure must not inherit an earlier attempt's headers.
		round.SetUpstreamHeaders(0, "", -1, "", 0)
	}
	responseInfo := upstreamResponseDebugInfo{
		TransferEncoding: attempt.Response.TransferEncoding,
		Attempt:          index,
		RoundID:          round.ID, At: responseAt, Provider: providerName, Protocol: "codexoauth",
		Status: attempt.Response.Status, DurationMS: attempt.Response.DurationMS,
		ContentType: http.Header(responseHeaders).Get("Content-Type"), ContentLength: attempt.Response.ContentLength,
		Headers: responseHeaders,
	}
	if attemptErr != nil {
		responseInfo.Error = attemptErr.Error()
		if failure, ok := codexresponses.AsFailure(attemptErr); ok {
			responseInfo.FailureClass = string(failure.Kind)
			responseInfo.RetryAfterSeconds = failure.RetryAfterSeconds
			responseInfo.UpstreamErrorCode = failure.UpstreamCode
			responseInfo.UpstreamErrorType = failure.UpstreamType
			responseInfo.UpstreamErrorParam = failure.UpstreamParam
			responseInfo.UpstreamErrorMessage = failure.UpstreamMessage
		}
	}
	responseInfo.ErrorBodyFormat = attempt.Response.ErrorBodyFormat
	responseInfo.ErrorBodyTruncated = attempt.Response.ErrorBodyTruncated
	responseInfo.ErrorBodyReadFailed = attempt.Response.ErrorBodyReadFailed
	if round.FullContent() && len(attempt.Response.ErrorBody) > 0 {
		name := fmt.Sprintf("upstream_response_%03d.error.body.txt", index)
		if err := h.writeArchiveResponse(round, name, attempt.Response.ErrorBody); err == nil {
			responseInfo.ErrorBodyPath = name
		}
	}
	encoded, encodeErr := json.Marshal(responseInfo)
	if encodeErr != nil {
		return
	}
	digest := sha256.Sum256(encoded)
	if round.UpstreamResponseDigests == nil {
		round.UpstreamResponseDigests = make(map[int][32]byte)
	}
	if previous, ok := round.UpstreamResponseDigests[index]; ok && previous == digest {
		return
	}
	written := true
	if err := round.WriteJSON(fmt.Sprintf("upstream_response_%03d.json", index), responseInfo); err != nil {
		written = false
		log.Printf("archive Codex attempt response: %v", err)
	}
	if err := round.WriteJSON("upstream_response.json", responseInfo); err != nil {
		written = false
		log.Printf("archive Codex upstream response metadata: %v", err)
	}
	if written {
		round.UpstreamResponseDigests[index] = digest
	}
	h.debugfRound(round, r, "round=%06d Codex upstream response provider=%s status=%d duration=%dms content_type=%q content_length=%d error=%q headers=%s",
		round.ID, providerName, responseInfo.Status, responseInfo.DurationMS, responseInfo.ContentType, responseInfo.ContentLength, responseInfo.Error, headerSummary(sanitizeHeaders(codexHeadersToHTTP(attempt.Response.Headers))))
}

// The callback stays within proxyapi; only value observations cross EventHub.
func (h *Handler) codexAttemptObserver(round *archive.Round, r *http.Request, provider string) func(codexresponses.HTTPAttempt, error) {
	if round == nil {
		return nil
	}
	return func(attempt codexresponses.HTTPAttempt, err error) {
		h.archiveCodexUpstreamAttempt(round, r, provider, attempt, err)
	}
}

func (h *Handler) codexArchiveBody(body []byte) []byte {
	if h.archiveUnredactedHeaders() {
		return body
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(body, &value) != nil {
		return body
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(value["client_metadata"], &metadata) != nil {
		return body
	}
	for _, key := range []string{"session_id", "thread_id", "installation_id", "window_id", "turn_id", "root_turn_id", "x-codex-installation-id", "x-codex-window-id", "x-codex-turn-metadata"} {
		if _, present := metadata[key]; present {
			metadata[key] = json.RawMessage(`"<redacted>"`)
		}
	}
	value["client_metadata"], _ = json.Marshal(metadata)
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}

func codexHeadersToHTTP(headers []codexresponses.Header) http.Header {
	result := make(http.Header, len(headers))
	for _, header := range headers {
		name := strings.TrimSpace(header.Name)
		if name != "" {
			result.Add(name, header.Value)
		}
	}
	return result
}

func (h *Handler) logUpstreamAlert(round *archive.Round, providerName, protocol string, status int, duration time.Duration, errMessage string) {
	if !h.cfg.VerboseLogging {
		return
	}
	if status < 400 && errMessage == "" {
		return
	}
	roundID := 0
	if round != nil {
		roundID = round.ID
	}
	level := slog.LevelWarn
	if errMessage != "" || status >= 500 {
		level = slog.LevelError
	}
	message := errMessage
	if message == "" {
		message = http.StatusText(status)
	}
	attrs := []any{
		slog.Int("round", roundID),
		slog.String("provider", providerName),
		slog.String("protocol", protocol),
		slog.Int("status", status),
		slog.Duration("duration", duration.Truncate(time.Millisecond)),
		slog.String("message", message),
	}
	slog.LogAttrs(context.Background(), level, "upstream alert", toAttrs(attrs)...)
}

// archiveHeaders 生成写入归档文件的 header 投影（CP-OBS-009）。默认与日志使用
// 同一脱敏名单；只有受控排障显式开启 archive_unredacted_headers 且归档本身已启用
// 时，四类信息才保留原值。日志始终走 sanitizeHeaders，不随该开关放宽。
func (h *Handler) archiveHeaders(headers http.Header) map[string][]string {
	return archiveHeaderProjection(headers, h.archiveUnredactedHeaders())
}

// archiveUnredactedHeaders 报告 CP-OBS-009 的保真开关。它只在交互归档启用时生效。
func (h *Handler) archiveUnredactedHeaders() bool {
	cfg := config.Config{}
	if h != nil {
		cfg = h.currentConfig()
	}
	return cfg.ArchiveInteractions && cfg.ArchiveUnredactedHeaders
}

func archiveHeaderProjection(headers http.Header, unredacted bool) map[string][]string {
	if unredacted {
		return rawHeaders(headers)
	}
	return sanitizeHeaders(headers)
}

// rawHeaders 复制全部 header 原值，仅用于显式开启保真的归档写入。
func rawHeaders(headers http.Header) map[string][]string {
	raw := make(map[string][]string, len(headers))
	for key, values := range headers {
		copied := make([]string, len(values))
		copy(copied, values)
		raw[http.CanonicalHeaderKey(key)] = copied
	}
	return raw
}

func sanitizeHeaders(headers http.Header) map[string][]string {
	sanitized := make(map[string][]string, len(headers))
	for key, values := range headers {
		canonical := http.CanonicalHeaderKey(key)
		if isSensitiveHeader(canonical) {
			sanitized[canonical] = []string{"<redacted>"}
			continue
		}
		copied := make([]string, len(values))
		copy(copied, values)
		sanitized[canonical] = copied
	}
	return sanitized
}

// isSensitiveHeader 判断某个 header 是否必须在归档前脱敏。
//
// 名单保持显式可预测：刻意不用 "-token" / "-key" 这类模糊后缀匹配，否则会误伤
// x-ratelimit-remaining-tokens 等排查限流时最需要看到的诊断头。
// 除请求方向的凭据外，也覆盖响应方向的质询头：上游可能把 Bearer / Digest
// nonce 回显在 WWW-Authenticate 一类头里。
func isSensitiveHeader(key string) bool {
	switch strings.ToLower(key) {
	case "authorization",
		"proxy-authorization",
		"proxy-authenticate",
		"www-authenticate",
		"authentication-info",
		"x-api-key",
		"api-key",
		"x-auth-token",
		"x-access-token",
		"x-goog-api-key",
		"x-amz-security-token",
		"cookie",
		"set-cookie",
		"chatgpt-account-id",
		"session-id",
		"session_id",
		"thread-id",
		"x-client-request-id",
		"x-codex-installation-id",
		"x-codex-turn-metadata",
		"x-codex-turn-state",
		"x-codex-window-id":
		return true
	default:
		return false
	}
}

// toAttrs 把 []any 中的元素逐个识别为 slog.Attr,返回同质 Attr 切片。
// 支持混合 slog.Attr 元素与 key-value 交替形式(向后兼容)。
func toAttrs(items []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(items))
	for _, item := range items {
		switch v := item.(type) {
		case slog.Attr:
			out = append(out, v)
		case []slog.Attr:
			out = append(out, v...)
		default:
			// key-value 交替形式:每两个元素为一组,key 必须是 string。
			continue
		}
	}
	return out
}

func headerSummary(headers map[string][]string) string {
	if len(headers) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		values := headers[key]
		if len(values) == 0 {
			parts = append(parts, key+"=")
			continue
		}
		parts = append(parts, key+"="+strings.Join(values, "|"))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
