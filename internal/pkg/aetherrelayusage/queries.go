package usage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// dashboardCache 是 Dashboard 短 TTL + LRU 缓存。
type dashboardCache struct {
	ttl     time.Duration
	maxSize int
	mu      sync.Mutex
	epoch   uint64 // prevents an in-flight read from repopulating an invalidated cache
	items   map[string]*dashCacheEntry
	order   []string // 简单 LRU:尾部最新
}

type dashCacheEntry struct {
	value     Dashboard
	expiresAt time.Time
}

func newDashboardCache(seconds, maxSize int) *dashboardCache {
	if seconds <= 0 || maxSize <= 0 {
		return &dashboardCache{ttl: 0, maxSize: 0, items: make(map[string]*dashCacheEntry)}
	}
	return &dashboardCache{
		ttl:     time.Duration(seconds) * time.Second,
		maxSize: maxSize,
		items:   make(map[string]*dashCacheEntry, maxSize),
		order:   make([]string, 0, maxSize),
	}
}

func (c *dashboardCache) get(key string) (Dashboard, bool, uint64) {
	if c == nil || c.ttl <= 0 {
		return Dashboard{}, false, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return Dashboard{}, false, c.epoch
	}
	if time.Now().After(e.expiresAt) {
		delete(c.items, key)
		c.removeOrder(key)
		return Dashboard{}, false, c.epoch
	}
	// 触碰 LRU。
	c.touchOrder(key)
	return e.value, true, c.epoch
}

func (c *dashboardCache) put(key string, value Dashboard, epoch uint64) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.epoch {
		return
	}
	if _, ok := c.items[key]; !ok && len(c.items) >= c.maxSize {
		// 淘汰最旧。
		if len(c.order) > 0 {
			old := c.order[0]
			c.order = c.order[1:]
			delete(c.items, old)
		}
	}
	c.items[key] = &dashCacheEntry{value: value, expiresAt: time.Now().Add(c.ttl)}
	c.touchOrder(key)
}

func (c *dashboardCache) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	c.items = make(map[string]*dashCacheEntry)
	c.order = nil
}

func (c *dashboardCache) touchOrder(key string) {
	c.removeOrder(key)
	c.order = append(c.order, key)
}

func (c *dashboardCache) removeOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

func dashboardCacheKey(f UsageFilter) string { data, _ := json.Marshal(f); return string(data) }

// Dashboard 聚合 summary / daily / by_api_key。
func (s *DuckDBStore) Dashboard(ctx context.Context, filter UsageFilter) (Dashboard, error) {
	if s.closed.Load() {
		return Dashboard{}, ErrStoreClosed
	}
	if err := ValidateUsageFilter(&filter); err != nil {
		return Dashboard{}, err
	}
	key := dashboardCacheKey(filter)
	cached, ok, epoch := s.cache.get(key)
	if ok {
		return cached, nil
	}

	from, to := filter.From.UTC(), filter.To.UTC()
	if filter.AllTime {
		// all-time:用极宽窗口,仍走同一 SQL 模板。
		if from.IsZero() {
			from = time.Unix(0, 0).UTC()
		}
		if to.IsZero() {
			to = time.Now().UTC().Add(24 * time.Hour)
		}
	}

	where, args := buildFilterWhere(filter, from, to)

	// Keep all dashboard aggregates on one connection and MVCC snapshot.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Dashboard{}, s.queryFailure("dashboard", "begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	summary, err := s.querySummary(ctx, tx, where, args)
	if err != nil {
		return Dashboard{}, err
	}
	daily, err := s.queryDaily(ctx, tx, where, args, filter.Timezone)
	if err != nil {
		return Dashboard{}, err
	}
	byKey, err := s.queryByAPIKey(ctx, tx, where, args)
	if err != nil {
		return Dashboard{}, err
	}

	if !filter.AllTime {
		daily = fillMissingDaysIn(from, to, daily, traceLocation(filter))
	}

	out := Dashboard{
		Scope: ScopeInfo{
			From:     from,
			To:       to,
			Timezone: traceLocation(filter).String(),
		},
		Summary:  summary,
		Daily:    daily,
		ByAPIKey: byKey,
	}
	if err := tx.Commit(); err != nil {
		return Dashboard{}, s.queryFailure("dashboard", "commit", err)
	}
	s.cache.put(key, out, epoch)
	return out, nil
}

