// Package chatgptimage defines the Proxy API-local synchronous image port.
package chatgptimage

import (
	"context"
	"errors"
	"io"

	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/pkg/chatgpttokenusage"
)

// Request is normalized at the HTTP boundary before image orchestration.
type Request struct {
	Prompt         string
	Model          string
	Size           string
	Quality        string
	ResponseFormat string
	N              int
	Images         [][]byte
	BaseURL        string
	APIKeyID       string
}

type Data struct {
	B64JSON       string `json:"b64_json,omitempty"`
	URL           string `json:"url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// Result is the OpenAI-compatible image response plus owner-local Usage.
// Usage is never serialized to clients (json:"-").
// On error, Data/Usage may still hold already-produced partial results when
// earlier of n upstream calls succeeded.
type Result struct {
	Created        int64             `json:"created"`
	Data           []Data            `json:"data"`
	ResponseFormat string            `json:"response_format,omitempty"`
	Usage          *tokenusage.Usage `json:"-"`
	ConversationID string            `json:"-"`
	AccountID      string            `json:"-"`
}

// Executor is implemented by proxyapi Biz; HTTP adapters never receive an
// EventHub, account token, upstream client or image store.
// On error, implementations should return any already-accumulated Result and a
// *chatgptfail.Failure when classification is known.
type Executor interface {
	GenerateImage(context.Context, Request) (Result, error)
	EditImage(context.Context, Request) (Result, error)
}

var ErrContentNotFound = errors.New("image content not found")

// Content is a scoped persisted image stream. The HTTP adapter owns Reader
// after a successful open and must close it.
type Content struct {
	Reader io.ReadSeekCloser
	Name   string
}

// ContentReader exposes scoped persisted images to the HTTP delivery adapter.
// Implementations keep filesystem paths and EventHub details inside the owning
// application layer.
type ContentReader interface {
	OpenImage(context.Context, string, string) (Content, error)
}

// ResponseArchiver persists image bytes already present in a native provider
// response. Implementations must not fetch arbitrary response URLs.
type ResponseArchiver interface {
	ArchiveResponseImages(context.Context, string, []byte, string) error
}

// AsFailure extracts a typed Failure from err, if present.
func AsFailure(err error) (*chatgptfail.Failure, bool) {
	var f *chatgptfail.Failure
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}
