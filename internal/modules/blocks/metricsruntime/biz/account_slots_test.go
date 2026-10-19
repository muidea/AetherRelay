package biz

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	metricscommon "aetherrelay/internal/modules/blocks/metricsruntime/pkg/common"
	metricsevents "aetherrelay/internal/modules/blocks/metricsruntime/pkg/events"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

func TestSlotMetricsPullOwnerAndDoNotRetainDeletedAccounts(t *testing.T) {
	hub := event.NewHub(16)
	defer hub.Terminate(context.Background())
	runtime, err := NewRuntime(context.Background(), configevents.Bootstrap{}, usage.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	s := &MetricsRuntime{Base: basebiz.New(metricscommon.UnitID, hub, nil), runtime: runtime}
	observer := event.NewSimpleObserver(metricscommon.UnitID, hub)
	observer.Subscribe(metricsevents.TopicAcquire, s.handleAcquire)
	observer.Subscribe(metricsevents.TopicStats, s.handleStats)
	observer.Subscribe(metricsevents.TopicPrometheus, s.handlePrometheus)
	pool := event.NewSimpleObserver(acccommon.UnitID, hub)
	snapshot := accevents.LeaseSnapshotResult{At: time.Now(), Accounts: []accevents.AccountSlots{{AccountID: "owner-id", MaxConcurrency: 6, Inflight: 2, WebsocketIdle: 1, OldestLeaseMS: 12500, ReleaseFailures: 2, Leases: []accevents.LeaseView{{LeaseID: "lease-not-a-label", LeaseRequest: accevents.LeaseRequest{RequestID: "request-not-a-label"}}}}}}
	unavailable := false
	pool.Subscribe(accevents.TopicLeaseSnapshot, func(_ event.Event, r event.Result) {
		if unavailable {
			r.Set(nil, cd.NewError(cd.Unexpected, "owner snapshot unavailable"))
			return
		}
		r.Set(snapshot, nil)
	})
	port, err := metricsevents.RequestPort(context.Background(), hub, "test")
	if err != nil {
		t.Fatal(err)
	}
	data, err := port.StatsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var stats struct {
		Slots accountSlotsSnapshot `json:"codex_account_slots"`
	}
	if err := json.Unmarshal(data, &stats); err != nil {
		t.Fatal(err)
	}
	if !stats.Slots.Available || len(stats.Slots.Accounts) != 1 || stats.Slots.Accounts[0].Inflight != 2 || stats.Slots.Accounts[0].Leases[0].RequestID != "request-not-a-label" {
		t.Fatal(string(data))
	}
	prom, err := port.Prometheus()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`aetherrelay_codex_account_limit{account_id="owner-id"} 6`, `aetherrelay_codex_account_inflight{account_id="owner-id"} 2`, `aetherrelay_codex_account_websocket_idle{account_id="owner-id"} 1`, `aetherrelay_codex_account_oldest_lease_seconds{account_id="owner-id"} 12.500`, `aetherrelay_codex_account_release_failures_total{account_id="owner-id"} 2`} {
		if !strings.Contains(string(prom), want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(string(prom), "request-not-a-label") || strings.Contains(string(prom), "lease-not-a-label") {
		t.Fatal("unbounded identity label")
	}
	snapshot.Accounts = []accevents.AccountSlots{}
	prom, err = port.Prometheus()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prom), `account_id="owner-id"`) {
		t.Fatal("retained deleted account")
	}
	unavailable = true
	prom, err = port.Prometheus()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prom), "aetherrelay_codex_account_slots_available 0") {
		t.Fatal(string(prom))
	}
	data, err = port.StatsJSON()
	if err != nil {
		t.Fatal(err)
	}
	stats = struct {
		Slots accountSlotsSnapshot `json:"codex_account_slots"`
	}{}
	if err := json.Unmarshal(data, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Slots.Available || !strings.Contains(stats.Slots.Error, "owner snapshot unavailable") {
		t.Fatal(string(data))
	}
}
