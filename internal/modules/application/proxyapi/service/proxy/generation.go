package proxy

import (
	"context"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	generation "aetherrelay/internal/pkg/aetherrelaygeneration"
)

func observeGenerationSSE(ctx context.Context, line []byte, err error) {
	if ctx == nil {
		return
	}
	if c := usageCompletionFromContext(ctx); c != nil {
		at := time.Now()
		c.generation.ObserveSSE(line, at)
		if err != nil {
			c.generation.Stop(at)
		}
	}
}
func resetGeneration(ctx context.Context) {
	if c := usageCompletionFromContext(ctx); c != nil {
		c.generation.Reset()
		c.generationMu.Lock()
		c.generationObserved = generation.Sample{}
		c.generationAttemptAt = time.Time{}
		c.generationAttemptKey = ""
		c.generationAttemptFailed = false
		c.generationMu.Unlock()
	}
}

// Header-only callbacks must not erase reader progress. A genuinely new
// attempt resets the sample, while stale attempts and late success callbacks
// after a terminal failure are ignored even when interaction archival is off.
func recordCodexGeneration(ctx context.Context, attempt codexresponses.HTTPAttempt, attemptErr error) {
	c := usageCompletionFromContext(ctx)
	if c == nil {
		return
	}
	c.generationMu.Lock()
	defer c.generationMu.Unlock()
	at := attempt.Request.At
	key := at.Format(time.RFC3339Nano) + " " + attempt.Request.Method + " " + attempt.Request.URL
	if at.Before(c.generationAttemptAt) {
		return
	}
	if key != c.generationAttemptKey {
		c.generationObserved = generation.Sample{}
		c.generationAttemptAt, c.generationAttemptKey = at, key
		c.generationAttemptFailed = false
	} else if c.generationAttemptFailed && attemptErr == nil {
		return
	}
	c.generationAttemptFailed = c.generationAttemptFailed || attemptErr != nil
	if !attempt.Response.FirstOutputAt.IsZero() {
		c.generationObserved = generation.Sample{FirstOutputAt: attempt.Response.FirstOutputAt, Duration: attempt.Response.GenerationDuration, Partial: attempt.Response.GenerationPartial, Buffered: c.generationObserved.Buffered || attempt.Response.GenerationBuffered}
	} else {
		c.generationObserved.Buffered = c.generationObserved.Buffered || attempt.Response.GenerationBuffered
	}
}
func stopGeneration(ctx context.Context) {
	if ctx == nil {
		return
	}
	if c := usageCompletionFromContext(ctx); c != nil {
		c.generation.Stop(time.Now())
	}
}

func recordGeneration(ctx context.Context, sample generation.Sample) {
	if ctx == nil {
		return
	}
	if c := usageCompletionFromContext(ctx); c != nil {
		c.generationMu.Lock()
		c.generationObserved = sample
		c.generationMu.Unlock()
	}
}
