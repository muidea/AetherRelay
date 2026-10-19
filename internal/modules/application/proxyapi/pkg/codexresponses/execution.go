package codexresponses

import "context"

// ExecutionIdentity correlates a request/WS turn with its account slot.
// Values are diagnostics only and never affect account selection.
type ExecutionIdentity struct {
	RequestID, EventID, APIKeyID, Protocol, Endpoint string
}

type executionKey struct{}

func WithExecutionIdentity(ctx context.Context, identity ExecutionIdentity) context.Context {
	return context.WithValue(ctx, executionKey{}, identity)
}

func ExecutionIdentityFromContext(ctx context.Context) ExecutionIdentity {
	if ctx == nil {
		return ExecutionIdentity{}
	}
	identity, _ := ctx.Value(executionKey{}).(ExecutionIdentity)
	return identity
}
