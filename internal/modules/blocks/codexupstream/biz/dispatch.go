package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/google/uuid"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

const maxConcurrentCommands = 64

type dispatchWaitKey struct{}
type commandLifetimeKey struct{}
type commandLifetime struct {
	cancel      context.CancelFunc
	transferred bool
}

func transferCommand(ctx context.Context) {
	if life, ok := ctx.Value(commandLifetimeKey{}).(*commandLifetime); ok {
		life.transferred = true
	}
}
func cancelCommand(ctx context.Context) {
	if life, ok := ctx.Value(commandLifetimeKey{}).(*commandLifetime); ok {
		life.cancel()
	}
}

func dispatchQueueWait(ctx context.Context) time.Duration {
	wait, _ := ctx.Value(dispatchWaitKey{}).(time.Duration)
	return wait
}

// Track in-flight commands so teardown cancels blocked network reads. Control
// commands bypass admission limits and can always close an existing stream.
func (s *Upstream) commandHandler(handler event.ObserverFunc) event.ObserverFunc {
	return func(ev event.Event, result event.Result) {
		if result == nil {
			return
		}
		if ev.Context().Err() != nil {
			result.Set(nil, cd.NewError(cd.Timeout, "Codex command canceled before execution"))
			return
		}
		control := ev.ID() == events.TopicCancel || ev.ID() == events.TopicWSClose
		ctx, cancel := context.WithCancel(context.WithValue(ev.Context(), dispatchWaitKey{}, events.QueueWait(ev.Context())))
		lifetime := &commandLifetime{cancel: cancel}
		ctx = context.WithValue(ctx, commandLifetimeKey{}, lifetime)
		defer func() {
			if !lifetime.transferred {
				cancel()
			}
		}()
		id := uuid.NewString()
		s.mu.Lock()
		if s.stopping || (!control && len(s.active) >= maxConcurrentCommands) {
			s.mu.Unlock()
			result.Set(nil, cd.NewError(cd.ResourceExhausted, "Codex upstream local capacity unavailable"))
			return
		}
		if s.active == nil {
			s.active = map[string]context.CancelFunc{}
		}
		s.workWG.Add(1)
		s.active[id] = cancel
		s.mu.Unlock()
		defer func() { s.mu.Lock(); delete(s.active, id); s.mu.Unlock(); s.workWG.Done() }()
		ev.BindContext(ctx)
		handler(ev, result)
	}
}

func classifyRequestTransport(ctx context.Context, err error) events.ErrorClass {
	if ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return events.ErrorTimeout
	}
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return events.ErrorCanceled
	}
	return classifyTransport(err)
}

func classifyResponseTransport(response *http.Response, err error) events.ErrorClass {
	if response.Request != nil {
		return classifyRequestTransport(response.Request.Context(), err)
	}
	return classifyTransport(err)
}

func businessEventCount(line []byte) int64 {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return 0
	}
	payload := bytes.TrimSpace(line[5:])
	documents, repaired := splitCodexJSONDocuments(payload)
	if !repaired {
		documents = [][]byte{payload}
	}
	var count int64
	for _, document := range documents {
		var value struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(document, &value) == nil && value.Type != "" && value.Type != "keepalive" {
			count++
		}
	}
	return count
}

func (s *responseStream) observe(line []byte) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	s.progress.WireBytes += int64(len(line))
	if count := businessEventCount(line); count > 0 {
		first := s.progress.EventCount == 0
		s.progress.EventCount += count
		if first {
			s.progress.FirstEventDurationMS = time.Since(s.readStarted).Milliseconds()
		}
	}
}
func (s *responseStream) observation() events.HTTPResponseObservation {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	value := s.progress
	value.ReadObserved = !s.readStarted.IsZero()
	if !s.readStarted.IsZero() {
		finished := s.readFinished
		if finished.IsZero() {
			finished = time.Now()
		}
		value.ReadDurationMS = finished.Sub(s.readStarted).Milliseconds()
	}
	return value
}

func (s *responseStream) finish() {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	s.readFinished = time.Now()
}
