package biz

import (
	"context"
	"fmt"

	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	taskcommon "ai-proxy/internal/modules/application/chatgptimagetask/pkg/common"
	taskevents "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	imgcommon "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/common"
	imgevents "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func (s *Admin) ListChatGPTAccounts(ctx context.Context) ([]accevents.AccountView, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicList, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.ListCommand{})).Get()
	if err != nil {
		return nil, fmt.Errorf("chatgpt account pool unavailable")
	}
	result, ok := value.(accevents.ListResult)
	if !ok {
		return nil, fmt.Errorf("invalid chatgpt account list result")
	}
	return result.Items, nil
}

func (s *Admin) AddChatGPTAccounts(ctx context.Context, tokens []string, sourceType string) (accevents.AddResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicAdd, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.AddCommand{Tokens: tokens, SourceType: sourceType})).Get()
	if err != nil {
		return accevents.AddResult{}, fmt.Errorf("chatgpt account add failed")
	}
	result, ok := value.(accevents.AddResult)
	if !ok {
		return accevents.AddResult{}, fmt.Errorf("invalid chatgpt account add result")
	}
	return result, nil
}

func (s *Admin) DeleteChatGPTAccounts(ctx context.Context, ids []string) (accevents.DeleteResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicDeleteByID, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.DeleteByIDCommand{IDs: ids})).Get()
	if err != nil {
		return accevents.DeleteResult{}, fmt.Errorf("chatgpt account delete failed")
	}
	result, ok := value.(accevents.DeleteResult)
	if !ok {
		return accevents.DeleteResult{}, fmt.Errorf("invalid chatgpt account delete result")
	}
	return result, nil
}

func (s *Admin) UpdateChatGPTAccount(ctx context.Context, command accevents.UpdateByIDCommand) (accevents.UpdateResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicUpdateByID, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, command)).Get()
	if err != nil {
		return accevents.UpdateResult{}, fmt.Errorf("chatgpt account update failed")
	}
	result, ok := value.(accevents.UpdateResult)
	if !ok {
		return accevents.UpdateResult{}, fmt.Errorf("invalid chatgpt account update result")
	}
	return result, nil
}

func (s *Admin) ExportChatGPTAccounts(ctx context.Context, ids []string) (accevents.ExportResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicExportByID, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.ExportByIDCommand{IDs: ids})).Get()
	if err != nil {
		return accevents.ExportResult{}, fmt.Errorf("chatgpt account export failed")
	}
	result, ok := value.(accevents.ExportResult)
	if !ok {
		return accevents.ExportResult{}, fmt.Errorf("invalid chatgpt account export result")
	}
	return result, nil
}

func (s *Admin) RefreshChatGPTAccounts(ctx context.Context, tokens []string) (accevents.RefreshResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicRefresh, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.RefreshCommand{AccessTokens: tokens})).Get()
	if err != nil {
		return accevents.RefreshResult{}, fmt.Errorf("chatgpt account refresh unavailable")
	}
	result, ok := value.(accevents.RefreshResult)
	if !ok {
		return accevents.RefreshResult{}, fmt.Errorf("invalid chatgpt account refresh result")
	}
	return result, nil
}

func (s *Admin) RefreshChatGPTAccountsByID(ctx context.Context, ids []string) (accevents.RefreshResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicRefreshByID, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.RefreshByIDCommand{IDs: ids})).Get()
	if err != nil {
		return accevents.RefreshResult{}, fmt.Errorf("chatgpt account refresh unavailable")
	}
	result, ok := value.(accevents.RefreshResult)
	if !ok {
		return accevents.RefreshResult{}, fmt.Errorf("invalid chatgpt account refresh result")
	}
	return result, nil
}
func (s *Admin) ChatGPTAccountRefreshProgress(ctx context.Context, id string) (accevents.RefreshProgress, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicRefreshProgress, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.RefreshProgressCommand{ProgressID: id})).Get()
	if err != nil {
		return accevents.RefreshProgress{}, fmt.Errorf("chatgpt account refresh progress unavailable")
	}
	result, ok := value.(accevents.RefreshProgressResult)
	if !ok {
		return accevents.RefreshProgress{}, fmt.Errorf("invalid chatgpt account refresh progress result")
	}
	return result.Progress, nil
}

