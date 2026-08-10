package biz

import (
	"bytes"
	"context"

	basebiz "aetherrelay/internal/modules/base/biz"
	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	metricscommon "aetherrelay/internal/modules/blocks/metricsruntime/pkg/common"
	metricsevents "aetherrelay/internal/modules/blocks/metricsruntime/pkg/events"
	usageevents "aetherrelay/internal/modules/blocks/usageruntime/pkg/events"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type MetricsRuntime struct {
	basebiz.Base
	runtime *Runtime
}

func New(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) (*MetricsRuntime, *cd.Error) {
	biz := &MetricsRuntime{Base: basebiz.New(metricscommon.UnitID, hub, background)}
	bootstrap, err := configevents.RequestBootstrap(ctx, biz.EventHub(), biz.ID())
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	store, err := usageevents.RequestStore(ctx, biz.EventHub(), biz.ID())
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	runtime, err := NewRuntime(ctx, bootstrap, store)
	if err != nil {
		return nil, cd.NewError(cd.Unexpected, "init metrics runtime: "+err.Error())
	}
	biz.runtime = runtime
	biz.SubscribeFunc(metricsevents.TopicAcquire, biz.handleAcquire)
	biz.SubscribeFunc(metricsevents.TopicRecord, biz.handleRecord)
	biz.SubscribeFunc(metricsevents.TopicPrometheus, biz.handlePrometheus)
	biz.SubscribeFunc(metricsevents.TopicStats, biz.handleStats)
	biz.SubscribeFunc(metricsevents.TopicProviderHealth, biz.handleProviderHealth)
	biz.SubscribeFunc(metricsevents.TopicProviderModel, biz.handleProviderModelHealth)
	biz.SubscribeFunc(metricsevents.TopicResetHealth, biz.handleResetProviderHealth)
	return biz, nil
}

func (s *MetricsRuntime) Run(context.Context) *cd.Error {
	if s.runtime == nil {
		return cd.NewError(cd.IllegalParam, "metrics runtime is not configured")
	}
	if err := s.runtime.Run(s.BackgroundRoutine()); err != nil {
		return cd.NewError(cd.Unexpected, "run metrics runtime: "+err.Error())
	}
	return nil
}

func (s *MetricsRuntime) Teardown(context.Context) {
	s.UnsubscribeFunc(metricsevents.TopicAcquire)
	s.UnsubscribeFunc(metricsevents.TopicRecord)
	s.UnsubscribeFunc(metricsevents.TopicPrometheus)
	s.UnsubscribeFunc(metricsevents.TopicStats)
	s.UnsubscribeFunc(metricsevents.TopicProviderHealth)
	s.UnsubscribeFunc(metricsevents.TopicProviderModel)
	s.UnsubscribeFunc(metricsevents.TopicResetHealth)
	if s.runtime != nil {
		s.runtime.Close()
	}
	s.runtime = nil
}

func (s *MetricsRuntime) handleAcquire(ev event.Event, result event.Result) {
	if _, ok := ev.Data().(metricsevents.AcquireCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid metrics acquire command"))
		return
	}
	if s.runtime == nil || s.runtime.Registry() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "metrics block is not ready"))
		return
	}
	result.Set(metricsevents.AcquireResult{}, nil)
}

