package proxy

import (
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
	"context"

	completion "aetherrelay/internal/pkg/aetherrelaycompletion"
	trace "aetherrelay/internal/pkg/aetherrelaytrace"
)

// Reuse the execution trace; do not create a second request ledger.
func recordTextCompletion(ctx context.Context, result completion.Text, err error) {
	if collector := trace.FromContext(ctx); collector != nil {
		failureClass := ""
		if err != nil {
			failureClass = string(chatgptfail.KindUpstream)
			if failure, ok := chatgpttext.AsFailure(err); ok {
				failureClass = string(failure.Kind)
			}
		}
		collector.ObserveTextCompletion(result, failureClass)
	}
}
