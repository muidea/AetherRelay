// Package chatgpttext defines the proxyapi-local text execution port.
// The HTTP adapter maps OpenAI payloads to this bounded value contract; the
// proxyapi Biz is its production implementation and owns all EventHub calls.
package chatgpttext

import (
	"context"
	"errors"

	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptfail"
	"ai-proxy/internal/pkg/chatattachment"
)

type Message struct {
	Role    string
	Content string
	// Images contains validated, decoded image bytes from OpenAI-compatible
	// image_url data-URI parts. The HTTP boundary never fetches remote URLs.
	Images [][]byte
	Files  []chatattachment.File
}

type Request struct {
	Model          string
	Messages       []Message
	ThinkingEffort string
}

// Result is the bounded final or partial text projection. On failure, Text and
// ActualModel may still be populated when the upstream produced useful partial
// state before the terminal error.
type Result struct {
	ConversationID string
	ActualModel    string
	Text           string
}

type Delta struct {
	Text        string
	ActualModel string
}

// Executor is local to proxyapi. Implementations must not expose account
// credentials, EventHub handles, or upstream transport resources.
// On error, implementations should still return any partial Result they have
// and wrap the cause as *chatgptfail.Failure when classification is known.
type Executor interface {
	Complete(context.Context, Request) (Result, error)
	Stream(context.Context, Request, func(Delta) error) (Result, error)
}

// AsFailure extracts a typed Failure from err, if present.
func AsFailure(err error) (*chatgptfail.Failure, bool) {
	var f *chatgptfail.Failure
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}
