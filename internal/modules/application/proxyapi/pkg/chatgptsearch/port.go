// Package chatgptsearch defines the bounded ChatGPT Web forced-search port.
// It is intentionally separate from chatgpttext: search is one isolated Web
// session, not an implementation of OpenAI tool loops or persistent threads.
package chatgptsearch

import (
	"context"
	"errors"

	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptfail"
)

type Request struct {
	Model string
	Query string
}

type Source struct {
	Title   string
	URL     string
	Snippet string
}

type Result struct {
	ConversationID string
	ActualModel    string
	Text           string
	Sources        []Source
}

// Executor is owned by proxyapi Biz. It must not expose an EventHub, account
// credentials or upstream HTTP resources to the protocol adapter.
type Executor interface {
	Search(context.Context, Request) (Result, error)
}

func AsFailure(err error) (*chatgptfail.Failure, bool) {
	var f *chatgptfail.Failure
	return f, errors.As(err, &f)
}
