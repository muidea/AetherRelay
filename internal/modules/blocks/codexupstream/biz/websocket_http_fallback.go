package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/google/uuid"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

// A native downstream WS may keep its framing while its upstream uses HTTP.
// Credentials and readers remain in this Block. The caller must provide a full
// validated transcript on every create after this handshake-only transition.
type httpWebsocketSession struct {
	mu        sync.Mutex
	command   events.WSOpenCommand
	handshake events.HTTPAttempt
	reason    string
	ctx       context.Context
	stream    *responseStream
	attempt   events.HTTPAttempt
	published bool
	busy      bool
}

func (s *Upstream) openHTTPWebsocketSession(ev event.Event, result event.Result, cmd events.WSOpenCommand, opened events.WSOpenResult, reason string) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ev.Context()))
	fallback := &httpWebsocketSession{command: cmd, handshake: opened.Attempt, reason: reason, ctx: ctx}
	session := &websocketSession{fallback: fallback, cancel: func() {
		cancel()
		fallback.mu.Lock()
		defer fallback.mu.Unlock()
		if fallback.stream != nil {
			fallback.stream.cancel()
		}
	}}
	id := uuid.NewString()
	s.mu.Lock()
	if s.stopping || len(s.streams)+len(s.websockets) >= maxConcurrentCommands {
		s.mu.Unlock()
		cancel()
		result.Set(nil, cd.NewError(cd.ResourceExhausted, "Codex upstream local capacity unavailable"))
		return
	}
	s.websockets[id] = session
	s.mu.Unlock()
	slog.InfoContext(ev.Context(), "Codex upstream transport fallback", "upstream_transport", "http", "transport_fallback_reason", reason, "ws_handshake_status", opened.HTTPStatus)
	opened.SessionID, opened.HTTPFallback, opened.ErrorClass = id, true, ""
	opened.Attempt.Response.FallbackReason = reason
	// Failed handshake headers must not masquerade as a successful HTTP attempt.
	opened.Headers = nil
	result.Set(opened, nil)
}

func (s *Upstream) sendHTTPWebsocketTurn(ev event.Event, result event.Result, session *websocketSession, cmd events.WSSendCommand) {
	fallback := session.fallback
	fallback.mu.Lock()
	defer fallback.mu.Unlock()
	if fallback.busy || fallback.ctx.Err() != nil || ev.Context().Err() != nil {
		result.Set(events.WSSendResult{ErrorClass: events.ErrorCanceled}, nil)
		return
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(cmd.Payload, &envelope) != nil || !fullResponsesInput(cmd.Payload) {
		result.Set(nil, cd.NewError(cd.IllegalParam, "HTTP fallback requires complete input without previous_response_id"))
		return
	}
	delete(envelope, "type")
	// HTTP Responses has no generate=false operation. A fallback session can
	// acknowledge the optional WS prewarm locally because proxyapi retains its
	// full input and reconstructs the next create. Never turn it into inference.
	if bytes.Equal(bytes.TrimSpace(envelope["generate"]), []byte("false")) {
		s.prepareHTTPFallbackWarmup(fallback, envelope)
		result.Set(events.WSSendResult{Sent: true}, nil)
		return
	}
	delete(envelope, "generate")
	envelope["stream"] = json.RawMessage("true")
	body, err := encodeCodexJSON(envelope)
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid HTTP fallback create"))
		return
	}
	open := fallback.command
	profile := codexRequestProfile{sessionHash: open.SessionHash, betaFeatures: open.BetaFeatures, responsesLite: open.ResponsesLite,
		turnState: open.TurnState, fingerprint: cmd.Fingerprint, archiveUnredacted: open.ArchiveUnredactedHeaders,
		archiveFullContent: open.ArchiveFullContent, clientIdentity: open.ClientIdentity, turnMetadata: open.TurnMetadata}
	// The command's cancellation owns this HTTP body until the stream task takes
	// over, exactly as with TopicStart. Session close also cancels the request.
	requestCtx, requestCancel := context.WithCancel(ev.Context())
	stop := context.AfterFunc(fallback.ctx, requestCancel)
	response, attempt, class, retry, err := perform(requestCtx, open.AccessToken, open.AccountIDHeader, open.Proxy, body, profile)
	attempt.Response.Transport, attempt.Response.FallbackReason = "http", fallback.reason
	attempt.Prior = []events.HTTPAttempt{fallback.handshake}
	if err != nil {
		stop()
		requestCancel()
		result.Set(events.WSSendResult{Attempt: attempt, ErrorClass: class, RetryAfterSeconds: retry}, nil)
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		errorBody, quota, retry, safe := readArchivedErrorObservation(response, &attempt, profile)
		_ = response.Body.Close()
		stop()
		requestCancel()
		result.Set(events.WSSendResult{Attempt: attempt, HTTPStatus: response.StatusCode, ErrorClass: errorClassWithBody(response.StatusCode, errorBody, quota), RetryAfterSeconds: retry, RateLimit: quota, SafeError: safe, Headers: responseHeaders(response.Header)}, nil)
		return
	}
	ctx, cancel := context.WithCancel(fallback.ctx)
	stream := &responseStream{updates: make(chan streamUpdate, 64), readStarted: time.Now(), progress: events.HTTPResponseObservation{Observation: attempt.Response.Observation}}
	stream.cancel = func() { cancel(); stop(); requestCancel(); cancelCommand(ev.Context()); _ = response.Body.Close() }
	if fallback.stream != nil {
		fallback.stream.cancel()
	}
	fallback.stream, fallback.attempt, fallback.busy = stream, attempt, true
	fallback.published = false
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		stream.cancel()
		fallback.busy = false
		result.Set(events.WSSendResult{ErrorClass: events.ErrorCanceled}, nil)
		return
	}
	s.workWG.Add(1)
	s.mu.Unlock()
	transferCommand(ev.Context())
	if err := s.BackgroundRoutine().AsyncFunction(func() { defer s.workWG.Done(); s.runStream(ctx, "", stream, response.Body, open.MaxMessageBytes) }); err != nil {
		s.workWG.Done()
		stream.cancel()
		fallback.busy = false
		result.Set(nil, cd.NewError(cd.ResourceExhausted, "Codex fallback reader unavailable"))
		return
	}
	result.Set(events.WSSendResult{Sent: true, Attempt: attempt, Headers: responseHeaders(response.Header)}, nil)
}

