package biz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	basebiz "aetherrelay/internal/modules/base/biz"
	"aetherrelay/internal/modules/blocks/usageruntime/pkg/common"
	usageevents "aetherrelay/internal/modules/blocks/usageruntime/pkg/events"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
	"github.com/muidea/magicCommon/event"
)

type timeoutStartStore struct{ usage.Store }

func (timeoutStartStore) Start(context.Context, usage.StartRecord) error {
	return fmt.Errorf("%w: %w", usage.ErrStoreUnavailable, context.DeadlineExceeded)
}

func TestUsagePortPreservesRealQueryFailureDetails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.duckdb")
	store, err := usage.OpenDuckDB(config.UsageStoreConfig{Path: path, MemoryLimit: "256MB", Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Alter only an isolated test DB to trigger a typed driver error.
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE usage_generation`); err != nil {
		t.Fatal(err)
	}
	hub := event.NewHub(4)
	defer hub.Terminate(context.Background())
	owner := &UsageRuntime{Base: basebiz.New(common.UnitID, hub, nil), runtime: &Runtime{store: store}}
	owner.SubscribeFunc(usageevents.TopicAcquire, func(_ event.Event, r event.Result) { r.Set(usageevents.AcquireResult{}, nil) })
	owner.SubscribeFunc(usageevents.TopicEvents, owner.handleEvents)
	port, err := usageevents.RequestStore(context.Background(), hub, "test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = port.Events(context.Background(), usage.EventFilter{UsageFilter: usage.UsageFilter{AllTime: true}})
	details, ok := usage.QueryFailureDetailsFromError(err)
	if usage.FailureReason(err) != "duckdb_catalog" || !errors.Is(err, usage.ErrStoreUnavailable) || !ok || details.Stage != "query" || details.Operation != "events" || details.DriverCode < 0 {
		t.Fatalf("query evidence lost across EventHub: err=%v details=%+v", err, details)
	}
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
