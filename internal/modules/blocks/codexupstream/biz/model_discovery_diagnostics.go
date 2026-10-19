package biz

import transport "aetherrelay/internal/pkg/aetherrelaytransport"

// Adds structured diagnostics while preserving the original failure.
type modelListError struct {
	status int
	reason transport.Reason
	cause  error
}

func (e *modelListError) Error() string { return e.cause.Error() }
func (e *modelListError) Unwrap() error { return e.cause }
