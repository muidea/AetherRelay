package biz

import (
	"log/slog"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
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
	attrs := []any{"request_id", request.Diagnostics.RequestID, "inbound_model", model,
		"upstream_model", model, "account_attempt", request.AccountAttempt,
		"prompt_cache_key_source", cacheKeySource,
		"request_kind", request.Diagnostics.RequestKind, "compaction_reason", request.Diagnostics.CompactionReason,
		"compaction_phase", request.Diagnostics.CompactionPhase}
	if failure != nil {
		attrs = append(attrs, "error_class", string(failure.Kind), "upstream_error_code", failure.UpstreamCode, "upstream_status", failure.HTTPStatus)
		slog.Warn("Codex attempt failed", attrs...)
	} else {
		slog.Debug("Codex attempt completed", attrs...)
	}
}
