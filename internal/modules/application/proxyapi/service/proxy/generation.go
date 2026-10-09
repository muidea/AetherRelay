package proxy

import (
	"context"
	"time"

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
		c.generationMu.Unlock()
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
