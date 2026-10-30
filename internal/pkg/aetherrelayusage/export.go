package usage

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"time"

	trace "aetherrelay/internal/pkg/aetherrelaytrace"
)

// csvExportHeader 是导出固定列;不含正文与任何密钥。
var csvExportHeader = []string{
	"event_id",
	"round_id",
	"started_at",
	"completed_at",
	"usage_date",
	"api_key_id",
	"provider",
	"model",
	"operation",
	"route",
	"client_endpoint",
	"client_protocol",
	"upstream_protocol",
	"upstream_endpoint",
	"conversion_mode",
	"conversion_level",
	"conversion_duration_ms",
	"conversion_degraded",
	"ignored_features",
	"unsupported_features",
	"input_tokens",
	"output_tokens",
	"total_tokens",
	"cached_input_tokens",
	"cache_creation_input_tokens",
	"http_status",
	"outcome",
	"error_code",
	"failure_class",
	"retryable",
	"retry_after_seconds",
	"duration_ms",
	"first_event_duration_ms",
	"upstream_duration_ms",
	"stream",
	"estimated",
	"state",
	"cached_input_tokens_known",
	"cache_creation_input_tokens_known",
	"first_output_at", "generation_duration_ms", "tps", "generation_partial", "generation_buffered", "observed_tps", "output_tokens_known",
	"account_id", "account_label", "upstream_request_model", "response_model", "model_substituted",
	"upstream_error_type", "upstream_error_code", "upstream_error_param", "upstream_error_message",
	"trace_state", "attempt_count", "failed_attempts", "had_anomaly", "primary_cause", "trace_transport", "trace_revision", "trace_request_id",
}

