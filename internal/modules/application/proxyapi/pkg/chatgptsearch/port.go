// Package chatgptsearch defines the bounded ChatGPT Web forced-search port.
// It is intentionally separate from chatgpttext: search is one isolated Web
// session, not an implementation of OpenAI tool loops or persistent threads.
package chatgptsearch

import (
	"context"
	"errors"

	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	completion "aetherrelay/internal/pkg/aetherrelaycompletion"
)

const MaxQueryBytes = 64 << 10

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
	Completion     completion.Search
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

// ValidateCompletion also guards custom executors and owner contract changes.
// Known model truncation remains an incomplete result for Responses; callers
// that require a complete answer must reject it explicitly.
func ValidateCompletion(result *Result, err error) error {
	if err != nil {
		return err
	}
	if result.Completion.Valid(result.Text) || result.Completion.Incomplete(result.Text) {
		return nil
	}
	result.Completion.Completed = false
	result.Completion.Reason = completion.InvalidResult
	return chatgptfail.New(chatgptfail.KindTextIncomplete, errors.New("search completion evidence missing or inconsistent"))
}
