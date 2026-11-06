package biz

import (
	"context"
	"testing"

	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

func TestUsageRefreshPreservesCredentialFailureClassification(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(map[bool]string{false: "wait_timeout", true: "permanent_credential"}[permanent], func(t *testing.T) {
			hub := event.NewHub(4)
			defer hub.Terminate(context.Background())
			accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
			accounts.Subscribe(accevents.TopicClientIdentity, func(_ event.Event, r event.Result) {
				r.Set(accevents.ClientIdentityProfile{Version: "0.162.1"}, nil)
			})
			accounts.Subscribe(accevents.TopicRefreshToken, func(_ event.Event, r event.Result) {
				class := accevents.ErrorTimeout
				if permanent {
					class = accevents.ErrorInvalidToken
				}
				r.Set(accevents.RefreshTokenResult{ErrorClass: class, PermanentFailure: permanent}, cd.NewError(cd.Unexpected, "refresh failed"))
			})
			recorded := ""
			accounts.Subscribe(accevents.TopicRecordUsageFailure, func(ev event.Event, r event.Result) {
				recorded = ev.Data().(accevents.RecordUsageFailureCommand).Error
				r.Set(accevents.RecordUsageFailureResult{OK: true}, nil)
			})
			upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
			upstream.Subscribe(upevents.TopicGetUsage, func(_ event.Event, r event.Result) {
				r.Set(upevents.GetUsageResult{ErrorClass: upevents.ErrorInvalidToken}, nil)
			})
			s := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, nil)}
			err := s.refreshOneCodexUsage(context.Background(), accevents.UsageCandidate{AccountID: "slot", AccessToken: "expired"}, "manual")
			want := accevents.ErrorTimeout
			if permanent {
				want = accevents.ErrorInvalidToken
			}
			if err == nil || recorded != want {
				t.Fatalf("recorded=%s want=%s err=%v", recorded, want, err)
			}
		})
	}
}
