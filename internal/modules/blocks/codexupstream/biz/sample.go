package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sync"
	"time"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

type limitedSampleBody struct {
	io.Reader
	io.Closer
}

type sampleSession struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	snapshot events.SampleSnapshotResult
}

func (p *sampleSession) read() events.SampleSnapshotResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshot.Clone()
}
func (s *Upstream) handleSampleStart(ev event.Event, r event.Result) {
	if r == nil {
		return
	}

	cmd, ok := ev.Data().(events.SampleStartCommand)
	if !ok || cmd.ProbeID == "" || cmd.Deadline.IsZero() || cmd.Request.AccessToken == "" || len(cmd.Request.Body) == 0 || len(cmd.Request.Body) > 16384 {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid sample request"))
		return
	}
	ctx, cancel := context.WithDeadline(context.WithoutCancel(ev.Context()), minTime(cmd.Deadline, time.Now().Add(60*time.Second)))
	p := &sampleSession{cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	if s.samples == nil {
		s.samples = map[string]*sampleSession{}
	}
	if s.stopping || len(s.samples) >= 1 {
		s.mu.Unlock()
		cancel()
		r.Set(nil, cd.NewError(cd.ResourceExhausted, "sample capacity unavailable"))
		return
	}
	s.samples[cmd.ProbeID] = p
	s.workWG.Add(1)
	s.mu.Unlock()
	// Always HTTP/SSE, bounded completion, credential-redacted observations.
	cmd.Request.PreferWebsocket = false
	cmd.Request.MaxResponseBytes = 1 << 20
	cmd.Request.ArchiveUnredactedHeaders = false
	cmd.Request.ArchiveFullContent = true
	err := s.AsyncTaskContext(ev.Context(), func() {
		defer s.workWG.Done()
		defer close(p.done)
		defer cancel()
		result := event.NewResult(events.TopicComplete, s.ID(), s.ID())
		s.completeBounded(event.NewEventWithContext(events.TopicComplete, s.ID(), s.ID(), event.NewHeader(), ctx, cmd.Request), result, 1<<20)
		value, err := result.Get()
		complete, ok := value.(events.CompleteResult)
		if len(complete.Body) > 1<<20 {
			complete.Body = nil
			complete.ErrorClass = events.ErrorProtocol
		}
		snap := events.SampleSnapshotResult{
			Done: true, Body: complete.Body, Headers: complete.Headers, HTTPStatus: complete.HTTPStatus, ErrorClass: complete.ErrorClass,
			RequestStartedAt: complete.Attempt.Request.At, ResponseReceivedAt: complete.Attempt.Response.At,
		}
		if snap.HTTPStatus == 0 {
			snap.HTTPStatus = complete.Attempt.Response.Status
		}
		if err != nil || !ok {
			snap.ErrorClass = events.ErrorProtocol
		}
		if len(complete.Attempt.Request.Body) > 0 {
			sum := sha256.Sum256(complete.Attempt.Request.Body)
			snap.EffectiveRequestSHA256 = hex.EncodeToString(sum[:])
		}
		p.mu.Lock()
		p.snapshot = snap
		p.mu.Unlock()
	})
	if err != nil {
		s.workWG.Done()
		cancel()
		p.mu.Lock()
		p.snapshot = events.SampleSnapshotResult{Done: true, ErrorClass: events.ErrorUpstream}
		p.mu.Unlock()
		close(p.done)
		r.Set(nil, cd.NewError(cd.ResourceExhausted, "sample worker unavailable"))
		return
	}
	r.Set(events.SampleStartResult{ProbeID: cmd.ProbeID}, nil)
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func (s *Upstream) sample(id string) *sampleSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.samples[id]
}
func (s *Upstream) handleSampleSnapshot(ev event.Event, r event.Result) {
	if r == nil {
		return
	}

	cmd, ok := ev.Data().(events.SampleSnapshotCommand)
	if !ok {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid sample query"))
		return
	}
	p := s.sample(cmd.ProbeID)
	if p == nil {
		r.Set(nil, cd.NewError(cd.NotFound, "sample not found"))
		return
	}
	r.Set(p.read(), nil)
}
func (s *Upstream) handleSampleClose(ev event.Event, r event.Result) {
	if r == nil {
		return
	}

	cmd, ok := ev.Data().(events.SampleCloseCommand)
	if !ok {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid sample close"))
		return
	}
	p := s.sample(cmd.ProbeID)
	if p == nil {
		r.Set(events.SampleCloseResult{Closed: true}, nil)
		return
	}
	p.cancel()
	select {
	case <-p.done:
		s.mu.Lock()
		delete(s.samples, cmd.ProbeID)
		s.mu.Unlock()
		r.Set(events.SampleCloseResult{Closed: true, Snapshot: p.read()}, nil)
	case <-ev.Context().Done():
		r.Set(nil, cd.NewError(cd.Timeout, "sample has not drained"))
	}
}
