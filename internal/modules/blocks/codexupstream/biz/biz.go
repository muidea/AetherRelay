// Package biz owns native Codex Responses upstream execution.
package biz

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	basebiz "ai-proxy/internal/modules/base/biz"
	"ai-proxy/internal/modules/blocks/codexupstream/pkg/common"
	events "ai-proxy/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/google/uuid"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

const (
	// Keep the native Codex identity independent from downstream client headers.
	// The upstream accepts a Codex CLI-style request without exposing arbitrary
	// client User-Agent or Originator values to the account provider.
	codexUserAgent  = "codex-tui/0.135.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.135.0)"
	codexOriginator = "codex-tui"
)

var responsesURL = "https://chatgpt.com/backend-api/codex/responses"

type streamUpdate struct {
	data              []byte
	done              bool
	errorClass        events.ErrorClass
	retryAfterSeconds int
}

type responseStream struct {
	cancel  context.CancelFunc
	updates chan streamUpdate
}

type Upstream struct {
	basebiz.Base
	topics  []string
	mu      sync.Mutex
	streams map[string]*responseStream
}

func New(hub event.Hub, background task.BackgroundRoutine) *Upstream {
	b := &Upstream{Base: basebiz.New(common.UnitID, hub, background), streams: map[string]*responseStream{}}
	b.topics = []string{events.TopicComplete, events.TopicStart, events.TopicPull, events.TopicCancel}
	b.SubscribeFunc(events.TopicComplete, b.handleComplete)
	b.SubscribeFunc(events.TopicStart, b.handleStart)
	b.SubscribeFunc(events.TopicPull, b.handlePull)
	b.SubscribeFunc(events.TopicCancel, b.handleCancel)
	return b
}

func (s *Upstream) Run(context.Context) *cd.Error { return nil }
func (s *Upstream) Teardown(context.Context) {
	for _, topic := range s.topics {
		s.UnsubscribeFunc(topic)
	}
	s.mu.Lock()
	for _, stream := range s.streams {
		stream.cancel()
	}
	s.streams = map[string]*responseStream{}
	s.mu.Unlock()
}

func (s *Upstream) handleComplete(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.CompleteCommand)
	if !ok || strings.TrimSpace(cmd.AccessToken) == "" || len(cmd.Body) == 0 {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex complete command"))
		return
	}
	body, err := forceStream(cmd.Body)
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid native Responses request"))
		return
	}
	response, class, retryAfter, err := perform(ev.Context(), cmd.AccessToken, cmd.AccountIDHeader, cmd.Proxy, body)
	if err != nil {
		result.Set(events.CompleteResult{ErrorClass: class, RetryAfterSeconds: retryAfter}, nil)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		result.Set(events.CompleteResult{Headers: responseHeaders(response.Header), ErrorClass: classifyStatus(response.StatusCode), RetryAfterSeconds: retryAfterSeconds(response.Header)}, nil)
		return
	}
	completed, class, err := completedResponse(response, cmd.MaxResponseBytes)
	if err != nil {
		result.Set(events.CompleteResult{Headers: responseHeaders(response.Header), ErrorClass: class}, nil)
		return
	}
	result.Set(events.CompleteResult{Body: completed, Headers: responseHeaders(response.Header)}, nil)
}