func (s *Admin) StartChatGPTOAuth(ctx context.Context, hint string) (accevents.OAuthStartResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicOAuthStart, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.OAuthStartCommand{EmailHint: hint})).Get()
	if err != nil {
		return accevents.OAuthStartResult{}, fmt.Errorf("chatgpt OAuth start failed")
	}
	result, ok := value.(accevents.OAuthStartResult)
	if !ok {
		return accevents.OAuthStartResult{}, fmt.Errorf("invalid chatgpt OAuth start result")
	}
	return result, nil
}
func (s *Admin) FinishChatGPTOAuth(ctx context.Context, sessionID, callback string) (accevents.OAuthFinishResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicOAuthFinish, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.OAuthFinishCommand{SessionID: sessionID, Callback: callback})).Get()
	if err != nil {
		return accevents.OAuthFinishResult{}, fmt.Errorf("chatgpt OAuth finish failed")
	}
	result, ok := value.(accevents.OAuthFinishResult)
	if !ok {
		return accevents.OAuthFinishResult{}, fmt.Errorf("invalid chatgpt OAuth finish result")
	}
	return result, nil
}

func (s *Admin) SubmitChatGPTImageGeneration(ctx context.Context, command taskevents.SubmitGenerationCommand) (taskevents.SubmitResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(taskevents.TopicSubmitGeneration, s.ID(), taskcommon.UnitID, event.NewHeader(), ctx, command)).Get()
	if err != nil {
		return taskevents.SubmitResult{}, fmt.Errorf("chatgpt image task submission failed")
	}
	result, ok := value.(taskevents.SubmitResult)
	if !ok {
		return taskevents.SubmitResult{}, fmt.Errorf("invalid chatgpt image task submission result")
	}
	return result, nil
}
func (s *Admin) SubmitChatGPTImageEdit(ctx context.Context, command taskevents.SubmitEditCommand) (taskevents.SubmitResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(taskevents.TopicSubmitEdit, s.ID(), taskcommon.UnitID, event.NewHeader(), ctx, command)).Get()
	if err != nil {
		return taskevents.SubmitResult{}, fmt.Errorf("chatgpt image task submission failed")
	}
	result, ok := value.(taskevents.SubmitResult)
	if !ok {
		return taskevents.SubmitResult{}, fmt.Errorf("invalid chatgpt image task submission result")
	}
	return result, nil
}
func (s *Admin) ListChatGPTImageTasks(ctx context.Context, ownerID string, ids []string) (taskevents.ListResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(taskevents.TopicList, s.ID(), taskcommon.UnitID, event.NewHeader(), ctx, taskevents.ListCommand{OwnerID: ownerID, TaskIDs: ids})).Get()
	if err != nil {
		return taskevents.ListResult{}, fmt.Errorf("chatgpt image tasks unavailable")
	}
	result, ok := value.(taskevents.ListResult)
	if !ok {
		return taskevents.ListResult{}, fmt.Errorf("invalid chatgpt image task list result")
	}
	return result, nil
}
func (s *Admin) ResumeChatGPTImageTask(ctx context.Context, ownerID, taskID string, seconds int) (taskevents.ResumePollResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(taskevents.TopicResumePoll, s.ID(), taskcommon.UnitID, event.NewHeader(), ctx, taskevents.ResumePollCommand{OwnerID: ownerID, TaskID: taskID, ExtraTimeoutSecs: seconds})).Get()
	if err != nil {
		return taskevents.ResumePollResult{}, fmt.Errorf("chatgpt image task resume failed")
	}
	result, ok := value.(taskevents.ResumePollResult)
	if !ok {
		return taskevents.ResumePollResult{}, fmt.Errorf("invalid chatgpt image task resume result")
	}
	return result, nil
}

func (s *Admin) ListChatGPTImages(ctx context.Context, baseURL, startDate, endDate string) (imgevents.ListResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(imgevents.TopicList, s.ID(), imgcommon.UnitID, event.NewHeader(), ctx, imgevents.ListCommand{BaseURL: baseURL, StartDate: startDate, EndDate: endDate})).Get()
	if err != nil {
		return imgevents.ListResult{}, fmt.Errorf("chatgpt image store unavailable")
	}
	result, ok := value.(imgevents.ListResult)
	if !ok {
		return imgevents.ListResult{}, fmt.Errorf("invalid chatgpt image list result")
	}
	return result, nil
}

