package biz

import (
	"context"

	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func (s *Admin) StartCodexSteeringProbe(ctx context.Context, slot, model, key string) (proxyevents.SteeringReport, error) {
	return s.steeringCommand(event.NewEventWithContext(proxyevents.TopicStartSteeringProbe, s.ID(), proxycommon.UnitID, event.NewHeader(), ctx, proxyevents.StartSteeringProbeCommand{SlotID: slot, Model: model, IdempotencyKey: key}))
}
func (s *Admin) GetCodexSteeringProbe(ctx context.Context, slot, id string) (proxyevents.SteeringReport, error) {
	return s.steeringCommand(event.NewEventWithContext(proxyevents.TopicGetSteeringProbe, s.ID(), proxycommon.UnitID, event.NewHeader(), ctx, proxyevents.GetSteeringProbeCommand{SlotID: slot, ProbeID: id}))
}
func (s *Admin) steeringCommand(ev event.Event) (proxyevents.SteeringReport, error) {
	v, err := s.SendEvent(ev).Get()
	if err != nil {
		return proxyevents.SteeringReport{}, &proxyevents.SteeringAPIError{Status: 503, Code: "probe_unavailable"}
	}
	out, ok := v.(proxyevents.SteeringProbeResult)
	if !ok {
		return proxyevents.SteeringReport{}, &proxyevents.SteeringAPIError{Status: 503, Code: "invalid_probe_result"}
	}
	if out.ErrorCode != "" {
		return proxyevents.SteeringReport{}, &proxyevents.SteeringAPIError{Status: out.Status, Code: out.ErrorCode}
	}
	return out.Report, nil
}
