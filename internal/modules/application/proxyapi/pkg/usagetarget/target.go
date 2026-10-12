// Package usagetarget carries the safe execution target from local Biz to the
// usage adapter. It never contains credentials or upstream account headers.
package usagetarget

import (
	"context"
	"strings"
	"sync"
)

type Target struct {
	Provider     string
	AccountID    string
	AccountLabel string
}

type Tracker struct {
	mu     sync.Mutex
	target Target
}

type contextKey struct{}

func WithTracker(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextKey{}, &Tracker{})
}

// Record is called only when a target will execute, not when scanning candidates.
// Replacing the whole target clears an account when falling back to a provider.
func Record(ctx context.Context, provider, localAccountID, label string) {
	tracker, _ := ctx.Value(contextKey{}).(*Tracker)
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.target = Target{Provider: strings.TrimSpace(provider), AccountID: strings.TrimSpace(localAccountID), AccountLabel: strings.TrimSpace(label)}
}

func Snapshot(ctx context.Context) Target {
	tracker, _ := ctx.Value(contextKey{}).(*Tracker)
	if tracker == nil {
		return Target{}
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.target
}
