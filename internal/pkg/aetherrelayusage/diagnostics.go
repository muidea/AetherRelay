package usage

import (
	"context"
	"errors"

	duckdb "github.com/duckdb/duckdb-go/v2"
	cd "github.com/muidea/magicCommon/def"
)

// QueryFailureDetails contains only fixed operation/stage names and counters.
// It deliberately excludes SQL, filter values, paths and driver messages.
type QueryFailureDetails struct {
	Operation          string
	Stage              string
	DriverCode         int
	PoolOpen           int
	PoolInUse          int
	PoolIdle           int
	PoolWaitCount      int64
	PoolWaitDurationMS int64
}

type queryFailure struct {
	details QueryFailureDetails
	cause   error
}

func (e *queryFailure) Error() string { return ErrStoreUnavailable.Error() }
func (e *queryFailure) Unwrap() []error {
	return []error{ErrStoreUnavailable, e.cause}
}

func (s *DuckDBStore) queryFailure(operation, stage string, err error) error {
	stats := s.db.Stats()
	details := QueryFailureDetails{
		Operation: operation, Stage: stage, DriverCode: -1,
		PoolOpen: stats.OpenConnections, PoolInUse: stats.InUse, PoolIdle: stats.Idle,
		PoolWaitCount: stats.WaitCount, PoolWaitDurationMS: stats.WaitDuration.Milliseconds(),
	}
	var driverErr *duckdb.Error
	if errors.As(err, &driverErr) {
		details.DriverCode = int(driverErr.Type)
	}
	return &queryFailure{details: details, cause: err}
}

func QueryFailureDetailsFromError(err error) (QueryFailureDetails, bool) {
	var failure *queryFailure
	if !errors.As(err, &failure) {
		return QueryFailureDetails{}, false
	}
	return failure.details, true
}

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
	}
	var driverErr *duckdb.Error
	if errors.As(err, &driverErr) {
		switch driverErr.Type {
		case duckdb.ErrorTypeOutOfMemory:
			return "duckdb_out_of_memory"
		case duckdb.ErrorTypeInterrupt:
			return "duckdb_interrupted"
		case duckdb.ErrorTypeIO:
			return "duckdb_io"
		case duckdb.ErrorTypeCatalog:
			return "duckdb_catalog"
		case duckdb.ErrorTypeBinder:
			return "duckdb_binder"
		case duckdb.ErrorTypeConversion, duckdb.ErrorTypeMismatchType:
			return "duckdb_conversion"
		case duckdb.ErrorTypeTransaction:
			return "duckdb_transaction"
		case duckdb.ErrorTypeInternal, duckdb.ErrorTypeFatal:
			return "duckdb_internal"
		default:
			return "duckdb_error"
		}
	}
	if details, ok := QueryFailureDetailsFromError(err); ok && details.Stage == "scan" {
		return "row_scan_failed"
	}
	if errors.Is(err, ErrStoreUnavailable) {
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
