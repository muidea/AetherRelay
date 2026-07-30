package biz

import (
	"context"
	"fmt"

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
func (s *Admin) ImportCodexAccounts(ctx context.Context, accounts []events.CredentialInput) (events.ImportResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(events.TopicImport, s.ID(), common.UnitID, event.NewHeader(), ctx, events.ImportCommand{Accounts: accounts})).Get()
	if err != nil {
		return events.ImportResult{}, fmt.Errorf("Codex account import failed")
	}
	result, ok := value.(events.ImportResult)
	if !ok {
		return events.ImportResult{}, fmt.Errorf("invalid Codex account import result")
	}
	return result, nil
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
func (s *Admin) RefreshCodexAccounts(ctx context.Context, ids []string) (events.RefreshByIDResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(events.TopicRefreshByID, s.ID(), common.UnitID, event.NewHeader(), ctx, events.RefreshByIDCommand{IDs: ids})).Get()
	if err != nil {
		return events.RefreshByIDResult{}, fmt.Errorf("Codex account refresh failed")
	}
	result, ok := value.(events.RefreshByIDResult)
	if !ok {
		return events.RefreshByIDResult{}, fmt.Errorf("invalid Codex account refresh result")
	}
	return result, nil
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
func (s *Admin) FinishCodexOAuth(ctx context.Context, sessionID, callback string) (events.OAuthFinishResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(events.TopicOAuthFinish, s.ID(), common.UnitID, event.NewHeader(), ctx, events.OAuthFinishCommand{SessionID: sessionID, Callback: callback})).Get()
	if err != nil {
		return events.OAuthFinishResult{}, fmt.Errorf("Codex OAuth finish failed")
	}
	result, ok := value.(events.OAuthFinishResult)
	if !ok {
		return events.OAuthFinishResult{}, fmt.Errorf("invalid Codex OAuth finish result")
	}
	return result, nil
}
