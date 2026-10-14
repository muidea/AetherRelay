// Package usagetarget carries the safe execution target from local Biz to the
// usage adapter. It never contains credentials or upstream account headers.
package usagetarget

import (
	"context"
	"strings"
	"sync"

	codexmodel "aetherrelay/internal/pkg/aetherrelaycodexmodel"
)

type Target struct {
	codexmodel.Observation
	Provider     string
	AccountID    string
	AccountLabel string
}

type Tracker struct {
	mu     sync.Mutex
	target Target
	model  codexmodel.Observer
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
	tracker.model = codexmodel.Observer{}
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

// BeginCodexAttempt also resets evidence for retries using the same account.
func BeginCodexAttempt(ctx context.Context, body []byte) {
	tracker, _ := ctx.Value(contextKey{}).(*Tracker)
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.target.Provider != "codexoauth" {
		return
	}
	tracker.model = codexmodel.Observer{}
	tracker.target.Observation = codexmodel.New(codexmodel.RequestModel(body), "")
}

func ObserveCodex(ctx context.Context, payload []byte) {
	tracker, _ := ctx.Value(contextKey{}).(*Tracker)
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.target.Provider != "codexoauth" {
		return
	}
	tracker.model.Observe(payload)
	tracker.target.Observation = codexmodel.New(tracker.target.UpstreamRequestModel, tracker.model.Model)
}

// SetCodexModels accepts the upstream owner's final wire/model projection.
func SetCodexModels(ctx context.Context, observation codexmodel.Observation) {
	tracker, _ := ctx.Value(contextKey{}).(*Tracker)
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.target.Provider == "codexoauth" {
		tracker.target.Observation = codexmodel.New(observation.UpstreamRequestModel, observation.ResponseModel)
	}
}
