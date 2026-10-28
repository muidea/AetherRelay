package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	codexmodel "aetherrelay/internal/pkg/aetherrelaycodexmodel"
	transport "aetherrelay/internal/pkg/aetherrelaytransport"
	fhttp "github.com/bogdanfinn/fhttp"
	wsclient "github.com/bogdanfinn/websocket"
)

// Both native WS sessions and HTTP/SSE entry points use the same handshake.
func dialResponsesWebsocket(ctx context.Context, cmd events.WSOpenCommand) (*wsclient.Conn, events.WSOpenResult, []byte) {
	if err := websocketContextError(ctx); err != nil {
		return nil, events.WSOpenResult{ErrorClass: classifyRequestTransport(ctx, err)}, nil
	}
	dialer, err := newWebsocketDialer(cmd.Proxy)
	if err != nil {
		return nil, events.WSOpenResult{ErrorClass: events.ErrorProtocol}, nil
	}
	profile := codexRequestProfile{sessionHash: cmd.SessionHash, betaFeatures: cmd.BetaFeatures, responsesLite: cmd.ResponsesLite, turnState: cmd.TurnState, fingerprint: cmd.Fingerprint, archiveUnredacted: cmd.ArchiveUnredactedHeaders, archiveFullContent: cmd.ArchiveFullContent, clientIdentity: cmd.ClientIdentity, turnMetadata: cmd.TurnMetadata}
	headers := fhttp.Header{}
	headers.Set("Authorization", "Bearer "+strings.TrimSpace(cmd.AccessToken))
	headers.Set("User-Agent", profile.requestUserAgent())
	headers.Set("Originator", profile.requestOriginator())
	headers.Set("OpenAI-Beta", currentIdentity.WebsocketBeta)
	applyCodexFeatureHeaders(headers, profile.betaFeatures, profile.responsesLite)
	applyCodexRequestIdentity(headers, profile)
	applyCodexTurnMetadata(headers, profile)
	if accountID := strings.TrimSpace(cmd.AccountIDHeader); accountID != "" {
		headers.Set("ChatGPT-Account-ID", accountID)
	}
	requestAt := time.Now()
	attempt := events.HTTPAttempt{Request: events.HTTPRequestObservation{QueueWaitMS: dispatchQueueWait(ctx).Milliseconds(), At: requestAt, Method: http.MethodGet, URL: responsesWebsocketURL, Headers: safeFHTTPHeaders(headers, profile.archiveUnredacted)}}
	releaseHandshake := guardWebsocketHandshake(ctx, dialer)
	conn, response, err := dialer.DialContext(ctx, responsesWebsocketURL, headers)
	releaseHandshake()
	if contextErr := websocketContextError(ctx); contextErr != nil {
		if conn != nil {
			_ = conn.Close()
			conn = nil
		}
		err = contextErr
	}
	attempt.Response.DurationMS = time.Since(requestAt).Milliseconds()
	if err != nil {
		status := 0
		var responseBody []byte
		var observation events.RateLimitObservation
		var retryAfter int
		var safeError events.SafeError
		var responseHeader []events.Header
		if response != nil {
			attempt.Response = observedHTTPResponse(response.StatusCode, response.ContentLength, response.TransferEncoding, response.Header, requestAt, profile.archiveUnredacted)
			status = response.StatusCode
			responseHeader = responseHeaders(response.Header)
			responseBody, observation, retryAfter, safeError = readArchivedErrorObservation(&http.Response{StatusCode: response.StatusCode, Header: http.Header(response.Header), Body: response.Body}, &attempt, profile)
			if response.Body != nil {
				_ = response.Body.Close()
			}
		}
		class := classifyRequestTransport(ctx, err)
		if status > 0 && websocketContextError(ctx) == nil {
			class = errorClassWithBody(status, responseBody, observation)
		}
		attempt.Response.Transport = "websocket"
		return nil, events.WSOpenResult{Headers: responseHeader, Attempt: attempt, HTTPStatus: status, ErrorClass: class, RetryAfterSeconds: retryAfter, RateLimit: observation, SafeError: safeError}, responseBody
	}
	var responseHeader []events.Header
	if response != nil {
		attempt.Response = observedHTTPResponse(response.StatusCode, response.ContentLength, response.TransferEncoding, response.Header, requestAt, profile.archiveUnredacted)
		responseHeader = responseHeaders(response.Header)
		if response.Body != nil {
			_ = response.Body.Close()
		}
	}
	limit := cmd.MaxMessageBytes
	if limit <= 0 || limit > 16<<20 {
		limit = 1 << 20
	}
	conn.SetReadLimit(limit)
	attempt.Response.WebsocketMessageLimitBytes = limit
	attempt.Response.Transport = "websocket"
	return conn, events.WSOpenResult{Headers: responseHeader, Attempt: attempt}, nil
}

