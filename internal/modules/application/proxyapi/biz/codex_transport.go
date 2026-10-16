package biz

import "aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"

// A socket read/write failure after response.create may hide a running or
// completed generation. This prohibition also reaches provider failover via
// the typed failure, even when no downstream business event was delivered.
func preventUncertainWebsocketReplay(failure *codexresponses.Failure) {
	if failure == nil || !failure.Attempt.Response.WebsocketRequestSent {
		return
	}
	switch failure.Kind {
	case codexresponses.KindInvalidToken, codexresponses.KindRateLimit, codexresponses.KindModelNotFound, codexresponses.KindInvalidRequest:
		// Explicit upstream rejection retains its existing admission/retry rules.
		return
	}
	retryable := false
	failure.Retryable = &retryable
}
