package biz

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
)

// CP-STREAM-013: a request-owned timer guards silence, not cumulative output
// time. No timer or reader outlives the attempt that owns it.
type codexStreamGuard struct {
	mu                             sync.Mutex
	cancel                         context.CancelCauseFunc
	silence, lifetime              *time.Timer
	deadline                       time.Time
	idle                           time.Duration
	kind                           codexresponses.ErrorKind
	closed                         bool
	started, firstEvent, lastEvent time.Time
	events, bytes                  int64
}

const firstEventTimeoutRetryAfter = 5

func newCodexStreamGuard(parent context.Context, first, idle, maximum time.Duration) (context.Context, *codexStreamGuard) {
	ctx, cancel := context.WithCancelCause(parent)
	g := &codexStreamGuard{cancel: cancel, idle: idle, kind: codexresponses.KindFirstEventTimeout, started: time.Now()}
	g.mu.Lock()
	defer g.mu.Unlock()
	if first > 0 {
		g.deadline = g.started.Add(first)
		g.silence = time.AfterFunc(first, g.expireSilence)
	}
	if maximum > 0 {
		g.lifetime = time.AfterFunc(maximum, func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if !g.closed {
				g.fail(codexresponses.KindStreamLifetime)
			}
		})
	}
	return ctx, g
}

func (g *codexStreamGuard) fail(kind codexresponses.ErrorKind) {
	retryAfter := 0
	if kind == codexresponses.KindFirstEventTimeout {
		retryAfter = firstEventTimeoutRetryAfter
	}
	g.cancel(codexresponses.NewFailure(kind, retryAfter, fmt.Errorf("Codex stream %s", kind)))
}

func (g *codexStreamGuard) expireSilence() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.deadline.IsZero() {
		return
	}
	// A Reset can race with an already scheduled callback. Recheck the current
	// deadline while holding the same lock as observe, rather than canceling a
	// stream that just made progress.
	if remaining := time.Until(g.deadline); remaining > 0 {
		g.silence.Reset(remaining)
		return
	}
	g.fail(g.kind)
}

// observe returns true only for the first business SSE data line. Comments and
// blank lines are neither client-visible progress nor timeout renewal.
func (g *codexStreamGuard) observe(data []byte) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.bytes += int64(len(data))
	line := bytes.TrimSpace(data)
	if !bytes.HasPrefix(line, []byte("data:")) || len(bytes.TrimSpace(line[5:])) == 0 {
		return false
	}
	first := g.events == 0
	g.events++
	observedAt := time.Now()
	if first {
		g.firstEvent = observedAt
	}
	g.lastEvent = observedAt
	g.kind = codexresponses.KindIdleTimeout
	if g.idle <= 0 {
		g.deadline = time.Time{}
		if g.silence != nil {
			g.silence.Stop()
		}
		return first
	}
	g.deadline = g.lastEvent.Add(g.idle)
	if g.silence == nil {
		g.silence = time.AfterFunc(g.idle, g.expireSilence)
	} else {
		g.silence.Reset(g.idle)
	}
	return first
}

func (g *codexStreamGuard) firstEventDuration() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.firstEvent.IsZero() {
		return 0
	}
	return g.firstEvent.Sub(g.started)
}

func (g *codexStreamGuard) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	if g.silence != nil {
		g.silence.Stop()
	}
	if g.lifetime != nil {
		g.lifetime.Stop()
	}
	g.cancel(context.Canceled)
}

func logCodexStreamAttempt(request codexresponses.Request, failure *codexresponses.Failure, phase string, g *codexStreamGuard) {
	logCodexAttempt(request, failure)
	if failure == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// CP-OBS-008: bounded phase/class and counters only, never raw transport
	// errors, credentials, or generated tool arguments.
	totalDurationMS := time.Since(g.started).Milliseconds()
	slog.Warn("Codex stream stopped", "request_id", request.Diagnostics.RequestID,
		"account_attempt", request.AccountAttempt, "phase", phase, "error_class", failure.Kind,
		"duration_ms", totalDurationMS,
		"first_event_duration_ms", durationMilliseconds(g.started, g.firstEvent),
		"total_duration_ms", totalDurationMS, "event_count", g.events,
		"stream_bytes", g.bytes, "last_event_at", g.lastEvent)
}

func durationMilliseconds(started, ended time.Time) int64 {
	if ended.IsZero() {
		return 0
	}
	return ended.Sub(started).Milliseconds()
}
