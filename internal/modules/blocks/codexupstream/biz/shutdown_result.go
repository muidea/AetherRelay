package biz

import (
	"context"
	"errors"

	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

// Preserve typed attempt evidence while distinguishing an owner cancellation
// from a client cancellation. Successful results and actual upstream rejections
// keep their original meaning even if shutdown races with their delivery.
type shutdownResult struct {
	event.Result
	ctx context.Context
}

func (r shutdownResult) Set(value any, err *cd.Error) {
	if errors.Is(context.Cause(r.ctx), events.ErrServiceShutdown) {
		if err != nil {
			err = cd.WrapError(cd.InvalidOperation, events.ErrServiceShutdown, "Codex command interrupted")
		}
		class := func(c events.ErrorClass) events.ErrorClass {
			if c == events.ErrorCanceled {
				return events.ErrorServiceShutdown
			}
			return c
		}
		switch v := value.(type) {
		case events.CompleteResult:
			v.ErrorClass = class(v.ErrorClass)
			value = v
		case events.CompactResult:
			v.ErrorClass = class(v.ErrorClass)
			value = v
		case events.StartResult:
			v.ErrorClass = class(v.ErrorClass)
			value = v
		case events.PullResult:
			v.ErrorClass = class(v.ErrorClass)
			value = v
		case events.WSOpenResult:
			v.ErrorClass = class(v.ErrorClass)
			value = v
		case events.WSSendResult:
			v.ErrorClass = class(v.ErrorClass)
			value = v
		case events.WSPullResult:
			v.ErrorClass = class(v.ErrorClass)
			value = v
		case events.ListModelsResult:
			v.ErrorClass = class(v.ErrorClass)
			value = v
		case events.GetUsageResult:
			v.ErrorClass = class(v.ErrorClass)
			value = v
		}
	}
	r.Result.Set(value, err)
}