func (s *Admin) ChatGPTImageStorage(ctx context.Context) (imgevents.StorageStatsResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(imgevents.TopicStorageStats, s.ID(), imgcommon.UnitID, event.NewHeader(), ctx, imgevents.StorageStatsCommand{})).Get()
	if err != nil {
		return imgevents.StorageStatsResult{}, fmt.Errorf("chatgpt image store unavailable")
	}
	result, ok := value.(imgevents.StorageStatsResult)
	if !ok {
		return imgevents.StorageStatsResult{}, fmt.Errorf("invalid chatgpt image storage result")
	}
	return result, nil
}

func (s *Admin) ListChatGPTImageTags(ctx context.Context) (imgevents.ListTagsResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(imgevents.TopicListTags, s.ID(), imgcommon.UnitID, event.NewHeader(), ctx, imgevents.ListTagsCommand{})).Get()
	if err != nil {
		return imgevents.ListTagsResult{}, fmt.Errorf("chatgpt image store unavailable")
	}
	result, ok := value.(imgevents.ListTagsResult)
	if !ok {
		return imgevents.ListTagsResult{}, fmt.Errorf("invalid chatgpt image tag result")
	}
	return result, nil
}

func (s *Admin) SetChatGPTImageTags(ctx context.Context, path string, tags []string) (imgevents.SetTagsResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(imgevents.TopicSetTags, s.ID(), imgcommon.UnitID, event.NewHeader(), ctx, imgevents.SetTagsCommand{Path: path, Tags: tags})).Get()
	if err != nil {
		return imgevents.SetTagsResult{}, fmt.Errorf("chatgpt image tag update failed")
	}
	result, ok := value.(imgevents.SetTagsResult)
	if !ok {
		return imgevents.SetTagsResult{}, fmt.Errorf("invalid chatgpt image tag update result")
	}
	return result, nil
}

func (s *Admin) DeleteChatGPTImages(ctx context.Context, paths []string) (imgevents.DeleteResult, error) {
	value, err := s.SendEvent(event.NewEventWithContext(imgevents.TopicDelete, s.ID(), imgcommon.UnitID, event.NewHeader(), ctx, imgevents.DeleteCommand{Paths: paths})).Get()
	if err != nil {
		return imgevents.DeleteResult{}, fmt.Errorf("chatgpt image delete failed")
	}
	result, ok := value.(imgevents.DeleteResult)
	if !ok {
		return imgevents.DeleteResult{}, fmt.Errorf("invalid chatgpt image delete result")
	}
	return result, nil
}

// GetChatGPTImageBytes returns original image bytes for a store-relative path.
// Path validation and traversal rejection stay in the image store owner.
func (s *Admin) GetChatGPTImageBytes(ctx context.Context, path string) ([]byte, error) {
	value, err := s.SendEvent(event.NewEventWithContext(imgevents.TopicGetBytes, s.ID(), imgcommon.UnitID, event.NewHeader(), ctx, imgevents.GetBytesCommand{RelativePath: path})).Get()
	if err != nil {
		return nil, fmt.Errorf("chatgpt image content unavailable")
	}
	result, ok := value.(imgevents.GetBytesResult)
	if !ok {
		return nil, fmt.Errorf("invalid chatgpt image content result")
	}
	return result.Bytes, nil
}

// GetChatGPTImageThumbnail returns a PNG thumbnail for a store-relative path.
func (s *Admin) GetChatGPTImageThumbnail(ctx context.Context, path string) ([]byte, error) {
	value, err := s.SendEvent(event.NewEventWithContext(imgevents.TopicGetThumbnail, s.ID(), imgcommon.UnitID, event.NewHeader(), ctx, imgevents.GetThumbnailCommand{RelativePath: path})).Get()
	if err != nil {
		return nil, fmt.Errorf("chatgpt image thumbnail unavailable")
	}
	result, ok := value.(imgevents.GetThumbnailResult)
	if !ok {
		return nil, fmt.Errorf("invalid chatgpt image thumbnail result")
	}
	return result.Bytes, nil
}

func (s *Admin) ChatGPTEffectiveCatalog(ctx context.Context) (effectivecatalog.Snapshot, error) {
	value, err := s.SendEvent(event.NewEventWithContext(proxyevents.TopicEffectiveCatalog, s.ID(), proxycommon.UnitID, event.NewHeader(), ctx, proxyevents.EffectiveCatalogCommand{})).Get()
	if err != nil {
		return effectivecatalog.Snapshot{}, fmt.Errorf("effective catalog unavailable")
	}
	result, ok := value.(proxyevents.EffectiveCatalogResult)
	if !ok {
		return effectivecatalog.Snapshot{}, fmt.Errorf("invalid effective catalog result")
	}
	return result.Snapshot, nil
}
