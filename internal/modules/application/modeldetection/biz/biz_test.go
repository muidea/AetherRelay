package biz

import (
	proxy "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	"context"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/modeldetection/pkg/events"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestDetectionLifecycleRejectsWorkAfterStop(t *testing.T) {
	hub := event.NewHub(8)
	bg := task.NewBackgroundRoutine(2)
	b, err := New(hub, bg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	defer func() { b.Teardown(ctx); bg.Shutdown(ctx); hub.Terminate(ctx) }()
	if b.engine.Snapshot().ModelCount != 18 {
		t.Fatal("bank not assembled")
	}
	b.BeginShutdown(ctx)
	result := event.NewResult(events.TopicStart, "test", b.ID())
	b.start(event.NewEventWithContext(events.TopicStart, "test", b.ID(), event.NewHeader(), ctx, events.StartCommand{SlotID: "slot", Model: "gpt-test", IdempotencyKey: "one"}), result)
	v, e := result.Get()
	if e != nil || v.(events.Result).Status != 503 {
		t.Fatal("stopped module accepted work")
	}
	if e := b.Quiesce(ctx); e != nil {
		t.Fatal(e)
	}
	b.start(nil, nil)
	b.get(nil, nil)
	b.cancel(nil, nil)
}
func TestDetectionRetainsFailedSamplePositions(t *testing.T) {
	hub := event.NewHub(8)
	bg := task.NewBackgroundRoutine(2)
	b, err := New(hub, bg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	defer func() { b.Teardown(ctx); bg.Shutdown(ctx); hub.Terminate(ctx) }()
	b.jobs["partial"] = &detectionJob{report: events.Report{PathValidation: "NOT_RUN", QualityValidation: "NOT_RUN"}}
	b.update("partial", proxy.ModelTraceReport{ProbeID: "partial", Done: true, Model: "unlisted-model", ErrorCode: "timeout", Samples: []proxy.ModelTraceSample{{State: "completed", ExpectedCount: 300, OutputText: strings.Repeat("1,", 300)}, {State: "failed", ExpectedCount: 300, ErrorCode: "timeout"}, {State: "not_started", ExpectedCount: 300}}})
	report := b.jobs["partial"].report
	if len(report.ParsedCounts) != 3 || report.ParsedCounts[0] != 300 {
		t.Fatal("parse counts missing")
	}
	if report.Result == nil || report.Result.Analysis.Used != 1 || len(report.Result.Analysis.Diagnostics) != 3 || report.Result.Analysis.Diagnostics[1].Accepted || report.Result.RequestedModelInBank || report.ErrorCode != "timeout" || report.QualityValidation != "NOT_RUN" {
		t.Fatal("failed samples were hidden or given a quality conclusion")
	}
	b.jobs["invalid"] = &detectionJob{}
	b.update("invalid", proxy.ModelTraceReport{Done: true, Samples: []proxy.ModelTraceSample{{State: "completed", ExpectedCount: 300, OutputText: "refused"}}})
	invalid := b.jobs["invalid"].report
	if invalid.Result != nil || invalid.ErrorCode != "no_usable_outputs" || len(invalid.ParsedCounts) != 1 || invalid.ParsedCounts[0] != 0 {
		t.Fatal("invalid output diagnostics missing")
	}

}
