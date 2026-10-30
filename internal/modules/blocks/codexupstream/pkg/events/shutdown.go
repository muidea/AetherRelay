package events

import "errors"

// ErrServiceShutdown is a local lifecycle cause, never upstream/account evidence.
var ErrServiceShutdown = errors.New("Codex service is shutting down")
