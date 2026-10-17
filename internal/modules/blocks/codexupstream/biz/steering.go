package biz

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"aetherrelay/internal/modules/blocks/codexupstream/pkg/steering"
	wsclient "github.com/bogdanfinn/websocket"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

type steeringSession struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	snapshot events.SteeringSnapshotResult
}

func (p *steeringSession) read() events.SteeringSnapshotResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	v := p.snapshot
	v.Evidence = v.Evidence.Clone()
	v.Fallback = v.Fallback.Clone()
	v.Headers = append([]events.Header(nil), v.Headers...)
	v.ClientHeaders = append([]events.Header(nil), v.ClientHeaders...)
	return v
}
func (p *steeringSession) publish(e steering.Evidence) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshot.Evidence = e.Clone()
	p.snapshot.Evidence.UpstreamRequestSent = e.UpstreamRequestSent || p.snapshot.Fallback != nil
	// Terminal publication waits for the fallback decision and connection cleanup.
	p.snapshot.Evidence.Done = false
}
func (p *steeringSession) beginFallback(e steering.Evidence, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshot.Fallback = steering.NewFallback(e, reason)
	p.snapshot.Evidence = steering.NewClientEvidence()
	p.snapshot.Evidence.UpstreamRequestSent = true
}
func (p *steeringSession) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshot.Evidence.Done = true
}

func (s *Upstream) handleSteeringStart(ev event.Event, r event.Result) {
	cmd, ok := ev.Data().(events.SteeringStartCommand)
	if !ok || cmd.ProbeID == "" || cmd.AccessToken == "" || cmd.Model == "" || cmd.Deadline.IsZero() {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid steering probe"))
		return
	}
	ctx, cancel := context.WithDeadline(context.WithoutCancel(ev.Context()), minTime(cmd.Deadline, time.Now().Add(120*time.Second)))
	p := &steeringSession{cancel: cancel, done: make(chan struct{}), snapshot: events.SteeringSnapshotResult{Evidence: steering.NewEvidence(), Endpoint: responsesWebsocketURL}}
	s.mu.Lock()
	if s.steering == nil {
		s.steering = map[string]*steeringSession{}
	}
	if s.stopping || len(s.steering) >= 2 || s.steering[cmd.ProbeID] != nil {
		s.mu.Unlock()
		cancel()
		r.Set(nil, cd.NewError(cd.ResourceExhausted, "steering probe capacity unavailable"))
		return
	}
	s.steering[cmd.ProbeID] = p
	s.workWG.Add(1)
	s.mu.Unlock()
	if err := s.AsyncTaskContext(ev.Context(), func() { defer s.workWG.Done(); defer close(p.done); defer cancel(); s.runSteering(ctx, cmd, p) }); err != nil {
		s.workWG.Done()
		cancel()
		m := steering.NewMachine()
		m.Finish("worker_unavailable")
		p.publish(m.Evidence)
		p.finish()
		close(p.done)
		r.Set(nil, cd.NewError(cd.ResourceExhausted, "steering probe worker unavailable"))
		return
	}
	r.Set(events.SteeringStartResult{ProbeID: cmd.ProbeID}, nil)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (s *Upstream) steeringSession(id string) *steeringSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.steering[id]
}
func (s *Upstream) handleSteeringSnapshot(ev event.Event, r event.Result) {
	cmd, ok := ev.Data().(events.SteeringSnapshotCommand)
	if !ok {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid steering snapshot"))
		return
	}
	p := s.steeringSession(cmd.ProbeID)
	if p == nil {
		r.Set(nil, cd.NewError(cd.NotFound, "steering probe not found"))
		return
	}
	r.Set(p.read(), nil)
}
func (s *Upstream) handleSteeringClose(ev event.Event, r event.Result) {
	cmd, ok := ev.Data().(events.SteeringCloseCommand)
	if !ok {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid steering close"))
		return
	}
	p := s.steeringSession(cmd.ProbeID)
	if p == nil {
		r.Set(events.SteeringCloseResult{}, nil)
		return
	}
	p.cancel()
	select {
	case <-p.done:
		s.mu.Lock()
		delete(s.steering, cmd.ProbeID)
		s.mu.Unlock()
		r.Set(events.SteeringCloseResult{Closed: true, Snapshot: p.read()}, nil)
	case <-ev.Context().Done():
		r.Set(nil, cd.NewError(cd.Timeout, "steering probe has not drained"))
	}
}

func (s *Upstream) runSteering(ctx context.Context, cmd events.SteeringStartCommand, p *steeringSession) {
	defer p.finish()
	var received int64
	stageCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	native := s.runSteeringCheck(stageCtx, cmd, p, false, &received)
	cancel()
	reason := steering.ClientFallbackReason(native)
	if reason == "" || ctx.Err() != nil || received >= 8<<20 {
		return
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return
	}
	// The native connection has already closed. This is a fresh synthetic
	// invocation, never a replay of production inputs or a second parallel check.
	p.beginFallback(native, reason)
	cmd.SessionHash = cmd.ClientSessionHash
	if cmd.SessionHash == "" {
		cmd.SessionHash = "steering-probe:" + cmd.ProbeID + ":client"
	}
	cmd.Fingerprint = cmd.ClientFingerprint
	stageCtx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s.runSteeringCheck(stageCtx, cmd, p, true, &received)
}

