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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	basebiz "aetherrelay/internal/modules/base/biz"
	"aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
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

// The model-list endpoint is account-scoped. Its client version is a local
// Codex identity detail, never an inbound request parameter.
var modelsURL = "https://chatgpt.com/backend-api/codex/models?client_version=0.135.0"

// usageURL is the account-scoped Codex usage endpoint used by CLIProxyAPI's
// management surface as well. It is intentionally not exposed to clients.
var usageURL = "https://chatgpt.com/backend-api/wham/usage"

type streamUpdate struct {
	data              []byte
	done              bool
	errorClass        events.ErrorClass
	retryAfterSeconds int
	rateLimit         events.RateLimitObservation
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
	b.topics = []string{events.TopicComplete, events.TopicStart, events.TopicPull, events.TopicCancel, events.TopicListModels, events.TopicGetUsage}
	b.SubscribeFunc(events.TopicComplete, b.handleComplete)
	b.SubscribeFunc(events.TopicStart, b.handleStart)
	b.SubscribeFunc(events.TopicPull, b.handlePull)
	b.SubscribeFunc(events.TopicCancel, b.handleCancel)
	b.SubscribeFunc(events.TopicListModels, b.handleListModels)
	b.SubscribeFunc(events.TopicGetUsage, b.handleGetUsage)
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
		observation, retryAfter := readRateLimitObservation(response)
		result.Set(events.CompleteResult{Headers: responseHeaders(response.Header), HTTPStatus: response.StatusCode, ErrorClass: errorClassWithRateLimit(response.StatusCode, observation), RetryAfterSeconds: retryAfter, RateLimit: observation}, nil)
		return
	}
	completed, class, observation, err := completedResponse(response, cmd.MaxResponseBytes)
	if err != nil {
		result.Set(events.CompleteResult{Headers: responseHeaders(response.Header), ErrorClass: class, RetryAfterSeconds: retryAfterFromObservation(observation), RateLimit: observation}, nil)
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
		observation, retryAfter := readRateLimitObservation(response)
		_ = response.Body.Close()
		result.Set(events.StartResult{Headers: responseHeaders(response.Header), HTTPStatus: response.StatusCode, ErrorClass: errorClassWithRateLimit(response.StatusCode, observation), RetryAfterSeconds: retryAfter, RateLimit: observation}, nil)
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
			s.removeStream(cmd.StreamID)
			result.Set(events.PullResult{Done: true}, nil)
			return
		}
		if update.done {
			s.removeStream(cmd.StreamID)
		}
		result.Set(events.PullResult{Data: update.data, Done: update.done, ErrorClass: update.errorClass, RetryAfterSeconds: update.retryAfterSeconds, RateLimit: update.rateLimit}, nil)
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

// handleListModels projects the account-scoped Codex model endpoint into a
// bounded DTO. Raw upstream payloads and credentials never leave this Block.
func (s *Upstream) handleListModels(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.ListModelsCommand)
	if !ok || strings.TrimSpace(cmd.AccessToken) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex model list command"))
		return
	}
	models, class, err := listModels(ev.Context(), cmd.AccessToken, cmd.AccountIDHeader, cmd.Proxy)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "Codex model discovery failed: "+string(class)))
		return
	}
	result.Set(events.ListModelsResult{Models: models}, nil)
}

// handleGetUsage requests a bounded, allowlisted usage projection. Network,
// authentication and upstream failures are returned as classified results so
// proxyapi can persist safe operational context without raw response bodies.
func (s *Upstream) handleGetUsage(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.GetUsageCommand)
	if !ok || strings.TrimSpace(cmd.AccessToken) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex usage command"))
		return
	}
	planType, windows, class, err := getUsage(ev.Context(), cmd.AccessToken, cmd.AccountIDHeader, cmd.Proxy)
	if err != nil {
		result.Set(events.GetUsageResult{ErrorClass: class}, nil)
		return
	}
	result.Set(events.GetUsageResult{PlanType: planType, Windows: windows}, nil)
}