func (s *Upstream) handleStart(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.StartCommand)
	if !ok || strings.TrimSpace(cmd.AccessToken) == "" || len(cmd.Body) == 0 {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex stream command"))
		return
	}
	body, err := forceStream(cmd.Body)
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid native Responses request"))
		return
	}
	response, class, retryAfter, err := perform(ev.Context(), cmd.AccessToken, cmd.AccountIDHeader, cmd.Proxy, body)
	if err != nil {
		result.Set(events.StartResult{ErrorClass: class, RetryAfterSeconds: retryAfter}, nil)
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		result.Set(events.StartResult{Headers: responseHeaders(response.Header), ErrorClass: classifyStatus(response.StatusCode), RetryAfterSeconds: retryAfterSeconds(response.Header)}, nil)
		return
	}
	streamID := uuid.NewString()
	// The EventHub command is synchronous and may finish before its SSE reader.
	// Keep request values, but let explicit Pull/Cancel own stream lifetime.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ev.Context()))
	stream := &responseStream{cancel: cancel, updates: make(chan streamUpdate, 64)}
	s.mu.Lock()
	s.streams[streamID] = stream
	s.mu.Unlock()
	if err := s.BackgroundRoutine().AsyncFunction(func() { s.runStream(ctx, streamID, stream, response.Body, cmd.MaxLineBytes) }); err != nil {
		s.removeStream(streamID)
		cancel()
		_ = response.Body.Close()
		result.Set(nil, cd.NewError(cd.Unexpected, "Codex stream task unavailable"))
		return
	}
	result.Set(events.StartResult{StreamID: streamID, Headers: responseHeaders(response.Header)}, nil)
}

func (s *Upstream) handlePull(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.PullCommand)
	if !ok || strings.TrimSpace(cmd.StreamID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex stream pull command"))
		return
	}
	stream := s.stream(cmd.StreamID)
	if stream == nil {
		result.Set(events.PullResult{Done: true, ErrorClass: events.ErrorProtocol}, nil)
		return
	}
	timeout := time.Duration(cmd.TimeoutMillis) * time.Millisecond
	if timeout <= 0 || timeout > time.Minute {
		timeout = time.Second
	}
	select {
	case update, ok := <-stream.updates:
		if !ok {
			result.Set(events.PullResult{Done: true}, nil)
			return
		}
		if update.done {
			s.removeStream(cmd.StreamID)
		}
		result.Set(events.PullResult{Data: update.data, Done: update.done, ErrorClass: update.errorClass, RetryAfterSeconds: update.retryAfterSeconds}, nil)
	case <-time.After(timeout):
		result.Set(events.PullResult{}, nil)
	case <-ev.Context().Done():
		result.Set(nil, cd.NewError(cd.Unexpected, "Codex stream pull canceled"))
	}
}

func (s *Upstream) handleCancel(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.CancelCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex stream cancel command"))
		return
	}
	canceled := s.removeStream(cmd.StreamID)
	result.Set(events.CancelResult{Cancelled: canceled}, nil)
}

func (s *Upstream) runStream(ctx context.Context, streamID string, stream *responseStream, body io.ReadCloser, maxLine int64) {
	defer body.Close()
	defer close(stream.updates)
	defer s.removeStream(streamID)
	reader := bufio.NewReader(body)
	for {
		line, err := readLine(reader, maxLine)
		if len(line) > 0 {
			if !sendUpdate(ctx, stream, streamUpdate{data: line}) {
				return
			}
			if done, class := terminalClass(line); done {
				sendUpdate(ctx, stream, streamUpdate{done: true, errorClass: class})
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				sendUpdate(ctx, stream, streamUpdate{done: true, errorClass: events.ErrorProtocol})
				return
			}
			sendUpdate(ctx, stream, streamUpdate{done: true, errorClass: classifyTransport(err)})
			return
		}
	}
}

func (s *Upstream) stream(id string) *responseStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[id]
}
func (s *Upstream) removeStream(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	stream := s.streams[id]
	if stream == nil {
		return false
	}
	delete(s.streams, id)
	stream.cancel()
	return true
}

func perform(ctx context.Context, accessToken, accountID, proxy string, body []byte) (*http.Response, events.ErrorClass, int, error) {
	client, err := newHTTPClient(proxy)
	if err != nil {
		return nil, events.ErrorProtocol, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, events.ErrorProtocol, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Connection", "Keep-Alive")
	req.Header.Set("User-Agent", codexUserAgent)
	req.Header.Set("Originator", codexOriginator)
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, classifyTransport(err), 0, err
	}
	return response, "", retryAfterSeconds(response.Header), nil
}

func forceStream(body []byte) ([]byte, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(body, &values); err != nil {
		return nil, err
	}
	values["stream"] = json.RawMessage("true")
	return json.Marshal(values)
}

