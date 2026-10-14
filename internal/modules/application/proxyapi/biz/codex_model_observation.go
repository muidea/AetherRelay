package biz

import (
	"context"
	"log/slog"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/usagetarget"
	codexmodel "aetherrelay/internal/pkg/aetherrelaycodexmodel"
)

var codexModelWarnings = &codexmodel.WarningLimiter{}

func logCodexModelSubstitution(ctx context.Context, requestID string, attempt int) {
	target := usagetarget.Snapshot(ctx)
	if target.Provider != "codexoauth" || !target.ModelSubstituted || !codexModelWarnings.Allow(target.AccountID, target.UpstreamRequestModel, target.ResponseModel, time.Now()) {
		return
	}
	// Only owner-projected local slot/email labels, never upstream credential headers.
	slog.Warn("Codex upstream model substituted", "request_id", requestID, "account_attempt", attempt,
		"provider", target.Provider, "account_id", target.AccountID, "account_label", target.AccountLabel,
		"upstream_request_model", target.UpstreamRequestModel, "response_model", target.ResponseModel)
}
