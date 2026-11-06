package biz

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	basebiz "aetherrelay/internal/modules/base/biz"
	"aetherrelay/internal/modules/blocks/usageruntime/pkg/common"
	usageevents "aetherrelay/internal/modules/blocks/usageruntime/pkg/events"
	clientaccess "aetherrelay/internal/pkg/aetherrelayclientaccess"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	trace "aetherrelay/internal/pkg/aetherrelaytrace"
	usage "aetherrelay/internal/pkg/aetherrelayusage"

	"github.com/muidea/magicCommon/event"
)

// Hold a real DuckDB export while its rows/connection are still open, rather
// than merely delaying admission to a fake query handler.
type heldExportStore struct {
	usage.Store
	entered chan struct{}
	once    sync.Once
}

func (s *heldExportStore) ExportCSV(ctx context.Context, f usage.UsageFilter, _ io.Writer) error {
	return s.Store.ExportCSV(ctx, f, &heldExportWriter{store: s, ctx: ctx})
}

type heldExportWriter struct {
	store *heldExportStore
	ctx   context.Context
}

func (w *heldExportWriter) Write(_ []byte) (int, error) {
	w.store.once.Do(func() { close(w.store.entered) })
	<-w.ctx.Done()
	return 0, w.ctx.Err()
}

func TestSlowUsageReadDoesNotBlockAccountingAndCanceledReadsDoNotDispatch(t *testing.T) {
	store, err := usage.OpenDuckDB(config.UsageStoreConfig{
		Path: filepath.Join(t.TempDir(), "usage.duckdb"), MemoryLimit: "256MB", Threads: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	// A long first row forces csv.Writer to flush before rows are exhausted.
	for _, id := range []string{"seed-a", "seed-b"} {
		if err := store.Start(ctx, usage.StartRecord{EventID: id, APIKeyID: "key", Model: strings.Repeat("m", 8192)}); err != nil {
			t.Fatal(err)
		}
	}
	held := &heldExportStore{Store: store, entered: make(chan struct{})}
	hub := event.NewHub(32)
	defer hub.Terminate(ctx)
	owner := &UsageRuntime{Base: basebiz.New(common.UnitID, hub, nil), runtime: &Runtime{store: held}}
	owner.SubscribeFunc(usageevents.TopicAcquire, owner.handleAcquire)
	owner.SubscribeFunc(usageevents.TopicStart, owner.handleStart)
	owner.SubscribeFunc(usageevents.TopicComplete, owner.handleComplete)
	owner.SubscribeFunc(usageevents.TopicWriteTrace, owner.handleWriteTrace)
	owner.SubscribeFunc(usageevents.TopicHealthy, owner.handleHealthy)
	owner.SubscribeFunc(usageevents.TopicClientKeyCreate, owner.handleClientKeyCommand)
	owner.SubscribeFunc(usageevents.TopicClientKeyRotate, owner.handleClientKeyCommand)
	owner.SubscribeFunc(usageevents.TopicClientKeyList, owner.handleClientKeyCommand)
	var reads atomic.Int32
	for topic, handler := range map[string]func(event.Event, event.Result){
		usageevents.TopicExport: owner.handleExport, usageevents.TopicEvents: owner.handleEvents,
		usageevents.TopicCount: owner.handleCount, usageevents.TopicDashboard: owner.handleDashboard,
		usageevents.TopicFilterOptions: owner.handleFilterOptions, usageevents.TopicAllTime: owner.handleAllTime,
		usageevents.TopicExecutionTrace: owner.handleExecutionTrace, usageevents.TopicAnomalies: owner.handleAnomalies,
	} {
		owner.SubscribeFunc(topic, func(ev event.Event, r event.Result) {
			reads.Add(1)
			handler(ev, r)
		})
	}
	port, err := usageevents.RequestStore(ctx, hub, "test")
	if err != nil {
		t.Fatal(err)
	}
	exportCtx, cancelExport := context.WithCancel(ctx)
	defer cancelExport()
	exportDone := make(chan error, 1)
	go func() { exportDone <- port.ExportCSV(exportCtx, usage.UsageFilter{AllTime: true}, io.Discard) }()
	select {
	case <-held.entered:
	case err := <-exportDone:
		t.Fatalf("export finished before it could hold rows: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("export did not start")
	}

	// These durable calls must finish while the export still owns its read.
	writeCtx, cancelWrite := context.WithTimeout(ctx, 3*time.Second)
	defer cancelWrite()
	at := time.Now().UTC()
	if err := port.Start(writeCtx, usage.StartRecord{EventID: "request", APIKeyID: "key", StartedAt: at}); err != nil {
		t.Fatalf("query blocked request admission: %v", err)
	}
	tr := trace.Trace{Context: trace.Context{RequestID: "request"}}
	if err := port.WriteTrace(writeCtx, "request", tr); err != nil {
		t.Fatalf("query blocked trace persistence: %v", err)
	}
	complete := usage.CompleteRecord{EventID: "request", CompletedAt: at.Add(time.Second), HTTPStatus: 200, Outcome: "success", OutputTokens: 7, Trace: &tr}
	if err := port.Complete(writeCtx, complete); err != nil {
		t.Fatalf("query blocked settlement: %v", err)
	}
	if err := port.Complete(writeCtx, complete); !errors.Is(err, usage.ErrEventNotStarted) {
		t.Fatalf("duplicate settlement accepted: %v", err)
	}
	if err := port.WriteTrace(writeCtx, "request", trace.Trace{}); !errors.Is(err, usage.ErrEventNotStarted) {
		t.Fatalf("late checkpoint overwrote settlement: %v", err)
	}
	if err := port.CreateClientAPIKey(writeCtx, usage.ClientAPIKeyRecord{ID: "key", Enabled: true, CreatedAt: at, ProviderAccess: clientaccess.All()}); err != nil {
		t.Fatal(err)
	}
	if err := port.RotateClientAPIKey(writeCtx, "key", "sha256:new", "hint", at); err != nil {
		t.Fatal(err)
	}
	keys, err := port.ListClientAPIKeys(writeCtx)
	if err != nil || keys["key"].Hash != "sha256:new" {
		t.Fatalf("key lifecycle blocked or reordered: keys=%v err=%v", keys, err)
	}
	if !port.Healthy() {
		t.Fatal("query affected store health")
	}

	filter := usage.UsageFilter{AllTime: true}
	for name, query := range map[string]func(context.Context) error{
		"events": func(c context.Context) error {
			_, e := port.Events(c, usage.EventFilter{UsageFilter: filter})
			return e
		},
		"count":     func(c context.Context) error { _, e := port.Count(c, filter); return e },
		"dashboard": func(c context.Context) error { _, e := port.Dashboard(c, filter); return e },
		"export":    func(c context.Context) error { return port.ExportCSV(c, filter, io.Discard) },
		"options": func(c context.Context) error {
			_, e := port.FilterOptions(c, usage.FilterOptionsQuery{AllTime: true})
			return e
		},
		"all-time":  func(c context.Context) error { _, e := port.AllTimeByKey(c); return e },
		"trace":     func(c context.Context) error { _, e := port.ExecutionTrace(c, "request"); return e },
		"anomalies": func(c context.Context) error { _, e := port.Anomalies(c, filter, "cause"); return e },
	} {
		t.Run(name, func(t *testing.T) {
			queryCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
			defer cancel()
			if err := query(queryCtx); err == nil || usage.FailureReason(err) != "command_canceled_before_dispatch" {
				t.Fatalf("query was not bounded by the occupied read lane: %v", err)
			}
		})
	}
	if reads.Load() != 1 {
		t.Fatalf("parallel reads escaped the shared query lane: %d", reads.Load())
	}
	drainCtx, cancelDrain := context.WithTimeout(ctx, 20*time.Millisecond)
	if err := hub.(event.DrainingHub).Drain(drainCtx); err == nil {
		t.Error("drain declared completion while the query was still active")
	}
	cancelDrain()
	cancelExport()
	if err := <-exportDone; err == nil {
		t.Fatal("canceled export returned success")
	}
	drainCtx, cancelDrain = context.WithTimeout(ctx, 3*time.Second)
	defer cancelDrain()
	if err := hub.(event.DrainingHub).Drain(drainCtx); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 1 {
		t.Fatal("canceled queued queries dispatched after the lane was released")
	}
	page, err := port.Events(drainCtx, usage.EventFilter{UsageFilter: filter, EventIDs: []string{"request"}})
	if err != nil || len(page.Events) != 1 || page.Events[0].State != usage.StateCompleted || page.Events[0].OutputTokens != 7 {
		t.Fatalf("read did not recover or settlement was corrupted: page=%+v err=%v", page, err)
	}
	if err := hub.(event.DrainingHub).Drain(drainCtx); err != nil {
		t.Fatal(err)
	}
	owner.Teardown(drainCtx)
}

type heldStartStore struct {
	usage.Store
	entered chan struct{}
	release chan struct{}
}

func (s *heldStartStore) Start(ctx context.Context, record usage.StartRecord) error {
	close(s.entered)
	select {
	case <-s.release:
		return s.Store.Start(ctx, record)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestUsageWriteLanePreservesPendingStartOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hub := event.NewHub(16)
	defer hub.Terminate(context.Background())
	store := &heldStartStore{Store: usage.NewMemoryStore(), entered: make(chan struct{}), release: make(chan struct{})}
	// Release even when an assertion fails, before waiting for Hub shutdown.
	release := sync.OnceFunc(func() { close(store.release) })
	defer release()
	owner := &UsageRuntime{Base: basebiz.New(common.UnitID, hub, nil), runtime: &Runtime{store: store}}
	owner.SubscribeFunc(usageevents.TopicAcquire, owner.handleAcquire)
	owner.SubscribeFunc(usageevents.TopicStart, owner.handleStart)
	owner.SubscribeFunc(usageevents.TopicCount, owner.handleCount)
	owner.SubscribeFunc(usageevents.TopicComplete, owner.handleComplete)
	owner.SubscribeFunc(usageevents.TopicWriteTrace, owner.handleWriteTrace)
	port, err := usageevents.RequestStore(ctx, hub, "test")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- port.Start(ctx, usage.StartRecord{EventID: "pending", APIKeyID: "key"}) }()
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("Start never entered the write lane")
	}
	// Reads can proceed, while later lifecycle writes must wait for Start.
	if count, err := port.Count(ctx, usage.UsageFilter{AllTime: true}); err != nil || count != 0 {
		t.Fatalf("pending write blocked reads: count=%d err=%v", count, err)
	}
	for name, call := range map[string]func(context.Context) error{
		"trace": func(c context.Context) error { return port.WriteTrace(c, "pending", trace.Trace{}) },
		"complete": func(c context.Context) error {
			return port.Complete(c, usage.CompleteRecord{EventID: "pending", HTTPStatus: 200, Outcome: "success"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			waitCtx, cancelWait := context.WithTimeout(ctx, 20*time.Millisecond)
			defer cancelWait()
			if err := call(waitCtx); usage.FailureReason(err) != "command_canceled_before_dispatch" {
				t.Fatalf("write overtook its pending Start: %v", err)
			}
		})
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := port.WriteTrace(ctx, "pending", trace.Trace{}); err != nil {
		t.Fatal(err)
	}
	if err := port.Complete(ctx, usage.CompleteRecord{EventID: "pending", HTTPStatus: 200, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
}