func (s *Upstream) runStream(ctx context.Context, streamID string, stream *responseStream, body io.ReadCloser, maxLine int64) {
	defer body.Close()
	defer close(stream.updates)
	reader := bufio.NewReader(body)
	pendingTerminal := false
	pendingClass := events.ErrorClass("")
	sawOutputDone := false
	for {
		line, err := readLine(reader, maxLine)
		if len(line) > 0 {
			if !sendUpdate(ctx, stream, streamUpdate{data: line}) {
				return
			}
			if done, class, observation := terminalOutcome(line); done {
				sendUpdate(ctx, stream, streamUpdate{done: true, errorClass: class, retryAfterSeconds: retryAfterFromObservation(observation), rateLimit: observation})
				return
			}
			if done, class := terminalEvent(line); done {
				pendingTerminal, pendingClass = true, class
			}
			if sseEventName(line) == "response.output_item.done" {
				sawOutputDone = true
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if pendingTerminal {
					sendUpdate(ctx, stream, streamUpdate{done: true, errorClass: pendingClass})
				} else if sawOutputDone {
					sendUpdate(ctx, stream, streamUpdate{done: true})
				} else {
					sendUpdate(ctx, stream, streamUpdate{done: true, errorClass: events.ErrorProtocol})
				}
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

func listModels(ctx context.Context, accessToken, accountID, proxy string) ([]events.ModelDescriptor, events.ErrorClass, error) {
	client, err := newHTTPClient(proxy)
	if err != nil {
		return nil, events.ErrorProtocol, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, events.ErrorProtocol, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("User-Agent", codexUserAgent)
	req.Header.Set("Originator", codexOriginator)
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, classifyTransport(err), err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, classifyStatus(response.StatusCode), fmt.Errorf("Codex models request returned %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil {
		return nil, classifyTransport(err), err
	}
	if len(body) > 8<<20 {
		return nil, events.ErrorProtocol, fmt.Errorf("Codex models response exceeds limit")
	}
	models, err := parseModels(body)
	if err != nil {
		return nil, events.ErrorProtocol, err
	}
	return models, "", nil
}

func getUsage(ctx context.Context, accessToken, accountID, proxy string) (string, []events.UsageWindow, events.ErrorClass, error) {
	client, err := newHTTPClient(proxy)
	if err != nil {
		return "", nil, events.ErrorProtocol, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return "", nil, events.ErrorProtocol, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("User-Agent", codexUserAgent)
	req.Header.Set("Originator", codexOriginator)
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
	response, err := client.Do(req)
	if err != nil {
		return "", nil, classifyTransport(err), err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", nil, classifyStatus(response.StatusCode), fmt.Errorf("Codex usage request returned %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return "", nil, classifyTransport(err), err
	}
	if len(body) > 1<<20 {
		return "", nil, events.ErrorProtocol, fmt.Errorf("Codex usage response exceeds limit")
	}
	planType, windows, err := parseUsage(body, time.Now().UTC())
	if err != nil {
		return "", nil, events.ErrorProtocol, err
	}
	return planType, windows, "", nil
}

type usageResponse struct {
	PlanType                  string            `json:"plan_type"`
	PlanTypeCamel             string            `json:"planType"`
	RateLimit                 *usageRateLimit   `json:"rate_limit"`
	RateLimitCamel            *usageRateLimit   `json:"rateLimit"`
	CodeReviewRateLimit       *usageRateLimit   `json:"code_review_rate_limit"`
	CodeReviewRateLimitCamel  *usageRateLimit   `json:"codeReviewRateLimit"`
	AdditionalRateLimits      []usageAdditional `json:"additional_rate_limits"`
	AdditionalRateLimitsCamel []usageAdditional `json:"additionalRateLimits"`
}

type usageAdditional struct {
	LimitName           string          `json:"limit_name"`
	LimitNameCamel      string          `json:"limitName"`
	MeteredFeature      string          `json:"metered_feature"`
	MeteredFeatureCamel string          `json:"meteredFeature"`
	RateLimit           *usageRateLimit `json:"rate_limit"`
	RateLimitCamel      *usageRateLimit `json:"rateLimit"`
}

type usageRateLimit struct {
	Allowed              json.RawMessage `json:"allowed"`
	LimitReached         json.RawMessage `json:"limit_reached"`
	LimitReachedCamel    json.RawMessage `json:"limitReached"`
	PrimaryWindow        *usageRawWindow `json:"primary_window"`
	PrimaryWindowCamel   *usageRawWindow `json:"primaryWindow"`
	SecondaryWindow      *usageRawWindow `json:"secondary_window"`
	SecondaryWindowCamel *usageRawWindow `json:"secondaryWindow"`
}

type usageRawWindow struct {
	UsedPercent             json.RawMessage `json:"used_percent"`
	UsedPercentCamel        json.RawMessage `json:"usedPercent"`
	LimitWindowSeconds      json.RawMessage `json:"limit_window_seconds"`
	LimitWindowSecondsCamel json.RawMessage `json:"limitWindowSeconds"`
	ResetAfterSeconds       json.RawMessage `json:"reset_after_seconds"`
	ResetAfterSecondsCamel  json.RawMessage `json:"resetAfterSeconds"`
	ResetAt                 json.RawMessage `json:"reset_at"`
	ResetAtCamel            json.RawMessage `json:"resetAt"`
}

func parseUsage(body []byte, now time.Time) (string, []events.UsageWindow, error) {
	var payload usageResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", nil, fmt.Errorf("decode Codex usage: %w", err)
	}
	planType := strings.TrimSpace(payload.PlanType)
	if planType == "" {
		planType = strings.TrimSpace(payload.PlanTypeCamel)
	}
	windows := make([]events.UsageWindow, 0, 8)
	appendUsageWindows(&windows, "standard", "", firstUsageRateLimit(payload.RateLimit, payload.RateLimitCamel), now)
	appendUsageWindows(&windows, "code-review", "", firstUsageRateLimit(payload.CodeReviewRateLimit, payload.CodeReviewRateLimitCamel), now)
	additional := payload.AdditionalRateLimits
	if len(additional) == 0 {
		additional = payload.AdditionalRateLimitsCamel
	}
	for index, item := range additional {
		name := firstNonEmpty(item.LimitName, item.LimitNameCamel, item.MeteredFeature, item.MeteredFeatureCamel)
		if name == "" {
			name = fmt.Sprintf("additional-%d", index+1)
		}
		appendUsageWindows(&windows, "additional-"+usageIdentifier(name, index+1), name, firstUsageRateLimit(item.RateLimit, item.RateLimitCamel), now)
	}
	if len(windows) == 0 {
		return "", nil, fmt.Errorf("Codex usage response has no usage windows")
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].ID < windows[j].ID })
	return planType, windows, nil
}

func firstUsageRateLimit(values ...*usageRateLimit) *usageRateLimit {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstUsageWindow(values ...*usageRawWindow) *usageRawWindow {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func appendUsageWindows(target *[]events.UsageWindow, scope, label string, limit *usageRateLimit, now time.Time) {
	if limit == nil {
		return
	}
	allowed, allowedKnown := rawBool(limit.Allowed)
	limitReached, _ := rawBool(firstRaw(limit.LimitReached, limit.LimitReachedCamel))
	if allowedKnown && !allowed {
		limitReached = true
	}
	for _, item := range []struct {
		name   string
		window *usageRawWindow
	}{
		{name: "primary", window: firstUsageWindow(limit.PrimaryWindow, limit.PrimaryWindowCamel)},
		{name: "secondary", window: firstUsageWindow(limit.SecondaryWindow, limit.SecondaryWindowCamel)},
	} {
		if item.window == nil {
			continue
		}
		usedPercent, usedKnown := rawFloat(firstRaw(item.window.UsedPercent, item.window.UsedPercentCamel))
		resetAt := usageResetAt(item.window, now)
		if !usedKnown && limitReached && resetAt != "" {
			usedPercent, usedKnown = 100, true
		}
		windowSeconds, _ := rawPositiveInt(firstRaw(item.window.LimitWindowSeconds, item.window.LimitWindowSecondsCamel))
		if windowSeconds > int64(366*24*60*60) {
			windowSeconds = 0
		}
		*target = append(*target, events.UsageWindow{
			ID:               scope + "-" + item.name,
			Label:            label,
			UsedPercent:      clampPercent(usedPercent),
			UsedPercentKnown: usedKnown,
			WindowSeconds:    int(windowSeconds),
			ResetAt:          resetAt,
			Allowed:          allowed,
			AllowedKnown:     allowedKnown,
			LimitReached:     limitReached,
		})
	}
}

func firstRaw(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) > 0 && string(value) != "null" {
			return value
		}
	}
	return nil
}

func rawBool(raw json.RawMessage) (bool, bool) {
	value := strings.Trim(strings.TrimSpace(string(raw)), "\"")
	if value == "true" || value == "1" {
		return true, true
	}
	if value == "false" || value == "0" {
		return false, true
	}
	return false, false
}

func rawFloat(raw json.RawMessage) (float64, bool) {
	value := strings.Trim(strings.TrimSpace(string(raw)), "\"")
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed != parsed || parsed < 0 || parsed > 100 {
		return 0, false
	}
	return parsed, true
}

func usageResetAt(window *usageRawWindow, now time.Time) string {
	if window == nil {
		return ""
	}
	if resetAt, ok := rawResetAt(firstRaw(window.ResetAt, window.ResetAtCamel), now); ok {
		return resetAt.UTC().Format(time.RFC3339)
	}
	if seconds, ok := rawPositiveInt(firstRaw(window.ResetAfterSeconds, window.ResetAfterSecondsCamel)); ok && seconds <= int64(366*24*60*60) {
		return now.Add(time.Duration(seconds) * time.Second).UTC().Format(time.RFC3339)
	}
	return ""
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func usageIdentifier(value string, fallback int) string {
	var out strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
		} else if out.Len() > 0 && !strings.HasSuffix(out.String(), "-") {
			out.WriteByte('-')
		}
	}
	result := strings.Trim(out.String(), "-")
	if result == "" {
		return strconv.Itoa(fallback)
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func parseModels(body []byte) ([]events.ModelDescriptor, error) {
	var payload struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode Codex models: %w", err)
	}
	if payload.Models == nil {
		return nil, fmt.Errorf("Codex models response has no models array")
	}
	seen := make(map[string]struct{}, len(payload.Models))
	out := make([]events.ModelDescriptor, 0, len(payload.Models))
	for _, raw := range payload.Models {
		var item struct {
			Slug    string `json:"slug"`
			ID      string `json:"id"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		id := strings.TrimSpace(item.Slug)
		if id == "" {
			id = strings.TrimSpace(item.ID)
		}
		if !validModelID(id) {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, events.ModelDescriptor{ID: id, CreatedAt: item.Created, OwnedBy: strings.TrimSpace(item.OwnedBy)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func validModelID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func forceStream(body []byte) ([]byte, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(body, &values); err != nil {
		return nil, err
	}
	// The public Responses API accepts a string input shorthand. The fixed
	// Codex backend accepts only the equivalent message-array representation.
	if raw, ok := values["input"]; ok {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			normalized, err := json.Marshal([]any{map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "input_text", "text": text}},
			}})
			if err != nil {
				return nil, err
			}
			values["input"] = normalized
		}
	}
	values["stream"] = json.RawMessage("true")
	// The ChatGPT Codex backend accepts native Responses requests only as
	// non-persisted executions. Downstream clients need not know this transport
	// detail, so enforce it at the upstream boundary.
	values["store"] = json.RawMessage("false")
	return json.Marshal(values)
}

func completedResponse(response *http.Response, maxBytes int64) ([]byte, events.ErrorClass, events.RateLimitObservation, error) {
	if response == nil || response.Body == nil {
		return nil, events.ErrorProtocol, events.RateLimitObservation{}, fmt.Errorf("Codex response body is unavailable")
	}
	if maxBytes <= 0 {
		maxBytes = 32 << 20
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
		payload, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
		if err != nil {
			return nil, classifyTransport(err), events.RateLimitObservation{}, err
		}
		if int64(len(payload)) > maxBytes {
			return nil, events.ErrorProtocol, events.RateLimitObservation{}, fmt.Errorf("Codex response exceeds limit of %d bytes", maxBytes)
		}
		var object struct {
			Object string `json:"object"`
		}
		if err := json.Unmarshal(payload, &object); err != nil || object.Object != "response" {
			return nil, events.ErrorProtocol, events.RateLimitObservation{}, fmt.Errorf("invalid native Codex response object")
		}
		return payload, "", events.RateLimitObservation{}, nil
	}
	reader := bufio.NewReader(response.Body)
	var outputText strings.Builder
	outputItems := map[int]json.RawMessage{}
	for {
		line, err := readLine(reader, 1<<20)
		if len(line) > 0 {
			trimmed := strings.TrimSpace(string(line))
			if strings.HasPrefix(trimmed, "data:") {
				payload := []byte(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
				var event struct {
					Type        string          `json:"type"`
					Delta       string          `json:"delta"`
					OutputIndex int             `json:"output_index"`
					Item        json.RawMessage `json:"item"`
					Response    json.RawMessage `json:"response"`
				}
				if json.Unmarshal(payload, &event) == nil {
					switch event.Type {
					case "response.output_text.delta":
						outputText.WriteString(event.Delta)
					case "response.output_item.done":
						if event.OutputIndex >= 0 && event.OutputIndex < 1024 && json.Valid(event.Item) {
							outputItems[event.OutputIndex] = bytes.Clone(event.Item)
						}
					case "response.completed":
						if len(event.Response) > 0 {
							return responseWithOutputItems(event.Response, outputItems, outputText.String()), "", events.RateLimitObservation{}, nil
						}
					case "response.failed", "response.incomplete":
						observation := rateLimitObservation(payload, time.Now().UTC())
						class := events.ErrorUpstream
						if observation.UsageLimited {
							class = events.ErrorRateLimit
						}
						return nil, class, observation, fmt.Errorf("Codex response did not complete")
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, events.ErrorProtocol, events.RateLimitObservation{}, fmt.Errorf("Codex response ended without response.completed")
			}
			return nil, classifyTransport(err), events.RateLimitObservation{}, err
		}
	}
}

func responseWithOutputText(response json.RawMessage, text string) []byte {
	return responseWithOutputItems(response, nil, text)
}

func responseWithOutputItems(response json.RawMessage, items map[int]json.RawMessage, text string) []byte {
	if strings.TrimSpace(text) == "" {
		if len(items) == 0 {
			return bytes.Clone(response)
		}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(response, &object) != nil {
		return bytes.Clone(response)
	}
	if outputHasItems(object["output"]) {
		return bytes.Clone(response)
	}
	if strings.TrimSpace(text) != "" {
		encoded, err := json.Marshal(text)
		if err != nil {
			return bytes.Clone(response)
		}
		object["output_text"] = encoded
	}
	outputItems := orderedOutputItems(items)
	if len(outputItems) == 0 {
		generated, err := json.Marshal(map[string]any{
			"id":     "msg_" + uuid.NewString(),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
			}},
		})
		if err != nil {
			return bytes.Clone(response)
		}
		outputItems = []json.RawMessage{generated}
	}
	output, err := json.Marshal(outputItems)
	if err != nil {
		return bytes.Clone(response)
	}
	object["output"] = output
	result, err := json.Marshal(object)
	if err != nil {
		return bytes.Clone(response)
	}
	return result
}

func orderedOutputItems(items map[int]json.RawMessage) []json.RawMessage {
	indices := make([]int, 0, len(items))
	for index := range items {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	result := make([]json.RawMessage, 0, len(indices))
	for _, index := range indices {
		result = append(result, bytes.Clone(items[index]))
	}
	return result
}

func outputHasItems(raw json.RawMessage) bool {
	var items []json.RawMessage
	return json.Unmarshal(raw, &items) == nil && len(items) > 0
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

func terminalEvent(line []byte) (bool, events.ErrorClass) {
	switch sseEventName(line) {
	case "response.completed":
		return true, ""
	case "response.failed", "response.incomplete":
		return true, events.ErrorUpstream
	default:
		return false, ""
	}
}

func sseEventName(line []byte) string {
	value := strings.TrimSpace(string(line))
	if !strings.HasPrefix(value, "event:") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(value, "event:"))
}

func terminalOutcome(line []byte) (bool, events.ErrorClass, events.RateLimitObservation) {
	done, class := terminalClass(line)
	if !done {
		return false, "", events.RateLimitObservation{}
	}
	observation := rateLimitObservationFromLine(line, time.Now().UTC())
	if observation.UsageLimited {
		class = events.ErrorRateLimit
	}
	return true, class, observation
}

func readRateLimitObservation(response *http.Response) (events.RateLimitObservation, int) {
	if response == nil || response.Body == nil {
		return events.RateLimitObservation{}, 0
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	observation := rateLimitObservation(body, time.Now().UTC())
	return observation, maxRetryAfter(retryAfterSeconds(response.Header), retryAfterFromObservation(observation))
}

func rateLimitObservationFromLine(line []byte, now time.Time) events.RateLimitObservation {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return events.RateLimitObservation{}
	}
	return rateLimitObservation([]byte(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))), now)
}

func rateLimitObservation(body []byte, now time.Time) events.RateLimitObservation {
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type            string          `json:"type"`
			ResetsAt        json.RawMessage `json:"resets_at"`
			ResetsInSeconds json.RawMessage `json:"resets_in_seconds"`
		} `json:"error"`
		Response struct {
			Error struct {
				Type            string          `json:"type"`
				ResetsAt        json.RawMessage `json:"resets_at"`
				ResetsInSeconds json.RawMessage `json:"resets_in_seconds"`
			} `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return events.RateLimitObservation{}
	}
	errorType, resetsAt, resetsIn := payload.Error.Type, payload.Error.ResetsAt, payload.Error.ResetsInSeconds
	if errorType == "" {
		errorType, resetsAt, resetsIn = payload.Response.Error.Type, payload.Response.Error.ResetsAt, payload.Response.Error.ResetsInSeconds
	}
	if !strings.EqualFold(strings.TrimSpace(errorType), "usage_limit_reached") {
		return events.RateLimitObservation{}
	}
	observation := events.RateLimitObservation{UsageLimited: true}
	if resetAt, ok := rawResetAt(resetsAt, now); ok {
		observation.ResetAt = resetAt.Format(time.RFC3339)
		return observation
	}
	if seconds, ok := rawPositiveInt(resetsIn); ok {
		observation.ResetAt = now.Add(time.Duration(seconds) * time.Second).UTC().Format(time.RFC3339)
	}
	return observation
}

func rawResetAt(raw json.RawMessage, now time.Time) (time.Time, bool) {
	value := strings.Trim(strings.TrimSpace(string(raw)), "\"")
	if timestamp, err := time.Parse(time.RFC3339, value); err == nil && timestamp.After(now) {
		return timestamp.UTC(), true
	}
	seconds, ok := rawPositiveInt(raw)
	if !ok {
		return time.Time{}, false
	}
	if seconds > 1_000_000_000_000 {
		seconds /= 1000
	}
	resetAt := time.Unix(seconds, 0).UTC()
	return resetAt, resetAt.After(now)
}

func rawPositiveInt(raw json.RawMessage) (int64, bool) {
	value := strings.Trim(strings.TrimSpace(string(raw)), "\"")
	seconds, err := strconv.ParseInt(value, 10, 64)
	return seconds, err == nil && seconds > 0
}

func retryAfterFromObservation(observation events.RateLimitObservation) int {
	if !observation.UsageLimited || strings.TrimSpace(observation.ResetAt) == "" {
		return 0
	}
	resetAt, err := time.Parse(time.RFC3339, observation.ResetAt)
	if err != nil || !resetAt.After(time.Now().UTC()) {
		return 0
	}
	seconds := int(time.Until(resetAt).Seconds())
	if seconds < 1 {
		return 1
	}
	if seconds > 3600 {
		return 3600
	}
	return seconds
}

func maxRetryAfter(values ...int) int {
	result := 0
	for _, value := range values {
		if value > result {
			result = value
		}
	}
	return result
}

func errorClassWithRateLimit(statusCode int, observation events.RateLimitObservation) events.ErrorClass {
	if observation.UsageLimited {
		return events.ErrorRateLimit
	}
	return classifyStatus(statusCode)
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
	if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return events.ErrorInvalidRequest
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
