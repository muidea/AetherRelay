package biz

import (
	"context"
	"errors"
	"fmt"
	"testing"

	basebiz "aetherrelay/internal/modules/base/biz"
	"aetherrelay/internal/modules/blocks/usageruntime/pkg/common"
	usageevents "aetherrelay/internal/modules/blocks/usageruntime/pkg/events"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
	"github.com/muidea/magicCommon/event"
)

type timeoutStartStore struct{ usage.Store }

func (timeoutStartStore) Start(context.Context, usage.StartRecord) error {
	return fmt.Errorf("%w: %w", usage.ErrStoreUnavailable, context.DeadlineExceeded)
}

func TestUsagePortPreservesFailureCauseForDiagnostics(t *testing.T) {
	hub := event.NewHub(4)
	defer hub.Terminate(context.Background())
	owner := &UsageRuntime{Base: basebiz.New(common.UnitID, hub, nil), runtime: &Runtime{store: timeoutStartStore{usage.NewMemoryStore()}}}
	owner.SubscribeFunc(usageevents.TopicAcquire, func(_ event.Event, r event.Result) { r.Set(usageevents.AcquireResult{}, nil) })
	owner.SubscribeFunc(usageevents.TopicStart, owner.handleStart)
	port, err := usageevents.RequestStore(context.Background(), hub, "test")
	if err != nil {
		t.Fatal(err)
	}
	err = port.Start(context.Background(), usage.StartRecord{EventID: "event", APIKeyID: "key"})
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, usage.ErrStoreUnavailable) || usage.FailureReason(err) != "deadline_exceeded" {
		t.Fatalf("usage port erased the failure cause: %v", err)
	}
}
