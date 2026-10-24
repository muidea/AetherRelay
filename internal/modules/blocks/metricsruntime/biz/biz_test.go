package biz

import (
	"context"
	"encoding/json"
	"testing"

	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	metricscommon "aetherrelay/internal/modules/blocks/metricsruntime/pkg/common"
	metricsevents "aetherrelay/internal/modules/blocks/metricsruntime/pkg/events"
	metrics "aetherrelay/internal/pkg/aetherrelaymetrics"
	metricsport "aetherrelay/internal/pkg/aetherrelaymetricsport"
	"aetherrelay/internal/pkg/aetherrelayusage"

	"github.com/muidea/magicCommon/event"
)

func TestHandleRecordAcceptsPostWithoutResult(t *testing.T) {
	runtime, err := NewRuntime(context.Background(), configevents.Bootstrap{}, usage.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	biz := &MetricsRuntime{runtime: runtime}
	ev := event.NewEventWithContext(
		metricsevents.TopicRecord,
		"test",
		metricscommon.UnitID,
		event.NewHeader(),
		context.Background(),
		metricsevents.RecordCommand{Kind: metricsevents.UsageStoreHealthy, Healthy: true},
	)

	// EventHub.Post invokes observers with result=nil; handlers must not reply.
	biz.handleRecord(ev, nil)
}

func TestRequestActivityPortUpdatesSnapshotsSynchronously(t *testing.T) {
	runtime, err := NewRuntime(context.Background(), configevents.Bootstrap{}, usage.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	biz := &MetricsRuntime{runtime: runtime}
	hub := event.NewHub(8)
	defer hub.Terminate(context.Background())
	observer := event.NewSimpleObserver(metricscommon.UnitID, hub)
	for topic, handler := range map[string]func(event.Event, event.Result){
		metricsevents.TopicAcquire:         biz.handleAcquire,
		metricsevents.TopicRequestActivity: biz.handleRequestActivity,
		metricsevents.TopicStats:           biz.handleStats,
	} {
		if err := observer.Subscribe(topic, handler); err != nil {
			t.Fatal(err)
		}
	}
	port, err := metricsevents.RequestPort(context.Background(), hub, "test")
	if err != nil {
		t.Fatal(err)
	}
	assertInflight := func(want int64) {
		t.Helper()
		data, err := port.StatsJSON()
		if err != nil {
			t.Fatal(err)
		}
		var stats metrics.StatsJSON
		if err := json.Unmarshal(data, &stats); err != nil {
			t.Fatal(err)
		}
		if stats.Requests.Inflight != want {
			t.Fatalf("inflight = %d, want %d", stats.Requests.Inflight, want)
		}
	}
	assertInflight(0)
	if err := port.BeginRequest(); err != nil {
		t.Fatal(err)
	}
	if err := port.BeginRequest(); err != nil {
		t.Fatal(err)
	}
	assertInflight(2)
	if err := port.EndRequest(); err != nil {
		t.Fatal(err)
	}
	assertInflight(1)
	if err := port.EndRequest(); err != nil {
		t.Fatal(err)
	}
	assertInflight(0)
	if err := port.EndRequest(); err == nil {
		t.Fatal("unmatched finish must fail across EventHub")
	}
	assertInflight(0)
}

// CP-OBS-017: production EventHub port retains unknown cache presence.
func TestTokenObservationPresenceAcrossEventHub(t *testing.T) {
	runtime, err := NewRuntime(context.Background(), configevents.Bootstrap{}, usage.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	biz := &MetricsRuntime{runtime: runtime}
	hub := event.NewHub(8)
	defer hub.Terminate(context.Background())
	observer := event.NewSimpleObserver(metricscommon.UnitID, hub)
	for topic, handler := range map[string]func(event.Event, event.Result){metricsevents.TopicAcquire: biz.handleAcquire, metricsevents.TopicRecord: biz.handleRecord, metricsevents.TopicStats: biz.handleStats} {
		if err := observer.Subscribe(topic, handler); err != nil {
			t.Fatal(err)
		}
	}
	port, err := metricsevents.RequestPort(context.Background(), hub, "test")
	if err != nil {
		t.Fatal(err)
	}
	observed, ok := port.(metricsport.TokenObservationReporter)
	if !ok {
		t.Fatal("port discards cache presence")
	}
	observed.RecordTokensObserved("chatgptweb", "gpt-6", 100, 20, 0, 0, false, true)
	observed.RecordTokensObserved("chatgptweb", "gpt-6", 100, 0, 0, 0, true, false)
	observed.RecordTokensObserved("chatgptweb", "gpt-6", 100, 20, 0, 0, true, true)
	data, err := port.StatsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var stats metrics.StatsJSON
	if err = json.Unmarshal(data, &stats); err != nil {
		t.Fatal(err)
	}
	cache := stats.Cache.ByProvider["chatgptweb"]
	if cache.Miss != 1 || cache.Hit != 0 {
		t.Fatalf("cache=%+v", cache)
	}
}
