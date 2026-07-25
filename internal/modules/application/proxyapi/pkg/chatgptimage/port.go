// Package chatgptimage defines the Proxy API-local synchronous image port.
package chatgptimage

import "context"

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
}

type Data struct {
	B64JSON       string `json:"b64_json,omitempty"`
	URL           string `json:"url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

type Result struct {
	Created int64  `json:"created"`
	Data    []Data `json:"data"`
}

// Executor is implemented by proxyapi Biz; HTTP adapters never receive an
// EventHub, account token, upstream client or image store.
type Executor interface {
	GenerateImage(context.Context, Request) (Result, error)
	EditImage(context.Context, Request) (Result, error)
}
