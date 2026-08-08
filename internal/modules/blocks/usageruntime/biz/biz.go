package biz

import (
	"bytes"
	"context"
	"fmt"

	basebiz "ai-proxy/internal/modules/base/biz"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	usagecommon "ai-proxy/internal/modules/blocks/usageruntime/pkg/common"
	usageevents "ai-proxy/internal/modules/blocks/usageruntime/pkg/events"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type UsageRuntime struct {
	basebiz.Base
	runtime *Runtime
}

func New(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) (*UsageRuntime, *cd.Error) {
	biz := &UsageRuntime{Base: basebiz.New(usagecommon.UnitID, hub, background)}
	bootstrap, err := configevents.RequestBootstrap(ctx, biz.EventHub(), biz.ID())
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	runtime, err := NewRuntime(bootstrap)
	if err != nil {
		return nil, cd.NewError(cd.Unexpected, "init usage runtime: "+err.Error())
	}
	biz.runtime = runtime
	biz.SubscribeFunc(usageevents.TopicAcquire, biz.handleAcquire)
	biz.SubscribeFunc(usageevents.TopicStart, biz.handleStart)
	biz.SubscribeFunc(usageevents.TopicComplete, biz.handleComplete)
	biz.SubscribeFunc(usageevents.TopicDashboard, biz.handleDashboard)
	biz.SubscribeFunc(usageevents.TopicCount, biz.handleCount)
	biz.SubscribeFunc(usageevents.TopicEvents, biz.handleEvents)
	biz.SubscribeFunc(usageevents.TopicExport, biz.handleExport)
	biz.SubscribeFunc(usageevents.TopicFilterOptions, biz.handleFilterOptions)
	biz.SubscribeFunc(usageevents.TopicRecover, biz.handleRecover)
	biz.SubscribeFunc(usageevents.TopicCheckpoint, biz.handleCheckpoint)
	biz.SubscribeFunc(usageevents.TopicHealthy, biz.handleHealthy)
	biz.SubscribeFunc(usageevents.TopicAllTime, biz.handleAllTime)
	biz.SubscribeFunc(usageevents.TopicClientKeyEnsure, biz.handleClientKeyEnsure)
	biz.SubscribeFunc(usageevents.TopicClientKeyTouch, biz.handleClientKeyTouch)
	biz.SubscribeFunc(usageevents.TopicClientKeyMetadata, biz.handleClientKeyMetadata)
	biz.SubscribeFunc(usageevents.TopicClientKeyList, biz.handleClientKeyCommand)
	biz.SubscribeFunc(usageevents.TopicClientKeyCreate, biz.handleClientKeyCommand)
	biz.SubscribeFunc(usageevents.TopicClientKeyEnable, biz.handleClientKeyCommand)
	biz.SubscribeFunc(usageevents.TopicClientKeyRotate, biz.handleClientKeyCommand)
	biz.SubscribeFunc(usageevents.TopicClientKeyRevoke, biz.handleClientKeyCommand)
	biz.SubscribeFunc(usageevents.TopicClientKeyDelete, biz.handleClientKeyCommand)
	biz.SubscribeFunc(usageevents.TopicClientKeyAccess, biz.handleClientKeyCommand)
	biz.SubscribeFunc(usageevents.TopicClientKeyRefs, biz.handleClientKeyCommand)
	return biz, nil
}

func (s *UsageRuntime) Run(context.Context) *cd.Error { return nil }

func (s *UsageRuntime) Teardown(ctx context.Context) {
	s.UnsubscribeFunc(usageevents.TopicAcquire)
	s.UnsubscribeFunc(usageevents.TopicStart)
	s.UnsubscribeFunc(usageevents.TopicComplete)
	s.UnsubscribeFunc(usageevents.TopicDashboard)
	s.UnsubscribeFunc(usageevents.TopicCount)
	s.UnsubscribeFunc(usageevents.TopicEvents)
	s.UnsubscribeFunc(usageevents.TopicExport)
	s.UnsubscribeFunc(usageevents.TopicFilterOptions)
	s.UnsubscribeFunc(usageevents.TopicRecover)
	s.UnsubscribeFunc(usageevents.TopicCheckpoint)
	s.UnsubscribeFunc(usageevents.TopicHealthy)
	s.UnsubscribeFunc(usageevents.TopicAllTime)
	s.UnsubscribeFunc(usageevents.TopicClientKeyEnsure)
	s.UnsubscribeFunc(usageevents.TopicClientKeyTouch)
	s.UnsubscribeFunc(usageevents.TopicClientKeyMetadata)
	s.UnsubscribeFunc(usageevents.TopicClientKeyList)
	s.UnsubscribeFunc(usageevents.TopicClientKeyCreate)
	s.UnsubscribeFunc(usageevents.TopicClientKeyEnable)
	s.UnsubscribeFunc(usageevents.TopicClientKeyRotate)
	s.UnsubscribeFunc(usageevents.TopicClientKeyRevoke)
	s.UnsubscribeFunc(usageevents.TopicClientKeyDelete)
	s.UnsubscribeFunc(usageevents.TopicClientKeyAccess)
	s.UnsubscribeFunc(usageevents.TopicClientKeyRefs)
	if s.runtime != nil {
		s.runtime.Close(ctx)
	}
	s.runtime = nil
}

func (s *UsageRuntime) handleAcquire(ev event.Event, result event.Result) {
	if _, ok := ev.Data().(usageevents.AcquireCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage acquire command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	result.Set(usageevents.AcquireResult{}, nil)
}

func (s *UsageRuntime) handleStart(ev event.Event, result event.Result) {
	command, ok := ev.Data().(usageevents.StartCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage start command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	if err := s.runtime.Store().Start(ev.Context(), command.Record); err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(struct{}{}, nil)
}

func (s *UsageRuntime) handleComplete(ev event.Event, result event.Result) {
	command, ok := ev.Data().(usageevents.CompleteCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage complete command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	if err := s.runtime.Store().Complete(ev.Context(), command.Record); err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(struct{}{}, nil)
}

func (s *UsageRuntime) handleDashboard(ev event.Event, result event.Result) {
	command, ok := ev.Data().(usageevents.DashboardCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage dashboard command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	value, err := s.runtime.Store().Dashboard(ev.Context(), command.Filter)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(usageevents.DashboardResult{Value: value}, nil)
}

func (s *UsageRuntime) handleCount(ev event.Event, result event.Result) {
	command, ok := ev.Data().(usageevents.CountCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage count command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	value, err := s.runtime.Store().Count(ev.Context(), command.Filter)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(usageevents.CountResult{Value: value}, nil)
}

func (s *UsageRuntime) handleEvents(ev event.Event, result event.Result) {
	command, ok := ev.Data().(usageevents.EventsCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage events command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	value, err := s.runtime.Store().Events(ev.Context(), command.Filter)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(usageevents.EventsResult{Value: value}, nil)
}

func (s *UsageRuntime) handleExport(ev event.Event, result event.Result) {
	command, ok := ev.Data().(usageevents.ExportCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage export command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	var buffer bytes.Buffer
	if err := s.runtime.Store().ExportCSV(ev.Context(), command.Filter, &buffer); err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(usageevents.ExportResult{Data: buffer.Bytes()}, nil)
}

func (s *UsageRuntime) handleFilterOptions(ev event.Event, result event.Result) {
	command, ok := ev.Data().(usageevents.FilterOptionsCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage filter options command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	value, err := s.runtime.Store().FilterOptions(ev.Context(), command.Query)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(usageevents.FilterOptionsResult{Value: value}, nil)
}

func (s *UsageRuntime) handleRecover(ev event.Event, result event.Result) {
	command, ok := ev.Data().(usageevents.RecoverCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage recover command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	count, err := s.runtime.Store().RecoverInterrupted(ev.Context(), command.At)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(usageevents.RecoverResult{Count: count}, nil)
}

func (s *UsageRuntime) handleCheckpoint(ev event.Event, result event.Result) {
	if _, ok := ev.Data().(usageevents.CheckpointCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage checkpoint command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	if err := s.runtime.Store().Checkpoint(ev.Context()); err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(struct{}{}, nil)
}

func (s *UsageRuntime) handleHealthy(ev event.Event, result event.Result) {
	if _, ok := ev.Data().(usageevents.HealthyCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage healthy command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	result.Set(usageevents.HealthyResult{Value: s.runtime.Store().Healthy()}, nil)
}

func (s *UsageRuntime) handleAllTime(ev event.Event, result event.Result) {
	if _, ok := ev.Data().(usageevents.AllTimeCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage all time command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	value, err := s.runtime.Store().AllTimeByKey(ev.Context())
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(usageevents.AllTimeResult{Value: value}, nil)
}
func (s *UsageRuntime) handleClientKeyCommand(ev event.Event, result event.Result) {
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	st := s.runtime.Store()
	var err error
	switch c := ev.Data().(type) {
	case usageevents.ClientKeyListCommand:
		v, e := st.ListClientAPIKeys(ev.Context())
		if e != nil {
			err = e
		} else {
			result.Set(usageevents.ClientKeyListResult{Value: v}, nil)
			return
		}
	case usageevents.ClientKeyCreateCommand:
		err = st.CreateClientAPIKey(ev.Context(), c.Value)
	case usageevents.ClientKeyEnableCommand:
		err = st.SetClientAPIKeyEnabled(ev.Context(), c.ID, c.Enabled)
	case usageevents.ClientKeyRotateCommand:
		err = st.RotateClientAPIKey(ev.Context(), c.ID, c.Hash, c.At)
	case usageevents.ClientKeyRevokeCommand:
		err = st.RevokeClientAPIKey(ev.Context(), c.ID, c.At)
	case usageevents.ClientKeyDeleteCommand:
		err = st.DeleteClientAPIKey(ev.Context(), c.ID)
	case usageevents.ClientKeyAccessCommand:
		err = st.SetClientAPIKeyProviderAccess(ev.Context(), c.ID, c.Policy)
	case usageevents.ClientKeyProviderRefsCommand:
		ids, e := st.ClientAPIKeyIDsForProvider(ev.Context(), c.ProviderID)
		if e != nil {
			err = e
		} else {
			result.Set(usageevents.ClientKeyProviderRefsResult{IDs: ids}, nil)
			return
		}
	default:
		err = fmt.Errorf("invalid client key command")
	}
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(struct{}{}, nil)
}

func (s *UsageRuntime) handleClientKeyEnsure(ev event.Event, result event.Result) {
	cmd, ok := ev.Data().(usageevents.ClientKeyEnsureCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid client key ensure command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	if err := s.runtime.Store().EnsureClientAPIKey(ev.Context(), cmd.ID, cmd.CreatedAt); err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(struct{}{}, nil)
}
func (s *UsageRuntime) handleClientKeyTouch(ev event.Event, result event.Result) {
	cmd, ok := ev.Data().(usageevents.ClientKeyTouchCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid client key touch command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	if err := s.runtime.Store().TouchClientAPIKey(ev.Context(), cmd.ID, cmd.UsedAt); err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(struct{}{}, nil)
}
func (s *UsageRuntime) handleClientKeyMetadata(ev event.Event, result event.Result) {
	if _, ok := ev.Data().(usageevents.ClientKeyMetadataCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid client key metadata command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	value, err := s.runtime.Store().ClientAPIKeyMetadata(ev.Context())
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(usageevents.ClientKeyMetadataResult{Value: value}, nil)
}
