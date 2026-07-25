// Package chatgpttext defines the proxyapi-local text execution port.
// The HTTP adapter maps OpenAI payloads to this bounded value contract; the
// proxyapi Biz is its production implementation and owns all EventHub calls.
package chatgpttext

import "context"

type Message struct {
	Role    string
	Content string
}

type Request struct {
	Model          string
	Messages       []Message
	ThinkingEffort string
}

type Result struct {
	ConversationID string
	Text           string
}

type Delta struct{ Text string }

// Executor is local to proxyapi. Implementations must not expose account
// credentials, EventHub handles, or upstream transport resources.
type Executor interface {
	Complete(context.Context, Request) (Result, error)
	Stream(context.Context, Request, func(Delta) error) (Result, error)
}