// Count 返回匹配筛选的事件数，供管理端在 CSV 导出前实施资源上限。
func (s *DuckDBStore) Count(ctx context.Context, filter UsageFilter) (int64, error) {
	if s.closed.Load() {
		return 0, ErrStoreClosed
	}
	if err := ValidateUsageFilter(&filter); err != nil {
		return 0, err
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
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM usage_events WHERE `+where, args...).Scan(&count); err != nil {
		return 0, s.queryFailure("count", "query", err)
	}
	return count, nil
}

func (s *DuckDBStore) querySummary(ctx context.Context, tx *sql.Tx, where string, args []any) (Summary, error) {
	q := `
SELECT
    count(*) AS requests,
    count(*) FILTER (WHERE outcome = 'success') AS success_requests,
    count(*) FILTER (WHERE state = 'completed' AND outcome IS DISTINCT FROM 'success') AS failed_requests,
    coalesce(sum(input_tokens), 0) AS input_tokens,
    coalesce(sum(output_tokens), 0) AS output_tokens,
    coalesce(sum(total_tokens), 0) AS total_tokens,
    coalesce(sum(input_tokens) FILTER (WHERE outcome = 'success' AND cached_input_tokens_known), 0) AS cache_input_tokens,
    coalesce(sum(cached_input_tokens) FILTER (WHERE outcome = 'success' AND cached_input_tokens_known), 0) AS cached_input_tokens,
    coalesce(sum(cache_creation_input_tokens) FILTER (WHERE outcome = 'success' AND cache_creation_input_tokens_known), 0) AS cache_creation_input_tokens,
    coalesce(bool_or(coalesce(cached_input_tokens_known, false)) FILTER (WHERE outcome = 'success'), false),
    coalesce(bool_or(coalesce(cache_creation_input_tokens_known, false)) FILTER (WHERE outcome = 'success'), false)` + tpsAggregates + `
FROM ` + tpsEvents + `
WHERE ` + where
	var sum Summary
	err := tx.QueryRowContext(ctx, q, args...).Scan(
		&sum.Requests,
		&sum.SuccessRequests,
		&sum.FailedRequests,
		&sum.InputTokens,
		&sum.OutputTokens,
		&sum.TotalTokens,
		&sum.CacheInputTokens,
		&sum.CachedInputTokens,
		&sum.CacheCreationInputTokens,
		&sum.CachedInputTokensKnown, &sum.CacheCreationInputTokensKnown,
		&sum.TPSSamples, &sum.TPSEstimatedSamples, &sum.TPSPartialSamples, &sum.TPSOutputTokens, &sum.TPSDurationMS,
		&sum.ObservedTPSSamples, &sum.ObservedTPSEstimatedSamples, &sum.ObservedTPSPartialSamples, &sum.ObservedTPSBufferedSamples, &sum.ObservedTPSOutputTokens, &sum.ObservedTPSGenerationDurationMS,
	)
	if err != nil {
		return Summary{}, s.queryFailure("summary", "query", err)
	}
	sum.TPSStats.finish()
	fillSummaryRates(&sum)
	return sum, nil
}

func (s *DuckDBStore) queryDaily(ctx context.Context, tx *sql.Tx, where string, args []any, zone string) ([]DailyBucket, error) {
	day := `cast(usage_date AS VARCHAR)`
	if zone == "Asia/Singapore" {
		day = `cast(cast(timezone('UTC',started_at)+INTERVAL '8 hours' AS DATE) AS VARCHAR)`
	}
	q := `
SELECT
    ` + day + ` AS bucket_date,
    count(*) AS requests,
    coalesce(sum(input_tokens), 0) AS input_tokens,
    coalesce(sum(output_tokens), 0) AS output_tokens,
    coalesce(sum(total_tokens), 0) AS total_tokens,
    coalesce(sum(input_tokens) FILTER (WHERE outcome = 'success' AND cached_input_tokens_known), 0) AS cache_input_tokens,
    coalesce(sum(cached_input_tokens) FILTER (WHERE outcome = 'success' AND cached_input_tokens_known), 0) AS cached_input_tokens,
    coalesce(sum(cache_creation_input_tokens) FILTER (WHERE outcome = 'success' AND cache_creation_input_tokens_known), 0) AS cache_creation_input_tokens,
    coalesce(bool_or(coalesce(cached_input_tokens_known, false)) FILTER (WHERE outcome = 'success'), false),
    coalesce(bool_or(coalesce(cache_creation_input_tokens_known, false)) FILTER (WHERE outcome = 'success'), false)` + tpsAggregates + `
FROM ` + tpsEvents + `
WHERE ` + where + `
GROUP BY bucket_date
ORDER BY bucket_date`
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, s.queryFailure("daily", "query", err)
	}
	defer rows.Close()

	var out []DailyBucket
	for rows.Next() {
		var b DailyBucket
		if err := rows.Scan(&b.Date, &b.Requests, &b.InputTokens, &b.OutputTokens, &b.TotalTokens, &b.CacheInputTokens, &b.CachedInputTokens, &b.CacheCreationInputTokens, &b.CachedInputTokensKnown, &b.CacheCreationInputTokensKnown,
			&b.TPSSamples, &b.TPSEstimatedSamples, &b.TPSPartialSamples, &b.TPSOutputTokens, &b.TPSDurationMS,
			&b.ObservedTPSSamples, &b.ObservedTPSEstimatedSamples, &b.ObservedTPSPartialSamples, &b.ObservedTPSBufferedSamples, &b.ObservedTPSOutputTokens, &b.ObservedTPSGenerationDurationMS); err != nil {
			return nil, s.queryFailure("daily", "scan", err)
		}
		b.TPSStats.finish()
		b.CacheHitRate = cacheHitRate(b.CachedInputTokens, b.CacheInputTokens)
		// 规范化日期字符串。
		if len(b.Date) > 10 {
			b.Date = b.Date[:10]
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, s.queryFailure("daily", "rows", err)
	}
	return out, nil
}

func (s *DuckDBStore) queryByAPIKey(ctx context.Context, tx *sql.Tx, where string, args []any) ([]KeySummary, error) {
	q := `
SELECT
    api_key_id,
    count(*) AS requests,
    count(*) FILTER (WHERE outcome = 'success') AS success_requests,
    count(*) FILTER (WHERE state = 'completed' AND outcome IS DISTINCT FROM 'success') AS failed_requests,
    coalesce(sum(input_tokens), 0) AS input_tokens,
    coalesce(sum(output_tokens), 0) AS output_tokens,
    coalesce(sum(total_tokens), 0) AS total_tokens,
    max(started_at) AS last_used_at,
    coalesce(sum(input_tokens) FILTER (WHERE outcome = 'success' AND cached_input_tokens_known), 0) AS cache_input_tokens,
    coalesce(sum(cached_input_tokens) FILTER (WHERE outcome = 'success' AND cached_input_tokens_known), 0) AS cached_input_tokens,
    coalesce(sum(cache_creation_input_tokens) FILTER (WHERE outcome = 'success' AND cache_creation_input_tokens_known), 0) AS cache_creation_input_tokens,
    coalesce(bool_or(coalesce(cached_input_tokens_known, false)) FILTER (WHERE outcome = 'success'), false),
    coalesce(bool_or(coalesce(cache_creation_input_tokens_known, false)) FILTER (WHERE outcome = 'success'), false)` + tpsAggregates + `
FROM ` + tpsEvents + `
WHERE ` + where + `
GROUP BY api_key_id
ORDER BY total_tokens DESC, api_key_id ASC`
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, s.queryFailure("by_api_key", "query", err)
	}
	defer rows.Close()

	var out []KeySummary
	for rows.Next() {
		var k KeySummary
		var last sql.NullTime
		if err := rows.Scan(
			&k.APIKeyID,
			&k.Requests,
			&k.SuccessRequests,
			&k.FailedRequests,
			&k.InputTokens,
			&k.OutputTokens,
			&k.TotalTokens,
			&last,
			&k.CacheInputTokens,
			&k.CachedInputTokens,
			&k.CacheCreationInputTokens,
			&k.CachedInputTokensKnown, &k.CacheCreationInputTokensKnown,
			&k.TPSSamples, &k.TPSEstimatedSamples, &k.TPSPartialSamples, &k.TPSOutputTokens, &k.TPSDurationMS,
			&k.ObservedTPSSamples, &k.ObservedTPSEstimatedSamples, &k.ObservedTPSPartialSamples, &k.ObservedTPSBufferedSamples, &k.ObservedTPSOutputTokens, &k.ObservedTPSGenerationDurationMS,
		); err != nil {
			return nil, s.queryFailure("by_api_key", "scan", err)
		}
		if last.Valid {
			t := last.Time.UTC()
			k.LastUsedAt = &t
		}
		k.TPSStats.finish()
		k.CacheHitRate = cacheHitRate(k.CachedInputTokens, k.CacheInputTokens)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, s.queryFailure("by_api_key", "rows", err)
	}
	return out, nil
}

// Events 使用 (started_at, event_id) cursor 倒序分页。
func (s *DuckDBStore) Events(ctx context.Context, filter EventFilter) (EventPage, error) {
	if s.closed.Load() {
		return EventPage{}, ErrStoreClosed
	}
	if err := ValidateEventFilter(&filter); err != nil {
		return EventPage{}, err
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

	where, args := buildFilterWhere(filter.UsageFilter, from, to)

	if len(filter.EventIDs) > 0 {
		where += ` AND event_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(filter.EventIDs)), ",") + `)`
		for _, id := range filter.EventIDs {
			args = append(args, id)
		}
	}
	// cursor: base64("rfc3339nano|event_id")
	if filter.Cursor != "" {
		curAt, curID, err := decodeEventCursor(filter.Cursor)
		if err != nil {
			return EventPage{}, fmt.Errorf("invalid cursor")
		}
		where += ` AND (started_at < ? OR (started_at = ? AND event_id < ?))`
		args = append(args, curAt, curAt, curID)
	}

	limit := filter.PageSize
	// Select only IDs before loading wide diagnostic projections. All reads
	// share a snapshot so concurrent completion/deletion cannot change the page.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EventPage{}, s.queryFailure("events", "begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	pageArgs := append(args, limit+1)
	idRows, err := tx.QueryContext(ctx, `SELECT event_id FROM usage_events WHERE `+where+` ORDER BY started_at DESC, event_id DESC LIMIT ?`, pageArgs...)
	if err != nil {
		return EventPage{}, s.queryFailure("events_page", "query", err)
	}
	var ids []string
	for idRows.Next() {
		var id string
		if err := idRows.Scan(&id); err != nil {
			_ = idRows.Close()
			return EventPage{}, s.queryFailure("events_page", "scan", err)
		}
		ids = append(ids, id)
	}
	err = idRows.Err()
	_ = idRows.Close()
	if err != nil {
		return EventPage{}, s.queryFailure("events_page", "rows", err)
	}
	whereIDs, detailArgs := eventPageIDs(ids)
	// Load only the selected page, including the extra row for has_more.
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
    coalesce(cached_input_tokens_known, false), coalesce(cache_creation_input_tokens_known, false),
    http_status, coalesce(outcome, ''), coalesce(error_code, ''),
    coalesce(failure_class, ''), retryable, retry_after_seconds,
    duration_ms, first_event_duration_ms, upstream_duration_ms,
    upstream_status, coalesce(upstream_content_type, ''), upstream_content_length, coalesce(upstream_transfer_encoding, ''),
    stream, estimated, state
