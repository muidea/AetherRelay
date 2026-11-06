package usage

import (
	"context"
	"errors"

	cd "github.com/muidea/magicCommon/def"
)

// FailureReason is safe for logs: driver errors can contain SQL and values.
func FailureReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrStoreClosed):
		return "store_closed"
	case errors.Is(err, ErrStoreUnavailable):
		return "store_unavailable"
	}
	var commandErr *cd.Error
	if errors.As(err, &commandErr) {
		switch commandErr.Message {
		case "event admission timed out or was canceled":
			return "command_admission_timeout"
		case "event canceled before dispatch", "event context expired before dispatch", "event context is canceled":
			return "command_canceled_before_dispatch"
		}
	}
	return "operation_failed"
}
