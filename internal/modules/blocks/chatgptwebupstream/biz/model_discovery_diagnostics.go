package biz

import (
	upclient "aetherrelay/internal/modules/blocks/chatgptwebupstream/internal/client"
	"aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	transport "aetherrelay/internal/pkg/aetherrelaytransport"
	"errors"
)

func modelDiscoveryFailure(err error) events.ListModelsResult {
	result := events.ListModelsResult{ErrorClass: classifyError(err)}
	var failure *upclient.Error
	if errors.As(err, &failure) && failure != nil {
		result.HTTPStatus = failure.StatusCode
		result.TransportReason = transport.Classify(failure.Cause)
	}
	return result
}
