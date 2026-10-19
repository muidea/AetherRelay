package biz

import (
	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	transport "aetherrelay/internal/pkg/aetherrelaytransport"
	"errors"
	"io"
)

func transportReason(err error) transport.Reason {
	if errors.Is(err, errSSELineLimit) {
		return transport.SSELineLimit
	}
	return transport.Classify(err)
}

// Protocol/business errors from completedResponse are not transport failures.
func completedTransportReason(class events.ErrorClass, err error) transport.Reason {
	if class == events.ErrorNetwork || class == events.ErrorTimeout || class == events.ErrorCanceled || errors.Is(err, io.EOF) || errors.Is(err, errSSELineLimit) {
		return transportReason(err)
	}
	return ""
}