func (s *Upstream) runSteeringCheck(ctx context.Context, cmd events.SteeringStartCommand, p *steeringSession, client bool, received *int64) (result steering.Evidence) {
	m := steering.NewMachine()
	observe, writeAttempt, writeResult, reject := m.Observe, m.WriteAttempt, m.WriteResult, m.Reject
	nextKind := "response.steer"
	nextBody := func() []byte { return steering.Steer(m.ParentID) }
	if client {
		cm := steering.NewClientMachine()
		m = cm.Machine
		observe, writeAttempt, writeResult, reject = cm.Observe, cm.WriteAttempt, cm.WriteResult, cm.Reject
		nextKind = "response.create"
		nextBody = func() []byte { return steering.Continue(cmd.Model, m.ParentID) }
	}
	publish := func() { p.publish(m.Evidence) }
	m.Phase = "websocket"
	m.ExecutionOutcome = "running"
	defer func() {
		if !m.Done {
			m.Finish("protocol_error")
		}
		publish()
		result = m.Evidence.Clone()
	}()
	if ctx.Err() != nil {
		m.Finish(steeringReadError(ctx, ctx.Err()))
		m.ExecutionOutcome = "not_started"
		return
	}
	m.UpstreamRequestSent = true
	publish()
	handshake, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, opened, _ := dialResponsesWebsocket(handshake, events.WSOpenCommand{
		AccessToken: cmd.AccessToken, AccountIDHeader: cmd.AccountIDHeader, Proxy: cmd.Proxy,
		MaxMessageBytes: 1 << 20, SessionHash: cmd.SessionHash, Fingerprint: cmd.Fingerprint, ClientIdentity: cmd.ClientIdentity,
	})
	class := opened.ErrorClass
	if handshake.Err() != nil {
		class = classifyRequestTransport(handshake, handshake.Err())
	}
	cancel()
	m.HTTPStatus = opened.Attempt.Response.Status
	if m.HTTPStatus == 0 {
		m.HTTPStatus = opened.HTTPStatus
	}
	m.Trace("receive", "websocket.handshake", "")
	p.mu.Lock()
	if client {
		p.snapshot.ClientHeaders = append([]events.Header(nil), opened.Headers...)
	} else {
		p.snapshot.Headers = append([]events.Header(nil), opened.Headers...)
	}
	p.mu.Unlock()
	if conn == nil {
		m.WebsocketState = "failed"
		if m.HTTPStatus >= 400 {
			m.RecordUpstreamError(steering.UpstreamError{EventType: "websocket.handshake", Type: opened.SafeError.Type, Code: opened.SafeError.Code, Param: opened.SafeError.Param, Message: opened.SafeError.Message, Status: m.HTTPStatus})
		}
		if class == events.ErrorTimeout || class == events.ErrorCanceled || class == events.ErrorNetwork {
			m.Finish(string(class))
		} else {
			reject(opened.SafeError.Code, m.HTTPStatus)
		}
		return
	}
	defer conn.Close()
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(closed); _ = conn.Close() })
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	deadline, _ := ctx.Deadline()
	_ = conn.SetReadDeadline(deadline)
	_ = conn.SetWriteDeadline(deadline)
	m.WebsocketState = "connected"
	m.HTTPStatus = 101
	m.Phase = "create"
	body, err := applyCodexRequestBody(steering.Create(cmd.Model), codexRequestProfile{sessionHash: cmd.SessionHash, fingerprint: cmd.Fingerprint, clientIdentity: cmd.ClientIdentity})
	if err != nil {
		m.Finish("configuration_error")
		return
	}
	// Writes may have reached the server even when the client reports an error.
	writeAttempt("response.create")
	publish()
	err = conn.WriteMessage(wsclient.TextMessage, body)
	writeResult("response.create", err)
	publish()
	if err != nil {
		m.Finish(steeringReadError(ctx, err))
		return
	}
	for !m.Done {
		kind, reader, err := conn.NextReader()
		if err != nil {
			m.Finish(steeringReadError(ctx, err))
			return
		}
		if kind != wsclient.TextMessage {
			m.Finish("protocol_error")
			return
		}
		payload, err := io.ReadAll(io.LimitReader(reader, min(int64(1<<20), (8<<20)-*received)+1))
		m.ReceivedBytes += int64(len(payload))
		*received += int64(len(payload))
		if err != nil {
			m.Finish(steeringReadError(ctx, err))
			return
		}
		if len(payload) > 1<<20 || *received > 8<<20 {
			m.Finish("receive_limit")
			return
		}
		if observe(payload) {
			body := nextBody()
			if client {
				body, err = applyCodexRequestBody(body, codexRequestProfile{sessionHash: cmd.SessionHash, fingerprint: cmd.Fingerprint, clientIdentity: cmd.ClientIdentity})
				if err != nil {
					m.Finish("configuration_error")
					return
				}
			}
			writeAttempt(nextKind)
			publish()
			err = conn.WriteMessage(wsclient.TextMessage, body)
			writeResult(nextKind, err)
			if err != nil {
				m.Finish(steeringReadError(ctx, err))
				return
			}
		}
		publish()
	}
	return
}

func steeringReadError(ctx context.Context, err error) string {
	if ctx.Err() == nil && errors.Is(err, wsclient.ErrReadLimit) {
		return "receive_limit"
	}
	return string(classifyRequestTransport(ctx, err))
}