func completedResponse(response *http.Response, maxBytes int64) ([]byte, events.ErrorClass, error) {
	if response == nil || response.Body == nil {
		return nil, events.ErrorProtocol, fmt.Errorf("Codex response body is unavailable")
	}
	if maxBytes <= 0 {
		maxBytes = 32 << 20
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
		payload, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
		if err != nil {
			return nil, classifyTransport(err), err
		}
		if int64(len(payload)) > maxBytes {
			return nil, events.ErrorProtocol, fmt.Errorf("Codex response exceeds limit of %d bytes", maxBytes)
		}
		var object struct {
			Object string `json:"object"`
		}
		if err := json.Unmarshal(payload, &object); err != nil || object.Object != "response" {
			return nil, events.ErrorProtocol, fmt.Errorf("invalid native Codex response object")
		}
		return payload, "", nil
	}
	reader := bufio.NewReader(response.Body)
	for {
		line, err := readLine(reader, 1<<20)
		if len(line) > 0 {
			trimmed := strings.TrimSpace(string(line))
			if strings.HasPrefix(trimmed, "data:") {
				payload := []byte(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
				var event struct {
					Type     string          `json:"type"`
					Response json.RawMessage `json:"response"`
				}
				if json.Unmarshal(payload, &event) == nil {
					switch event.Type {
					case "response.completed":
						if len(event.Response) > 0 {
							return event.Response, "", nil
						}
					case "response.failed", "response.incomplete":
						return nil, events.ErrorUpstream, fmt.Errorf("Codex response did not complete")
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, events.ErrorProtocol, fmt.Errorf("Codex response ended without response.completed")
			}
			return nil, classifyTransport(err), err
		}
	}
}

func readLine(reader *bufio.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = 1 << 20
	}
	var out []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(part) > 0 {
			if int64(len(out)+len(part)) > limit {
				return nil, fmt.Errorf("Codex SSE line exceeds limit")
			}
			out = append(out, part...)
		}
		if err == nil || err != bufio.ErrBufferFull {
			return out, err
		}
	}
}

func terminalClass(line []byte) (bool, events.ErrorClass) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return false, ""
	}
	var item struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))), &item) != nil {
		return false, ""
	}
	switch item.Type {
	case "response.completed":
		return true, ""
	case "response.failed", "response.incomplete":
		return true, events.ErrorUpstream
	default:
		return false, ""
	}
}

func sendUpdate(ctx context.Context, stream *responseStream, update streamUpdate) bool {
	select {
	case stream.updates <- update:
		return true
	case <-ctx.Done():
		return false
	}
}

func responseHeaders(headers http.Header) []events.Header {
	out := make([]events.Header, 0, 2)
	for _, key := range []string{"Content-Type", "X-Request-ID"} {
		if value := strings.TrimSpace(headers.Get(key)); value != "" {
			out = append(out, events.Header{Name: key, Value: value})
		}
	}
	return out
}

func retryAfterSeconds(headers http.Header) int {
	value := strings.TrimSpace(headers.Get("Retry-After"))
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 && seconds <= 3600 {
		return seconds
	}
	return 0
}
func classifyStatus(status int) events.ErrorClass {
	if status == http.StatusUnauthorized {
		return events.ErrorInvalidToken
	}
	if status == http.StatusTooManyRequests {
		return events.ErrorRateLimit
	}
	return events.ErrorUpstream
}
func classifyTransport(err error) events.ErrorClass {
	if errors.Is(err, context.DeadlineExceeded) {
		return events.ErrorTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return events.ErrorTimeout
	}
	return events.ErrorNetwork
}
func newHTTPClient(rawProxy string) (*http.Client, error) {
	base, _ := http.DefaultTransport.(*http.Transport)
	transport := base.Clone()
	if rawProxy = strings.TrimSpace(rawProxy); rawProxy != "" {
		proxyURL, err := url.ParseRequestURI(rawProxy)
		if err != nil || proxyURL.Host == "" || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https") {
			return nil, fmt.Errorf("invalid account proxy URL")
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: transport}, nil
}
