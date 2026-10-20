package biz

import (
	"context"

	detectioncommon "aetherrelay/internal/modules/application/modeldetection/pkg/common"
	detectionevents "aetherrelay/internal/modules/application/modeldetection/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func (s *Admin) StartCodexModelTraceProbe(ctx context.Context, slot, model, reasoning, key string) (detectionevents.Report, error) {
	return s.modeltraceCommand(event.NewEventWithContext(detectionevents.TopicStart, s.ID(), detectioncommon.UnitID, event.NewHeader(), ctx, detectionevents.StartCommand{SlotID: slot, Model: model, Reasoning: reasoning, IdempotencyKey: key}))
}
func (s *Admin) GetCodexModelTraceProbe(ctx context.Context, slot, id string) (detectionevents.Report, error) {
	return s.modeltraceCommand(event.NewEventWithContext(detectionevents.TopicGet, s.ID(), detectioncommon.UnitID, event.NewHeader(), ctx, detectionevents.GetCommand{SlotID: slot, ProbeID: id}))
}
func (s *Admin) modeltraceCommand(ev event.Event) (detectionevents.Report, error) {
	v, err := s.SendEvent(ev).Get()
	if err != nil {
		return detectionevents.Report{}, &detectionevents.APIError{Status: 503, Code: "probe_unavailable"}
	}
	out, ok := v.(detectionevents.Result)
	if !ok {
		return detectionevents.Report{}, &detectionevents.APIError{Status: 503, Code: "invalid_probe_result"}
	}
	if out.ErrorCode != "" {
		return detectionevents.Report{}, &detectionevents.APIError{Status: out.Status, Code: out.ErrorCode}
	}
	return out.Report, nil
}

func (s *Admin) CancelCodexModelTraceProbe(ctx context.Context, slot, id string) (detectionevents.Report, error) {
	return s.modeltraceCommand(event.NewEventWithContext(detectionevents.TopicCancel, s.ID(), detectioncommon.UnitID, event.NewHeader(), ctx, detectionevents.CancelCommand{SlotID: slot, ProbeID: id}))
}