// The websocket client does not observe cancellation while reading CONNECT or
// Upgrade response headers. Close its underlying connection during handshake,
// then disarm and join the callback before handing ownership to the session.
func guardWebsocketHandshake(ctx context.Context, dialer *wsclient.Dialer) func() {
	var releases []func()
	wrap := func(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
		return func(dialCtx context.Context, network, address string) (net.Conn, error) {
			conn, err := dial(dialCtx, network, address)
			if err != nil {
				return nil, err
			}
			done := make(chan struct{})
			stop := context.AfterFunc(ctx, func() {
				defer close(done)
				_ = conn.Close()
			})
			releases = append(releases, func() {
				if !stop() {
					<-done
				}
			})
			return conn, nil
		}
	}
	baseDial := dialer.NetDialContext
	if baseDial == nil {
		baseDial = (&net.Dialer{}).DialContext
		if dialer.NetDial != nil {
			baseDial = func(_ context.Context, network, address string) (net.Conn, error) {
				return dialer.NetDial(network, address)
			}
		}
	}
	dialer.NetDialContext = wrap(baseDial)
	if dialer.NetDialTLSContext != nil {
		// HTTPS proxy dialing performs TLS before returning the connection.
		// That handshake already observes ctx; guard the following CONNECT too.
		dialer.NetDialTLSContext = wrap(dialer.NetDialTLSContext)
	}
	return func() {
		for _, release := range releases {
			release()
		}
	}
}

// No response.create has been sent at this boundary. Authentication, quota,
// model and request rejections must retain their meaning rather than switch
// transports. Canceled requests never start another network operation.
func websocketFallbackReason(ctx context.Context, opened events.WSOpenResult) string {
	if websocketContextError(ctx) != nil || opened.ErrorClass == "" {
		return ""
	}
	switch opened.ErrorClass {
	case events.ErrorModelNotFound, events.ErrorInvalidToken, events.ErrorRateLimit, events.ErrorCanceled, events.ErrorProtocol:
		return ""
	}
	switch opened.HTTPStatus {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUpgradeRequired, http.StatusNotImplemented:
		return "ws_upgrade_unsupported"
	case 0:
		if opened.ErrorClass == events.ErrorNetwork || opened.ErrorClass == events.ErrorTimeout {
			return "ws_connect_failed"
		}
	default:
		if opened.HTTPStatus >= 500 && opened.HTTPStatus <= 599 {
			return "ws_handshake_unavailable"
		}
	}
	return ""
}

// Socket deadlines and the context timer are scheduled independently. An I/O
// timeout can arrive at the request deadline before Err/Done reflect expiry.
// Never spend that exhausted request budget on another transport operation.
func websocketContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func websocketCreateBody(body []byte, profile codexRequestProfile) ([]byte, error) {
	payload, err := applyCodexRequestBody(body, profile)
	if err != nil {
		return nil, err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, err
	}
	delete(envelope, "stream")
	envelope["type"] = json.RawMessage(`"response.create"`)
	return encodeCodexJSON(envelope)
}

func fullResponsesInput(body []byte) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	previous := bytes.TrimSpace(envelope["previous_response_id"])
	return len(previous) == 0 || bytes.Equal(previous, []byte("null")) || bytes.Equal(previous, []byte(`""`))
}

