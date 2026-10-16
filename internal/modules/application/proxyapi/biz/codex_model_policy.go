package biz

import (
	"context"
	"fmt"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func codexModelPolicyFailure(reason string) *codexresponses.Failure {
	kind := codexresponses.KindProviderUnavailable
	if reason == "model_blocked" {
		kind = codexresponses.KindModelNotFound
	}
	failure := codexresponses.NewFailure(kind, 0, fmt.Errorf("Codex account model policy denied the request"))
	retryable := false
	failure.UnavailableReason = reason
	failure.Retryable = &retryable
	return failure
}

func (s *Proxy) checkCodexModelPolicy(ctx context.Context, accountID, model string) error {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicCheckModelPolicy, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.CheckModelPolicyCommand{AccountID: accountID, Model: model})).Get()
	if ctx.Err() != nil {
		return clientFailure(ctx.Err())
	}
	policy, ok := value.(accevents.CheckModelPolicyResult)
	if err != nil || !ok {
		return codexModelPolicyFailure("policy_check_unavailable")
	}
	if !policy.Allowed {
		return codexModelPolicyFailure(policy.Reason)
	}
	return nil
}