FROM usage_events
WHERE ` + whereIDs + `
ORDER BY started_at DESC, event_id DESC`

	rows, err := tx.QueryContext(ctx, q, detailArgs...)
	if err != nil {
		return EventPage{}, s.queryFailure("events", "query", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var completedAt sql.NullTime
		var httpStatus sql.NullInt64
		var durationMS, firstEventMS, upstreamMS, retryAfter sql.NullInt64
		var retryable sql.NullBool
		var upstreamStatus, contentLength sql.NullInt64
		var ignored, unsupported string
		var usageDate string
		if err := rows.Scan(
			&e.EventID, &e.RoundID, &e.StartedAt, &completedAt,
			&usageDate, &e.APIKeyID,
			&e.Provider, &e.Model,
			&e.Operation, &e.Route,
			&e.ClientEndpoint, &e.ClientProtocol,
			&e.UpstreamProtocol, &e.UpstreamEndpoint,
			&e.ConversionMode,
			&e.ConversionLevel, &e.ConversionDurationMS, &e.ConversionDegraded, &ignored, &unsupported,
			&e.InputTokens, &e.OutputTokens, &e.TotalTokens,
			&e.CachedInputTokens, &e.CacheCreationInputTokens,
			&e.CachedInputTokensKnown, &e.CacheCreationInputTokensKnown,
			&httpStatus, &e.Outcome, &e.ErrorCode,
			&e.FailureClass, &retryable, &retryAfter,
			&durationMS, &firstEventMS, &upstreamMS, &upstreamStatus, &e.UpstreamContentType, &contentLength, &e.UpstreamTransferEncoding,
			&e.Stream, &e.Estimated, &e.State,
		); err != nil {
			return EventPage{}, s.queryFailure("events", "scan", err)
		}
		e.TraceState = "not_collected"
		e.OutputTokensKnown = e.OutputTokens > 0
		e.UpstreamContentLengthKnown = contentLength.Valid
		e.UpstreamContentLength = contentLength.Int64
		e.StartedAt = e.StartedAt.UTC()
		e.CacheHitRate = cacheHitRate(e.CachedInputTokens, e.InputTokens)
		if completedAt.Valid {
			t := completedAt.Time.UTC()
			e.CompletedAt = &t
		}
		if httpStatus.Valid {
			e.HTTPStatus = int(httpStatus.Int64)
		}
		if retryable.Valid {
			value := retryable.Bool
			e.Retryable = &value
		}
		if retryAfter.Valid {
			e.RetryAfterSeconds = int(retryAfter.Int64)
		}
		if durationMS.Valid {
			e.DurationMS = durationMS.Int64
		}
		if firstEventMS.Valid {
			e.FirstEventDurationMS = firstEventMS.Int64
		}
		if upstreamMS.Valid {
			e.UpstreamDurationMS = upstreamMS.Int64
		}
		if upstreamStatus.Valid {
			e.UpstreamStatus = int(upstreamStatus.Int64)
		}
		if strings.TrimSpace(ignored) != "" && ignored != "null" {
			_ = json.Unmarshal([]byte(ignored), &e.IgnoredFeatures)
		}
		if strings.TrimSpace(unsupported) != "" && unsupported != "null" {
			_ = json.Unmarshal([]byte(unsupported), &e.UnsupportedFeatures)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return EventPage{}, s.queryFailure("events", "rows", err)
	}

	if err := rows.Close(); err != nil {
		return EventPage{}, s.queryFailure("events", "rows", err)
	}
	if err := s.loadEventPageDetails(ctx, tx, ids, events); err != nil {
		return EventPage{}, err
	}
	if err := tx.Commit(); err != nil {
		return EventPage{}, s.queryFailure("events", "commit", err)
	}

	page := EventPage{}
	if len(events) > limit {
		page.Events = events[:limit]
		last := page.Events[len(page.Events)-1]
		page.NextCursor = encodeEventCursor(last.StartedAt, last.EventID)
	} else {
		page.Events = events
	}
	if page.Events == nil {
		page.Events = []Event{}
	}
	return page, nil
}

// AllTimeByKey 返回全量按 api_key_id 汇总,供 metrics bootstrap。
func (s *DuckDBStore) AllTimeByKey(ctx context.Context) (map[string]Summary, error) {
	if s.closed.Load() {
		return nil, ErrStoreClosed
	}
	q := `
