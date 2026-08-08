package biz

import (
	"context"
	"os"

	basebiz "ai-proxy/internal/modules/base/biz"
	"ai-proxy/internal/modules/blocks/chatgptimagestore/internal/store"
	"ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/common"
	events "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/events"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type ImageStore struct {
	basebiz.Base
	store  *store.Store
	topics []string
}

func New(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) (*ImageStore, *cd.Error) {
	b := &ImageStore{
		Base: basebiz.New(common.UnitID, hub, background),
	}
	bootstrap, err := configevents.RequestBootstrap(ctx, b.EventHub(), b.ID())
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	if err := os.MkdirAll(bootstrap.Config.State.Dir, 0o700); err != nil {
		return nil, cd.NewError(cd.Unexpected, "create chatgpt web data directory: "+err.Error())
	}
	b.store, err = store.Open(bootstrap.Config.State.Dir, bootstrap.Config.State.Database, bootstrap.Config.State.MemoryLimit, bootstrap.Config.State.Threads)
	if err != nil {
		return nil, cd.NewError(cd.Unexpected, "open chatgpt image state: "+err.Error())
	}
	b.topics = []string{
		events.TopicSave,
		events.TopicGetBytes,
		events.TopicDelete,
		events.TopicDeleteScope,
		events.TopicList,
		events.TopicEnsureThumbnail,
		events.TopicGetThumbnail,
		events.TopicExists,
		events.TopicListTags,
		events.TopicSetTags,
		events.TopicDeleteTag,
		events.TopicStorageStats,
		events.TopicCompress,
		events.TopicCleanupToTarget,
	}
	b.SubscribeFunc(events.TopicSave, b.handleSave)
	b.SubscribeFunc(events.TopicGetBytes, b.handleGetBytes)
	b.SubscribeFunc(events.TopicDelete, b.handleDelete)
	b.SubscribeFunc(events.TopicDeleteScope, b.handleDeleteScope)
	b.SubscribeFunc(events.TopicList, b.handleList)
	b.SubscribeFunc(events.TopicEnsureThumbnail, b.handleEnsureThumbnail)
	b.SubscribeFunc(events.TopicGetThumbnail, b.handleGetThumbnail)
	b.SubscribeFunc(events.TopicExists, b.handleExists)
	b.SubscribeFunc(events.TopicListTags, b.handleListTags)
	b.SubscribeFunc(events.TopicSetTags, b.handleSetTags)
	b.SubscribeFunc(events.TopicDeleteTag, b.handleDeleteTag)
	b.SubscribeFunc(events.TopicStorageStats, b.handleStorageStats)
	b.SubscribeFunc(events.TopicCompress, b.handleCompress)
	b.SubscribeFunc(events.TopicCleanupToTarget, b.handleCleanupToTarget)
	return b, nil
}

func (s *ImageStore) Run(context.Context) *cd.Error { return nil }

func (s *ImageStore) Teardown(context.Context) {
	for _, topic := range s.topics {
		s.UnsubscribeFunc(topic)
	}
	if s.store != nil {
		_ = s.store.Close()
	}
	s.store = nil
}

func (s *ImageStore) AbsolutePath(rel string) string {
	return s.store.AbsolutePath(rel)
}

func (s *ImageStore) handleSave(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.SaveCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid save command"))
		return
	}
	if cmd.APIKeyID == "" || len(cmd.Bytes) == 0 {
		result.Set(nil, cd.NewError(cd.IllegalParam, "empty image bytes"))
		return
	}
	out, err := s.store.Save(cmd.Bytes, cmd.BaseURL, cmd.APIKeyID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(out, nil)
}

func (s *ImageStore) handleGetBytes(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.GetBytesCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid get bytes command"))
		return
	}
	data, err := s.store.GetBytes(cmd.RelativePath, cmd.APIKeyID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.GetBytesResult{Bytes: data}, nil)
}

func (s *ImageStore) handleDelete(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.DeleteCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid delete command"))
		return
	}
	n, err := s.store.Delete(cmd.Paths, cmd.APIKeyID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.DeleteResult{Deleted: n}, nil)
}

func (s *ImageStore) handleDeleteScope(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.DeleteScopeCommand)
	if !ok || cmd.APIKeyID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid delete image scope command"))
		return
	}
	n, err := s.store.DeleteScope(cmd.APIKeyID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.DeleteScopeResult{Deleted: n}, nil)
}

func (s *ImageStore) handleList(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.ListCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid list command"))
		return
	}
	result.Set(events.ListResult{Items: s.store.List(cmd.BaseURL, cmd.StartDate, cmd.EndDate, cmd.APIKeyID)}, nil)
}

func (s *ImageStore) handleExists(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.ExistsCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid exists command"))
		return
	}
	result.Set(events.ExistsResult{Exists: s.store.Exists(cmd.RelativePath, cmd.APIKeyID)}, nil)
}

func (s *ImageStore) handleEnsureThumbnail(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.EnsureThumbnailCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid ensure thumbnail command"))
		return
	}
	out, err := s.store.EnsureThumbnail(cmd.RelativePath, "", cmd.APIKeyID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(out, nil)
}

func (s *ImageStore) handleGetThumbnail(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.GetThumbnailCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid get thumbnail command"))
		return
	}
	data, err := s.store.GetThumbnailBytes(cmd.RelativePath, cmd.APIKeyID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.GetThumbnailResult{Bytes: data}, nil)
}

func (s *ImageStore) handleListTags(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if _, ok := ev.Data().(events.ListTagsCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid list image tags command"))
		return
	}
	cmd, _ := ev.Data().(events.ListTagsCommand)
	result.Set(events.ListTagsResult{Tags: s.store.ListTags(cmd.APIKeyID)}, nil)
}

func (s *ImageStore) handleSetTags(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.SetTagsCommand)
	if !ok || cmd.Path == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid set image tags command"))
		return
	}
	tags, err := s.store.SetTags(cmd.Path, cmd.Tags, cmd.APIKeyID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	result.Set(events.SetTagsResult{Tags: tags}, nil)
}

func (s *ImageStore) handleDeleteTag(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.DeleteTagCommand)
	if !ok || cmd.Tag == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid delete image tag command"))
		return
	}
	removed, err := s.store.DeleteTag(cmd.Tag, cmd.APIKeyID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.DeleteTagResult{RemovedFrom: removed}, nil)
}

func (s *ImageStore) handleStorageStats(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if _, ok := ev.Data().(events.StorageStatsCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid image storage stats command"))
		return
	}
	cmd, _ := ev.Data().(events.StorageStatsCommand)
	stats, err := s.store.StorageStats(cmd.APIKeyID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(stats, nil)
}

func (s *ImageStore) handleCompress(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if _, ok := ev.Data().(events.CompressCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid image compression command"))
		return
	}
	cmd, _ := ev.Data().(events.CompressCommand)
	compressed, err := s.store.CompressImages(cmd.APIKeyID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(compressed, nil)
}

func (s *ImageStore) handleCleanupToTarget(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.CleanupToTargetCommand)
	if !ok || cmd.TargetFreeMB < 0 {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid image cleanup command"))
		return
	}
	cleaned, err := s.store.CleanupToTarget(cmd.TargetFreeMB, cmd.DryRun, cmd.APIKeyID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(cleaned, nil)
}
