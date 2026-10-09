package biz

import (
	"log/slog"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	codexidentity "aetherrelay/internal/pkg/aetherrelaycodexidentity"
)

func logCodexAttempt(request codexresponses.Request, failure *codexresponses.Failure) {
	// CP-OBS-006: no body, account identity or credentials; exact-model routing
	// means ingress and upstream model are equal, not the UI's pending selection.
	model := request.Model
	if len(model) > 200 {
		model = "<oversized>"
	}
	cacheKeySource := request.PromptCacheKeySource
	switch cacheKeySource {
	case codexresponses.PromptCacheKeyExplicit, codexresponses.PromptCacheKeyGenerated, codexresponses.PromptCacheKeyAbsent:
	default:
		cacheKeySource = codexresponses.PromptCacheKeyAbsent
	}
	// CP-HDR-023: the opaque turn state stays in the outbound header; only its
	// bounded provenance reaches the log.
	turnStateSource := request.TurnStateSource
	if !codexresponses.ValidTurnStateSource(turnStateSource) {
		turnStateSource = codexresponses.TurnStateSourceAbsent
	}
	clientIdentityReason := codexidentity.ObservationReason(request.Diagnostics.ClientIdentityReason)
	if !codexidentity.ValidObservationReason(clientIdentityReason) {
		clientIdentityReason = codexidentity.ObservationAbsent
	}
	attrs := []any{"request_id", request.Diagnostics.RequestID, "inbound_model", model,
		"upstream_model", model, "account_attempt", request.AccountAttempt,
		"prompt_cache_key_source", cacheKeySource, "turn_state_source", turnStateSource,
		"client_identity_reason", clientIdentityReason,
		"request_kind", request.Diagnostics.RequestKind, "compaction_reason", request.Diagnostics.CompactionReason,
		"compaction_phase", request.Diagnostics.CompactionPhase}
	if failure != nil {
		status := failure.HTTPStatus
		if failure.Attempt.Response.Observed {
			status = failure.Attempt.Response.Status
		}
		attrs = append(attrs, "error_class", string(failure.Kind), "upstream_error_code", failure.UpstreamCode, "upstream_status", status, "transport_reason", failure.Attempt.Response.TransportReason.Safe(), "queue_wait_ms", failure.Attempt.Request.QueueWaitMS, "headers_duration_ms", failure.Attempt.Response.DurationMS, "read_observed", failure.Attempt.Response.ReadObserved, "read_duration_ms", failure.Attempt.Response.ReadDurationMS, "upstream_event_count", failure.Attempt.Response.EventCount, "upstream_wire_bytes", failure.Attempt.Response.WireBytes, "last_upstream_event_at", failure.Attempt.Response.LastEventAt, "last_upstream_event_duration_ms", failure.Attempt.Response.LastEventDurationMS, "upstream_terminal_event", failure.Attempt.Response.TerminalEvent)
		slog.Warn("Codex attempt failed", attrs...)
	} else {
		slog.Debug("Codex attempt completed", attrs...)
	}
}
