package biz

import (
	"context"
	"fmt"

	"ai-proxy/internal/modules/application/adminapi/pkg/codexmanagement"
	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	common "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/common"
	events "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func (s *Admin) ListCodexAccounts(ctx context.Context) ([]events.AccountView, error) {
	value, err := s.SendEvent(event.NewEventWithContext(events.TopicList, s.ID(), common.UnitID, event.NewHeader(), ctx, events.ListCommand{})).Get()
	if err != nil {
		return nil, fmt.Errorf("Codex account pool unavailable")
	}
	result, ok := value.(events.ListResult)
	if !ok {
		return nil, fmt.Errorf("invalid Codex account list result")
	}
	return result.Items, nil
}
func (s *Admin) ImportCodexAccounts(ctx context.Context, accounts []events.CredentialInput) (codexmanagement.ImportResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(events.TopicImport, s.ID(), common.UnitID, event.NewHeader(), ctx, events.ImportCommand{Accounts: accounts})).Get()
	if err != nil {
		return codexmanagement.ImportResult{}, fmt.Errorf("Codex account import failed")
	}
	result, ok := value.(events.ImportResult)
	if !ok {
		return codexmanagement.ImportResult{}, fmt.Errorf("invalid Codex account import result")
	}
	output := codexmanagement.ImportResult{Added: result.Added, Updated: result.Updated, Skipped: result.Skipped}
	progress, discoveryErr := s.StartCodexModelDiscovery(context.WithoutCancel(ctx), nil)
	if discoveryErr != nil {
		output.ModelDiscoveryError = discoveryErr.Error()
	} else {
		output.ModelDiscovery = &progress
	}
	usage, usageErr := s.StartCodexUsageRefresh(context.WithoutCancel(ctx), nil)
	if usageErr != nil {
		output.UsageRefreshError = usageErr.Error()
	} else {
		output.UsageRefresh = &usage
	}
	return output, nil
}
func (s *Admin) DeleteCodexAccounts(ctx context.Context, ids []string) (events.DeleteResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(events.TopicDelete, s.ID(), common.UnitID, event.NewHeader(), ctx, events.DeleteCommand{IDs: ids})).Get()
	if err != nil {
		return events.DeleteResult{}, fmt.Errorf("Codex account delete failed")
	}
	result, ok := value.(events.DeleteResult)
	if !ok {
		return events.DeleteResult{}, fmt.Errorf("invalid Codex account delete result")
	}
	return result, nil
}
func (s *Admin) UpdateCodexAccount(ctx context.Context, command events.UpdateCommand) (events.UpdateResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(events.TopicUpdate, s.ID(), common.UnitID, event.NewHeader(), ctx, command)).Get()
	if err != nil {
		return events.UpdateResult{}, fmt.Errorf("Codex account update failed")
	}
	result, ok := value.(events.UpdateResult)
	if !ok {
		return events.UpdateResult{}, fmt.Errorf("invalid Codex account update result")
	}
	return result, nil
}
func (s *Admin) RefreshCodexAccounts(ctx context.Context, ids []string) (codexmanagement.RefreshResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(events.TopicRefreshByID, s.ID(), common.UnitID, event.NewHeader(), ctx, events.RefreshByIDCommand{IDs: ids})).Get()
	if err != nil {
		return codexmanagement.RefreshResult{}, fmt.Errorf("Codex account refresh failed")
	}
	result, ok := value.(events.RefreshByIDResult)
	if !ok {
		return codexmanagement.RefreshResult{}, fmt.Errorf("invalid Codex account refresh result")
	}
	output := codexmanagement.RefreshResult{Refreshed: result.Refreshed, Failed: result.Failed, Items: result.Items}
	progress, discoveryErr := s.StartCodexModelDiscovery(context.WithoutCancel(ctx), ids)
	if discoveryErr != nil {
		output.ModelDiscoveryError = discoveryErr.Error()
	} else {
		output.ModelDiscovery = &progress
	}
	usage, usageErr := s.StartCodexUsageRefresh(context.WithoutCancel(ctx), ids)
	if usageErr != nil {
		output.UsageRefreshError = usageErr.Error()
	} else {
		output.UsageRefresh = &usage
	}
	return output, nil
}
func (s *Admin) StartCodexOAuth(ctx context.Context, hint, proxy string) (events.OAuthStartResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(events.TopicOAuthStart, s.ID(), common.UnitID, event.NewHeader(), ctx, events.OAuthStartCommand{EmailHint: hint, Proxy: proxy})).Get()
	if err != nil {
		return events.OAuthStartResult{}, fmt.Errorf("Codex OAuth start failed")
	}
	result, ok := value.(events.OAuthStartResult)
	if !ok {
		return events.OAuthStartResult{}, fmt.Errorf("invalid Codex OAuth start result")
	}
	return result, nil
}
func (s *Admin) FinishCodexOAuth(ctx context.Context, sessionID, callback string) (codexmanagement.OAuthFinishResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(events.TopicOAuthFinish, s.ID(), common.UnitID, event.NewHeader(), ctx, events.OAuthFinishCommand{SessionID: sessionID, Callback: callback})).Get()
	if err != nil {
		return codexmanagement.OAuthFinishResult{}, fmt.Errorf("Codex OAuth finish failed")
	}
	result, ok := value.(events.OAuthFinishResult)
	if !ok {
		return codexmanagement.OAuthFinishResult{}, fmt.Errorf("invalid Codex OAuth finish result")
	}
	output := codexmanagement.OAuthFinishResult{Added: result.Added, Item: result.Item}
	accountIDs := []string(nil)
	if result.Item.ID != "" {
		accountIDs = []string{result.Item.ID}
	}
	progress, discoveryErr := s.StartCodexModelDiscovery(context.WithoutCancel(ctx), accountIDs)
	if discoveryErr != nil {
		output.ModelDiscoveryError = discoveryErr.Error()
	} else {
		output.ModelDiscovery = &progress
	}
	usage, usageErr := s.StartCodexUsageRefresh(context.WithoutCancel(ctx), accountIDs)
	if usageErr != nil {
		output.UsageRefreshError = usageErr.Error()
	} else {
		output.UsageRefresh = &usage
	}
	return output, nil
}

