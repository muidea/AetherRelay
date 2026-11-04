package proxy

import (
	"context"

	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"

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

func recordSearchCompletion(ctx context.Context, result completion.Search, started bool, fail *streamFail) {
	evidence := completion.Text{ResponseStarted: started, Completed: result.Completed && fail == nil, Reason: result.Reason, Terminal: "search_" + result.Terminal, TextBytes: result.TextBytes, TextSHA256: result.TextSHA256}
	var err error
	if fail != nil {
		err = chatgptfail.New(chatgptfail.Kind(chatGPTFailureCode(fail)), fail)
	}
	recordTextCompletion(ctx, evidence, err)
}