func (s *Upstream) prepareHTTPFallbackWarmup(fallback *httpWebsocketSession, envelope map[string]json.RawMessage) {
	if fallback.stream != nil {
		fallback.stream.cancel()
	}
	var model string
	_ = json.Unmarshal(envelope["model"], &model)
	response := map[string]any{
		"id": "resp_relay_prewarm_" + uuid.NewString(), "object": "response", "model": model,
		"status": "completed", "output": []any{},
		"usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
	}
	completed, _ := encodeCodexJSON(map[string]any{"type": "response.completed", "response": response})
	response["status"] = "in_progress"
	created, _ := encodeCodexJSON(map[string]any{"type": "response.created", "response": response})
	stream := &responseStream{cancel: func() {}, readStarted: time.Now(), updates: make(chan streamUpdate, 2)}
	for _, payload := range [][]byte{created, completed} {
		line := append(append([]byte("data: "), payload...), '\n', '\n')
		stream.observe(line)
		stream.updates <- streamUpdate{data: line}
	}
	stream.finish()
	close(stream.updates)
	fallback.stream, fallback.busy, fallback.attempt = stream, true, events.HTTPAttempt{}
	fallback.published = false
}

func (s *Upstream) pullHTTPWebsocketTurn(ev event.Event, result event.Result, session *websocketSession, cmd events.WSPullCommand) {
	fallback := session.fallback
	fallback.mu.Lock()
	stream, attempt := fallback.stream, fallback.attempt
	fallback.mu.Unlock()
	if stream == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "no active HTTP fallback response"))
		return
	}
	timeout := time.Duration(cmd.TimeoutMillis) * time.Millisecond
	if timeout <= 0 || timeout > time.Minute {
		timeout = time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case update, ok := <-stream.updates:
			progress := stream.observation()
			attempt.Response.Observation = progress.Observation
			attempt.Response.ReadObserved, attempt.Response.ReadDurationMS = progress.ReadObserved, progress.ReadDurationMS
			attempt.Response.FirstOutputAt, attempt.Response.GenerationDuration, attempt.Response.GenerationPartial = progress.FirstOutputAt, progress.GenerationDuration, progress.GenerationPartial
			attempt.Response.EventCount, attempt.Response.WireBytes = progress.EventCount, progress.WireBytes
			attempt.Response.FirstEventDurationMS, attempt.Response.LastEventAt, attempt.Response.LastEventDurationMS = progress.FirstEventDurationMS, progress.LastEventAt, progress.LastEventDurationMS
			attempt.Response.TerminalEvent = progress.TerminalEvent
			payload := bytes.TrimSpace(sseData(update.data))
			if len(payload) == 0 && !update.done && ok {
				continue
			}
			terminal := terminalEventType(update.data) != ""
			if terminal || update.done || !ok {
				fallback.mu.Lock()
				fallback.busy = false
				fallback.mu.Unlock()
				// runStream queues a final status after its terminal frame. Cancel after
				// consuming that frame so no old status can leak into the next response.
				stream.cancel()
			}
			class := update.errorClass
			quota := update.rateLimit
			if terminal {
				class, quota = websocketTerminalFailure(payload)
			}
			if !ok && !terminal && class == "" {
				class = events.ErrorProtocol
			}
			fallback.mu.Lock()
			publish := !fallback.published || terminal || update.done || !ok
			fallback.published = true
			fallback.mu.Unlock()
			if !publish {
				attempt = events.HTTPAttempt{}
			}
			result.Set(events.WSPullResult{Attempt: attempt, Payload: bytes.Clone(payload), Done: update.done || !ok, ErrorClass: class, RateLimit: quota, RetryAfterSeconds: max(update.retryAfterSeconds, retryAfterFromObservation(quota))}, nil)
			return
		case <-timer.C:
			result.Set(events.WSPullResult{}, nil)
			return
		case <-ev.Context().Done():
			result.Set(nil, cd.NewError(cd.Timeout, "Codex fallback pull canceled"))
			return
		}
	}
}