// StartCodexModelDiscovery asks the proxy orchestrator to synchronize the
// account-scoped Codex model cache. Admin never talks to codexupstream or the
// account store directly.
func (s *Admin) StartCodexModelDiscovery(ctx context.Context, accountIDs []string) (proxyevents.CodexDiscoveryProgress, error) {
	value, err := s.SendEvent(event.NewEventWithContext(proxyevents.TopicStartCodexDiscovery, s.ID(), proxycommon.UnitID, event.NewHeader(), ctx, proxyevents.StartCodexDiscoveryCommand{AccountIDs: accountIDs})).Get()
	if err != nil {
		return proxyevents.CodexDiscoveryProgress{}, fmt.Errorf("Codex model discovery unavailable")
	}
	result, ok := value.(proxyevents.StartCodexDiscoveryResult)
	if !ok || !result.Progress.Valid() {
		return proxyevents.CodexDiscoveryProgress{}, fmt.Errorf("invalid Codex model discovery result")
	}
	return result.Progress, nil
}

func (s *Admin) CodexModelDiscoveryProgress(ctx context.Context, progressID string) (proxyevents.CodexDiscoveryProgress, error) {
	value, err := s.SendEvent(event.NewEventWithContext(proxyevents.TopicCodexDiscoveryProgress, s.ID(), proxycommon.UnitID, event.NewHeader(), ctx, proxyevents.CodexDiscoveryProgressCommand{ProgressID: progressID})).Get()
	if err != nil {
		return proxyevents.CodexDiscoveryProgress{}, fmt.Errorf("Codex model discovery progress unavailable")
	}
	result, ok := value.(proxyevents.CodexDiscoveryProgressResult)
	if !ok || !result.Progress.Valid() {
		return proxyevents.CodexDiscoveryProgress{}, fmt.Errorf("invalid Codex model discovery progress result")
	}
	return result.Progress, nil
}

// StartCodexUsageRefresh asks proxyapi to fetch the redacted upstream usage
// window projection. It stays separate from model discovery and does not
// change routing eligibility or cooldown state.
func (s *Admin) StartCodexUsageRefresh(ctx context.Context, accountIDs []string) (proxyevents.CodexUsageProgress, error) {
	value, err := s.SendEvent(event.NewEventWithContext(proxyevents.TopicStartCodexUsageRefresh, s.ID(), proxycommon.UnitID, event.NewHeader(), ctx, proxyevents.StartCodexUsageRefreshCommand{AccountIDs: accountIDs})).Get()
	if err != nil {
		return proxyevents.CodexUsageProgress{}, fmt.Errorf("Codex usage refresh unavailable")
	}
	result, ok := value.(proxyevents.StartCodexUsageRefreshResult)
	if !ok || !result.Progress.Valid() {
		return proxyevents.CodexUsageProgress{}, fmt.Errorf("invalid Codex usage refresh result")
	}
	return result.Progress, nil
}

func (s *Admin) CodexUsageRefreshProgress(ctx context.Context, progressID string) (proxyevents.CodexUsageProgress, error) {
	value, err := s.SendEvent(event.NewEventWithContext(proxyevents.TopicCodexUsageProgress, s.ID(), proxycommon.UnitID, event.NewHeader(), ctx, proxyevents.CodexUsageProgressCommand{ProgressID: progressID})).Get()
	if err != nil {
		return proxyevents.CodexUsageProgress{}, fmt.Errorf("Codex usage refresh progress unavailable")
	}
	result, ok := value.(proxyevents.CodexUsageProgressResult)
	if !ok || !result.Progress.Valid() {
		return proxyevents.CodexUsageProgress{}, fmt.Errorf("invalid Codex usage refresh progress result")
	}
	return result.Progress, nil
}
