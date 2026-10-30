package biz

import (
	"errors"

	usageevents "aetherrelay/internal/modules/blocks/usageruntime/pkg/events"
	usage "aetherrelay/internal/pkg/aetherrelayusage"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

func (s *UsageRuntime) handleWriteTrace(ev event.Event, result event.Result) {
	c, ok := ev.Data().(usageevents.WriteTraceCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage trace command"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	err := s.runtime.Store().WriteTrace(ev.Context(), c.EventID, c.Value)
	if errors.Is(err, usage.ErrEventNotStarted) {
		result.Set(usageevents.WriteTraceResult{EventNotStarted: true}, nil)
		return
	}
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(usageevents.WriteTraceResult{}, nil)
}
func (s *UsageRuntime) handleExecutionTrace(ev event.Event, result event.Result) {
	c, ok := ev.Data().(usageevents.ExecutionTraceCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage trace query"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	value, err := s.runtime.Store().ExecutionTrace(ev.Context(), c.EventID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	out := usageevents.ExecutionTraceResult{Found: value != nil}
	if value != nil {
		out.Value = *value
	}
	result.Set(out, nil)
}
func (s *UsageRuntime) handleAnomalies(ev event.Event, result event.Result) {
	c, ok := ev.Data().(usageevents.AnomaliesCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage anomaly query"))
		return
	}
	if s.runtime == nil || s.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	value, err := s.runtime.Store().Anomalies(ev.Context(), c.Filter, c.GroupBy)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(usageevents.AnomaliesResult{Value: value}, nil)
}