// WS frames enter the existing SSE event processing, including terminal,
// metadata, output projection, usage and timeout handling. The reader runs in
// the existing Block-owned command/stream task, without an extra goroutine.
func performResponses(ctx context.Context, token, accountID, proxy string, body []byte, profile codexRequestProfile, preferWS bool, maxMessage int64) (*http.Response, events.HTTPAttempt, events.ErrorClass, int, error) {
	if err := websocketContextError(ctx); err != nil {
		return nil, events.HTTPAttempt{}, classifyRequestTransport(ctx, err), 0, err
	}
	if !preferWS {
		response, attempt, class, retry, err := perform(ctx, token, accountID, proxy, body, profile)
		attempt.Response.Transport = "http"
		return response, attempt, class, retry, err
	}
	payload, err := websocketCreateBody(body, profile)
	if err != nil {
		return nil, events.HTTPAttempt{}, events.ErrorProtocol, 0, err
	}
	conn, opened, rejectionBody := dialResponsesWebsocket(ctx, events.WSOpenCommand{
		AccessToken: token, AccountIDHeader: accountID, Proxy: proxy, MaxMessageBytes: maxMessage,
		SessionHash: profile.sessionHash, BetaFeatures: profile.betaFeatures, ResponsesLite: profile.responsesLite,
		TurnState: profile.turnState, Fingerprint: profile.fingerprint, ClientIdentity: profile.clientIdentity,
		TurnMetadata: profile.turnMetadata, ArchiveUnredactedHeaders: profile.archiveUnredacted, ArchiveFullContent: profile.archiveFullContent,
	})
	if opened.ErrorClass != "" {
		if err := websocketContextError(ctx); err != nil {
			return nil, opened.Attempt, classifyRequestTransport(ctx, err), 0, err
		}
		reason := websocketFallbackReason(ctx, opened)
		if reason != "" && httpFallbackCompatible(body) {
			slog.InfoContext(ctx, "Codex upstream transport fallback", "upstream_transport", "http", "transport_fallback_reason", reason, "ws_handshake_status", opened.HTTPStatus)
			response, attempt, class, retry, err := perform(ctx, token, accountID, proxy, body, profile)
			attempt.Response.Transport, attempt.Response.FallbackReason = "http", reason
			attempt.Prior = []events.HTTPAttempt{opened.Attempt}
			return response, attempt, class, retry, err
		}
		if opened.HTTPStatus > 0 {
			header := make(http.Header)
			for _, value := range opened.Headers {
				header.Add(value.Name, value.Value)
			}
			if opened.RetryAfterSeconds > 0 {
				header.Set("Retry-After", fmt.Sprint(opened.RetryAfterSeconds))
			}
			return &http.Response{StatusCode: opened.HTTPStatus, Header: header, Body: io.NopCloser(bytes.NewReader(rejectionBody))}, opened.Attempt, "", 0, nil
		}
		return nil, opened.Attempt, opened.ErrorClass, opened.RetryAfterSeconds, fmt.Errorf("Codex websocket handshake rejected")
	}
	attempt := opened.Attempt
	attempt.Prior = []events.HTTPAttempt{opened.Attempt}
	attempt.Request.Method = "WS"
	attempt.Request.BodyBytes = len(payload)
	if profile.archiveFullContent {
		attempt.Request.Body = bytes.Clone(payload)
	}
	attempt.Response.Observation = codexmodel.New(requestBodyModel(body), "")
	// Even a failed write can have delivered a complete request. Never turn this
	// path into HTTP fallback or transparent transport/account retries.
	attempt.Response.WebsocketRequestSent = true
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := conn.WriteMessage(wsclient.TextMessage, payload); err != nil {
		stop()
		_ = conn.Close()
		observed := websocketFailure(ctx, "write", err, attempt.Response.WebsocketDiagnostics)
		attempt.Response.WebsocketDiagnostics = observed.diagnostic
		attempt.Response.TransportReason = observed.reason
		return nil, attempt, classifyRequestTransport(ctx, err), 0, observed
	}
	reader := &responsesWebsocketBody{conn: conn, stop: stop, ctx: ctx, diagnostic: attempt.Response.WebsocketDiagnostics}
	header := make(http.Header)
	for _, value := range opened.Headers {
		header.Add(value.Name, value.Value)
	}
	// Synthetic headers describe the local SSE adapter only. The attempt keeps
	// the actual 101 status and upstream handshake headers for archival.
	header.Set("Content-Type", "text/event-stream")
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, responsesWebsocketURL, nil)
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: reader, ContentLength: -1, Request: request}, attempt, "", 0, nil
}

func httpFallbackCompatible(body []byte) bool {
	if !fullResponsesInput(body) {
		return false
	}
	var envelope struct {
		Generate *bool `json:"generate"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	// Native WS handles this prewarm with retained history. A standalone HTTP
	// request has no such local continuation state and must not infer instead.
	return envelope.Generate == nil || *envelope.Generate
}

func requestBodyModel(body []byte) string {
	var value struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &value)
	return value.Model
}

type responsesWebsocketBody struct {
	ctx        context.Context
	diagnostic transport.WebsocketDiagnostics
	conn       *wsclient.Conn
	stop       func() bool
	pending    []byte
	terminal   bool
	closeOnce  sync.Once
}

func normalizeResponsesWSEvent(payload []byte) ([]byte, bool, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, false, err
	}
	var kind string
	_ = json.Unmarshal(envelope["type"], &kind)
	switch kind {
	case "response.done":
		envelope["type"] = json.RawMessage(`"response.completed"`)
		encoded, err := encodeCodexJSON(envelope)
		return encoded, true, err
	case "response.completed", "response.incomplete", "response.failed", "response.cancelled", "response.canceled", "error":
		return payload, true, nil
	}
	return payload, false, nil
}

func (b *responsesWebsocketBody) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		if b.terminal {
			return 0, io.EOF
		}
		_ = b.conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		kind, payload, err := b.conn.ReadMessage()
		if err != nil {
			return 0, websocketFailure(b.ctx, "read", err, b.diagnostic)
		}
		b.diagnostic.WebsocketLastMessageObserved = true
		b.diagnostic.WebsocketLastMessageBytes = int64(len(payload))
		if kind != wsclient.TextMessage {
			continue
		}
		documents, repaired := splitCodexJSONDocuments(payload)
		if !repaired {
			documents = [][]byte{payload}
		}
		for _, document := range documents {
			normalized, terminal, err := normalizeResponsesWSEvent(document)
			if err != nil {
				return 0, websocketFailure(b.ctx, "decode", err, b.diagnostic)
			}
			b.pending = append(b.pending, "data: "...)
			b.pending = append(b.pending, normalized...)
			b.pending = append(b.pending, '\n', '\n')
			b.terminal = terminal
			if terminal {
				break
			}
		}
	}
	n := copy(dst, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *responsesWebsocketBody) Close() error {
	var err error
	b.closeOnce.Do(func() { b.stop(); err = b.conn.Close() })
	return err
}