// ExportCSV 按筛选条件流式写出 CSV。
func (s *DuckDBStore) ExportCSV(ctx context.Context, filter UsageFilter, w io.Writer) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	if err := ValidateUsageFilter(&filter); err != nil {
		return err
	}
	from, to := filter.From.UTC(), filter.To.UTC()
	if filter.AllTime {
		if from.IsZero() {
			from = time.Unix(0, 0).UTC()
		}
		if to.IsZero() {
			to = time.Now().UTC().Add(24 * time.Hour)
		}
	}

	where, args := buildFilterWhere(filter, from, to)
	q := `
SELECT
    event_id, coalesce(round_id, 0), started_at, completed_at,
    cast(usage_date AS VARCHAR), api_key_id,
    coalesce(provider, ''), coalesce(model, ''),
    coalesce(operation, ''), coalesce(route, ''),
    coalesce(client_endpoint, ''), coalesce(client_protocol, ''),
    coalesce(upstream_protocol, ''), coalesce(upstream_endpoint, ''),
    coalesce(conversion_mode, ''),
    coalesce(conversion_level, 0), coalesce(conversion_duration_ms, 0),
    coalesce(conversion_degraded, false), coalesce(ignored_features, ''), coalesce(unsupported_features, ''),
    input_tokens, output_tokens, total_tokens,
    cached_input_tokens, cache_creation_input_tokens,
    http_status, coalesce(outcome, ''), coalesce(error_code, ''),
    coalesce(failure_class, ''), retryable, retry_after_seconds,
    duration_ms, first_event_duration_ms, upstream_duration_ms,
    stream, estimated, state,
    coalesce(cached_input_tokens_known, false), coalesce(cache_creation_input_tokens_known, false), first_output_at, generation_duration_ns, coalesce(generation_partial, false), generation_buffered, output_tokens_known, account_id, account_label, upstream_request_model, response_model, model_substituted, upstream_error_type, upstream_error_code, upstream_error_param, upstream_error_message, trace_state,attempt_count,failed_attempts,had_anomaly,primary_cause,trace_transport,trace_revision,trace_request_id
FROM ` + anomalyEventSource + `
WHERE ` + where + `
ORDER BY started_at ASC, event_id ASC LIMIT 10001`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return ErrStoreUnavailable
	}
	defer rows.Close()

	cw := csv.NewWriter(w)
	if err := cw.Write(csvExportHeader); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}

	count := 0
	for rows.Next() {
		count++
		if count > 10000 {
			return fmt.Errorf("export limited to 10000 events")
		}
		var traceSummary trace.Summary
		var (
			eventID, apiKeyID, provider, model, operation, route string
			clientEndpoint, clientProtocol                       string
			upstreamProtocol, upstreamEndpoint, conversionMode   string
			ignoredFeatures, unsupportedFeatures                 string
			outcome, errorCode, failureClass, state, usageDate   string
			roundID                                              int64
			inputTok, outputTok, totalTok                        int64
			cachedIn, cacheCreate                                int64
			stream, estimated                                    bool
			startedAt                                            time.Time
			completedAt                                          sql.NullTime
			httpStatus, durationMS, firstEventMS, upstreamMS     sql.NullInt64
			retryAfter                                           sql.NullInt64
			retryable                                            sql.NullBool
			conversionLevel, conversionDurationMS                int64
			conversionDegraded                                   bool
			cachedKnown, creationKnown                           bool
		)
		var accountID, accountLabel, upstreamRequestModel, responseModel string
		var modelSubstituted bool
		var upstreamErrorType, upstreamErrorCode, upstreamErrorParam, upstreamErrorMessage string
		var firstOutputAt sql.NullTime
		var generationNS sql.NullInt64
		var partial, buffered, outputKnown bool
		if err := rows.Scan(
			&eventID, &roundID, &startedAt, &completedAt,
			&usageDate, &apiKeyID,
			&provider, &model,
			&operation, &route,
			&clientEndpoint, &clientProtocol,
			&upstreamProtocol, &upstreamEndpoint,
			&conversionMode,
			&conversionLevel, &conversionDurationMS, &conversionDegraded, &ignoredFeatures, &unsupportedFeatures,
			&inputTok, &outputTok, &totalTok,
			&cachedIn, &cacheCreate,
			&httpStatus, &outcome, &errorCode,
			&failureClass, &retryable, &retryAfter,
			&durationMS, &firstEventMS, &upstreamMS,
			&stream, &estimated, &state,
			&cachedKnown, &creationKnown, &firstOutputAt, &generationNS, &partial, &buffered, &outputKnown, &accountID, &accountLabel, &upstreamRequestModel, &responseModel, &modelSubstituted, &upstreamErrorType, &upstreamErrorCode, &upstreamErrorParam, &upstreamErrorMessage,
			&traceSummary.TraceState, &traceSummary.AttemptCount, &traceSummary.FailedAttempts, &traceSummary.HadAnomaly, &traceSummary.PrimaryCause, &traceSummary.TraceTransport, &traceSummary.TraceRevision, &traceSummary.TraceRequestID,
		); err != nil {
			return ErrStoreUnavailable
		}
		if len(usageDate) > 10 {
			usageDate = usageDate[:10]
		}
		completed := ""
		if completedAt.Valid {
			completed = completedAt.Time.UTC().Format(time.RFC3339Nano)
		}
		httpS := ""
		if httpStatus.Valid {
			httpS = strconv.FormatInt(httpStatus.Int64, 10)
		}
		durS := ""
		if durationMS.Valid {
			durS = strconv.FormatInt(durationMS.Int64, 10)
		}
		upDurS := ""
		if upstreamMS.Valid {
			upDurS = strconv.FormatInt(upstreamMS.Int64, 10)
		}
		firstEventS := ""
		if firstEventMS.Valid {
			firstEventS = strconv.FormatInt(firstEventMS.Int64, 10)
		}
		retryableS := ""
		if retryable.Valid {
			retryableS = strconv.FormatBool(retryable.Bool)
		}
		retryAfterS := ""
		if retryAfter.Valid {
			retryAfterS = strconv.FormatInt(retryAfter.Int64, 10)
		}
		row := []string{
			eventID,
			strconv.FormatInt(roundID, 10),
			startedAt.UTC().Format(time.RFC3339Nano),
			completed,
			usageDate,
			apiKeyID,
			provider,
			model,
			operation,
			route,
			clientEndpoint,
			clientProtocol,
			upstreamProtocol,
			upstreamEndpoint,
			conversionMode,
			strconv.FormatInt(conversionLevel, 10),
			strconv.FormatInt(conversionDurationMS, 10),
			strconv.FormatBool(conversionDegraded),
			ignoredFeatures,
			unsupportedFeatures,
			strconv.FormatInt(inputTok, 10),
			strconv.FormatInt(outputTok, 10),
			strconv.FormatInt(totalTok, 10),
			strconv.FormatInt(cachedIn, 10),
			strconv.FormatInt(cacheCreate, 10),
			httpS,
			outcome,
			errorCode,
			failureClass,
			retryableS,
			retryAfterS,
			durS,
			firstEventS,
			upDurS,
			strconv.FormatBool(stream),
			strconv.FormatBool(estimated),
			state,
			strconv.FormatBool(cachedKnown),
			strconv.FormatBool(creationKnown),
		}
		sample := GenerationSample{GenerationBuffered: buffered}
		if firstOutputAt.Valid && generationNS.Valid {
			sample = makeGenerationSample(firstOutputAt.Time.UTC(), generationNS.Int64, outputTok, partial, buffered, outputKnown)
		}
		sample.OutputTokensKnown = outputKnown
		sample.TPS = outputRate(outputTok, float64(durationMS.Int64)/1000, state == StateCompleted && outputKnown)
		row = append(row, tpsCSV(sample)...)
		row = append(row, accountID, accountLabel, upstreamRequestModel, responseModel, strconv.FormatBool(modelSubstituted))
		row = append(row, upstreamErrorType, upstreamErrorCode, upstreamErrorParam, upstreamErrorMessage)
		row = append(row, traceCSV(traceSummary)...)
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return ErrStoreUnavailable
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("flush csv: %w", err)
	}
	return nil
}

func traceCSV(s trace.Summary) []string {
	state := s.TraceState
	if state == "" {
		state = "not_collected"
	}
	count, failed := "", ""
	if state != "not_collected" {
		count = strconv.Itoa(s.AttemptCount)
		failed = strconv.Itoa(s.FailedAttempts)
	}
	return []string{state, count, failed, strconv.FormatBool(s.HadAnomaly), s.PrimaryCause, s.TraceTransport, s.TraceRevision, s.TraceRequestID}
}