SELECT
    api_key_id,
    count(*) AS requests,
    count(*) FILTER (WHERE outcome = 'success') AS success_requests,
    count(*) FILTER (WHERE state = 'completed' AND outcome IS DISTINCT FROM 'success') AS failed_requests,
    coalesce(sum(input_tokens), 0) AS input_tokens,
    coalesce(sum(output_tokens), 0) AS output_tokens,
    coalesce(sum(total_tokens), 0) AS total_tokens,
    coalesce(sum(input_tokens) FILTER (WHERE outcome = 'success' AND cached_input_tokens_known), 0) AS cache_input_tokens,
    coalesce(sum(cached_input_tokens) FILTER (WHERE outcome = 'success' AND cached_input_tokens_known), 0) AS cached_input_tokens,
    coalesce(sum(cache_creation_input_tokens) FILTER (WHERE outcome = 'success' AND cache_creation_input_tokens_known), 0) AS cache_creation_input_tokens,
    coalesce(bool_or(coalesce(cached_input_tokens_known, false)) FILTER (WHERE outcome = 'success'), false),
    coalesce(bool_or(coalesce(cache_creation_input_tokens_known, false)) FILTER (WHERE outcome = 'success'), false)` + tpsAggregates + `
FROM ` + tpsEvents + `
GROUP BY api_key_id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, s.queryFailure("all_time_by_key", "query", err)
	}
	defer rows.Close()

	out := make(map[string]Summary)
	for rows.Next() {
		var id string
		var sum Summary
		if err := rows.Scan(
			&id,
			&sum.Requests,
			&sum.SuccessRequests,
			&sum.FailedRequests,
			&sum.InputTokens,
			&sum.OutputTokens,
			&sum.TotalTokens,
			&sum.CacheInputTokens,
			&sum.CachedInputTokens,
			&sum.CacheCreationInputTokens,
			&sum.CachedInputTokensKnown, &sum.CacheCreationInputTokensKnown,
			&sum.TPSSamples, &sum.TPSEstimatedSamples, &sum.TPSPartialSamples, &sum.TPSOutputTokens, &sum.TPSDurationMS,
			&sum.ObservedTPSSamples, &sum.ObservedTPSEstimatedSamples, &sum.ObservedTPSPartialSamples, &sum.ObservedTPSBufferedSamples, &sum.ObservedTPSOutputTokens, &sum.ObservedTPSGenerationDurationMS,
		); err != nil {
			return nil, s.queryFailure("all_time_by_key", "scan", err)
		}
		sum.TPSStats.finish()
		fillSummaryRates(&sum)
		out[id] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, s.queryFailure("all_time_by_key", "rows", err)
	}
	return out, nil
}