// handleRecord only handles EventHub Post notifications; Post has no Result.
func (s *MetricsRuntime) handleRecord(ev event.Event, _ event.Result) {
	command, ok := ev.Data().(metricsevents.RecordCommand)
	if !ok || s.runtime == nil || s.runtime.Registry() == nil {
		return
	}
	registry := s.runtime.Registry()
	switch command.Kind {
	case metricsevents.ReserveModels:
		registry.ReserveModels(command.Provider, command.Models)
	case metricsevents.ClientUsage:
		registry.RecordClientUsage(command.APIKeyID, command.Input, command.Output)
	case metricsevents.UsageStoreWriteError:
		registry.RecordUsageStoreWriteError(command.Phase)
	case metricsevents.UsageStoreQuery:
		var err error
		if command.Failed {
			err = context.DeadlineExceeded
		}
		registry.RecordUsageStoreQuery(command.Duration, err, command.Healthy)
	case metricsevents.UsageStoreRecovered:
		registry.RecordUsageStoreRecovered(command.Count)
	case metricsevents.UsageStoreHealthy:
		registry.SetUsageStoreHealthy(command.Healthy)
	case metricsevents.RequestPlan:
		registry.RecordRequestPlanWithLevel(command.Provider, command.Model, command.Route, command.Status, command.Duration, command.Outcome, command.ClientEndpoint, command.UpstreamProtocol, command.UpstreamEndpoint, command.ConversionMode, command.ConversionLevel)
	case metricsevents.Conversion:
		registry.RecordConversion(command.Provider, command.Model, command.ClientProtocol, command.UpstreamProtocol, command.ConversionMode, command.ConversionLevel, command.UpstreamStatus, command.Duration, command.Degraded, command.Estimated, command.IgnoredFeatures, command.UnsupportedFeatures)
	case metricsevents.Tokens:
		registry.RecordTokens(command.Provider, command.Model, command.Input, command.Output, command.Cached, command.CacheCreation)
	case metricsevents.UpstreamAttempt:
		registry.RecordUpstreamAttempt(command.Provider, command.Duration, command.AttemptKind)
	case metricsevents.UpstreamError:
		registry.RecordUpstreamError(command.Provider, command.Status)
	}
}

func (s *MetricsRuntime) handlePrometheus(ev event.Event, result event.Result) {
	if _, ok := ev.Data().(metricsevents.PrometheusCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid prometheus command"))
		return
	}
	if s.runtime == nil || s.runtime.Registry() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "metrics block is not ready"))
		return
	}
	registry := s.runtime.Registry()
	var buffer bytes.Buffer
	err := registry.WritePrometheus(&buffer)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(metricsevents.BytesResult{Data: buffer.Bytes()}, nil)
}

func (s *MetricsRuntime) handleStats(ev event.Event, result event.Result) {
	if _, ok := ev.Data().(metricsevents.StatsCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid metrics stats command"))
		return
	}
	if s.runtime == nil || s.runtime.Registry() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "metrics block is not ready"))
		return
	}
	data, err := s.runtime.Registry().StatsJSON()
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(metricsevents.BytesResult{Data: data}, nil)
}

func (s *MetricsRuntime) handleProviderHealth(ev event.Event, result event.Result) {
	if _, ok := ev.Data().(metricsevents.ProviderHealthCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid metrics provider health command"))
		return
	}
	if s.runtime == nil || s.runtime.Registry() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "metrics block is not ready"))
		return
	}
	result.Set(metricsevents.ProviderHealthResult{Values: s.runtime.Registry().ProviderHealthSnapshot()}, nil)
}

func (s *MetricsRuntime) handleProviderModelHealth(ev event.Event, result event.Result) {
	command, ok := ev.Data().(metricsevents.ProviderModelHealthCommand)
	if !ok || command.Provider == "" || command.Model == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid metrics provider model health command"))
		return
	}
	if s.runtime == nil || s.runtime.Registry() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "metrics block is not ready"))
		return
	}
	value, found := s.runtime.Registry().ProviderModelHealth(command.Provider, command.Model)
	result.Set(metricsevents.ProviderModelHealthResult{Value: value, Found: found}, nil)
}

func (s *MetricsRuntime) handleResetProviderHealth(ev event.Event, result event.Result) {
	command, ok := ev.Data().(metricsevents.ResetProviderHealthCommand)
	if !ok || command.Provider == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid metrics reset provider health command"))
		return
	}
	if s.runtime == nil || s.runtime.Registry() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "metrics block is not ready"))
		return
	}
	s.runtime.Registry().ResetProviderHealth(command.Provider)
	result.Set(metricsevents.ResetProviderHealthResult{}, nil)
}
