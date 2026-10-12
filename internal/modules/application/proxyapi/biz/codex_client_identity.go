package biz

import (
	"context"
	"fmt"

	codexcommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	codexevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
)

// Background candidates can wait behind the concurrency limit. Resolve the
// shared maximum again at dispatch, including after a credential refresh.
func (s *Proxy) currentCodexClientIdentity(ctx context.Context) (codexevents.ClientIdentityProfile, error) {
	value, err := s.SendEvent(event.NewEventWithContext(codexevents.TopicClientIdentity, s.ID(), codexcommon.UnitID, event.NewHeader(), ctx, nil)).Get()
	if err != nil {
		return codexevents.ClientIdentityProfile{}, fmt.Errorf("resolve Codex client identity failed")
	}
	identity, ok := value.(codexevents.ClientIdentityProfile)
	if !ok || identity.Version == "" {
		return codexevents.ClientIdentityProfile{}, fmt.Errorf("invalid Codex client identity result")
	}
	return identity, nil
}