// buildFilterWhere 构造固定模板 WHERE 与参数(不拼接用户列名)。
func buildFilterWhere(f UsageFilter, from, to time.Time) (string, []any) {
	parts := []string{"started_at >= ?", "started_at < ?"}
	args := []any{from, to}
	if f.APIKeyID != "" {
		parts = append(parts, "api_key_id = ?")
		args = append(args, f.APIKeyID)
	}
	if f.Provider != "" {
		parts = append(parts, "provider = ?")
		args = append(args, f.Provider)
	}
	if f.Model != "" {
		parts = append(parts, "model = ?")
		args = append(args, f.Model)
	}
	if f.Outcome != "" {
		parts = append(parts, "outcome = ?")
		args = append(args, f.Outcome)
	}
	if f.Estimated != nil {
		parts = append(parts, "estimated = ?")
		args = append(args, *f.Estimated)
	}
	parts, args = appendTraceFilters(parts, args, f)
	return strings.Join(parts, " AND "), args
}

func encodeEventCursor(at time.Time, eventID string) string {
	raw := at.UTC().Format(time.RFC3339Nano) + "|" + eventID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeEventCursor(cursor string) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		// 兼容标准 padding 编码。
		b, err = base64.URLEncoding.DecodeString(cursor)
		if err != nil {
			return time.Time{}, "", err
		}
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return time.Time{}, "", fmt.Errorf("bad cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		at, err = time.Parse(time.RFC3339, parts[0])
		if err != nil {
			return time.Time{}, "", err
		}
	}
	return at.UTC(), parts[1], nil
}
