package biz

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	detectionbiz "aetherrelay/internal/modules/application/modeldetection/biz"
	detectioncommon "aetherrelay/internal/modules/application/modeldetection/pkg/common"
	detection "aetherrelay/internal/modules/application/modeldetection/pkg/events"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	proxy "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	acc "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	up "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type modeltraceFixture struct {
	mu                                      sync.Mutex
	denied, version                         string
	starts, releases, health, closeFailures int
	snapshots                               map[string]up.SampleSnapshotResult
	sessions                                map[string]bool
	pending                                 bool
}

func newModelTraceFixture(t *testing.T) (*Proxy, *modeltraceFixture) {
	t.Helper()
	hub := event.NewHub(64)
	bg := task.NewBackgroundRoutine(8)
	p := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, bg)}
	p.SubscribeFunc(proxy.TopicStartModelTraceProbe, p.handleStartModelTraceProbe)
	p.SubscribeFunc(proxy.TopicGetModelTraceProbe, p.handleGetModelTraceProbe)
	p.SubscribeFunc(proxy.TopicCancelModelTraceProbe, p.handleCancelModelTraceProbe)
	p.SubscribeFunc(proxy.TopicDrainModelTraceProbe, p.handleDrainModelTraceProbe)
	d, err := detectionbiz.New(hub, bg)
	if err != nil {
		t.Fatal(err)
	}
	f := &modeltraceFixture{version: "v1", snapshots: map[string]up.SampleSnapshotResult{}, sessions: map[string]bool{}}
	a := event.NewSimpleObserver(acccommon.UnitID, hub)
	a.Subscribe(acc.TopicProbeAdmission, func(ev event.Event, r event.Result) {
		cmd := ev.Data().(acc.ProbeAdmissionCommand)
		f.mu.Lock()
		defer f.mu.Unlock()
		out := acc.ProbeAdmissionResult{Allowed: f.denied == "", Reason: f.denied, ConfigVersion: f.version}
		if cmd.Acquire && out.Allowed {
			out.Credential = acc.AcquireResult{AccountID: cmd.AccountID, LeaseID: "lease", AccessToken: "fixture-secret"}
		}
		r.Set(out, nil)
	})
	a.Subscribe(acc.TopicRelease, func(_ event.Event, r event.Result) {
		f.mu.Lock()
		f.releases++
		f.mu.Unlock()
		r.Set(acc.ReleaseResult{Released: true}, nil)
	})
	for _, topic := range []string{acc.TopicRecordResult, acc.TopicRecordTransportCapability} {
		a.Subscribe(topic, func(_ event.Event, r event.Result) { f.mu.Lock(); f.health++; f.mu.Unlock(); r.Set(nil, nil) })
	}
	u := event.NewSimpleObserver(upcommon.UnitID, hub)
	u.Subscribe(up.TopicSampleStart, func(ev event.Event, r event.Result) {
		cmd := ev.Data().(up.SampleStartCommand)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.sessions[cmd.Request.SessionHash] {
			t.Error("session reused")
		}
		f.sessions[cmd.Request.SessionHash] = true
		f.starts++
		var body struct {
			Model, Input string
			Instructions *string `json:"instructions"`
			Reasoning    struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if json.Unmarshal(cmd.Request.Body, &body) != nil || body.Model != "gpt-6.1-sol" || body.Input == "" || body.Reasoning.Effort != "low" || cmd.Request.AccessToken != "fixture-secret" {
			t.Error("invalid strict sample")
		}
		if body.Instructions == nil || *body.Instructions != "" {
			t.Error("CP-REQ-003: sample must explicitly carry empty string instructions")
		}
		text := strings.Repeat("1,", 332)
		payload, _ := json.Marshal(struct {
			Status, Model string
			Output        []struct {
				Type, Role string
				Content    []struct{ Type, Text string }
			}
			Usage proxy.ModelTraceUsage
		}{Status: "completed", Model: "claimed", Output: []struct {
			Type, Role string
			Content    []struct{ Type, Text string }
		}{{Type: "message", Role: "assistant", Content: []struct{ Type, Text string }{{Type: "output_text", Text: text}}}}, Usage: proxy.ModelTraceUsage{InputTokens: 10, OutputTokens: 300, TotalTokens: 310}})
		// Marshal test wire with lowercase field names, like native Responses.
		payload = []byte(strings.NewReplacer(`"Status":`, `"status":`, `"Model":`, `"model":`, `"Output":`, `"output":`, `"Type":`, `"type":`, `"Role":`, `"role":`, `"Content":`, `"content":`, `"Text":`, `"text":`, `"Usage":`, `"usage":`).Replace(string(payload)))
		f.snapshots[cmd.ProbeID] = up.SampleSnapshotResult{Done: !f.pending, Body: payload, EffectiveRequestSHA256: strings.Repeat("a", 64)}
		r.Set(up.SampleStartResult{ProbeID: cmd.ProbeID}, nil)
	})
	u.Subscribe(up.TopicSampleSnapshot, func(ev event.Event, r event.Result) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.Set(f.snapshots[ev.Data().(up.SampleSnapshotCommand).ProbeID].Clone(), nil)
	})
	u.Subscribe(up.TopicSampleClose, func(ev event.Event, r event.Result) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.closeFailures > 0 {
			f.closeFailures--
			r.Set(nil, cd.NewError(cd.Timeout, "worker still exiting"))
			return
		}
		id := ev.Data().(up.SampleCloseCommand).ProbeID
		snap := f.snapshots[id]
		delete(f.snapshots, id)
		r.Set(up.SampleCloseResult{Closed: true, Snapshot: snap}, nil)
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if e := d.Quiesce(ctx); e != nil {
			t.Error(e)
		}
		if e := p.Quiesce(ctx); e != nil {
			t.Error(e)
		}
		d.Teardown(ctx)
		bg.Shutdown(ctx)
		hub.Terminate(ctx)
	})
	return p, f
}
func detectionCall(t *testing.T, p *Proxy, topic string, data interface{}) detection.Result {
	t.Helper()
	v, err := p.SendEvent(event.NewEventWithContext(topic, p.ID(), detectioncommon.UnitID, event.NewHeader(), context.Background(), data)).Get()
	if err != nil {
		t.Fatal(err)
	}
	return v.(detection.Result)
}
func waitModelTrace(t *testing.T, p *Proxy, id string) detection.Report {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		p.tickModelTrace()
		out := detectionCall(t, p, detection.TopicGet, detection.GetCommand{SlotID: "slot", ProbeID: id})
		if out.Report.Done {
			return out.Report
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("probe did not finish")
	return detection.Report{}
}
func TestModelTraceThreeChallengesAndReportIsolation(t *testing.T) {
	p, f := newModelTraceFixture(t)
	cmd := detection.StartCommand{SlotID: " slot ", Model: "gpt-6.1-sol", Reasoning: "low", IdempotencyKey: "one"}
	start := detectionCall(t, p, detection.TopicStart, cmd)
	if start.Status != 202 {
		t.Fatal(start)
	}
	duplicate := detectionCall(t, p, detection.TopicStart, cmd)
	if duplicate.Report.ProbeID != start.Report.ProbeID {
		t.Fatal("idempotency lost")
	}
	cmd.Reasoning = "high"
	if out := detectionCall(t, p, detection.TopicStart, cmd); out.Status != 409 {
		t.Fatal(out)
	}
	done := waitModelTrace(t, p, start.Report.ProbeID)
	if done.Result == nil || done.Result.Analysis.Used != 3 || done.Result.RequestedModelInBank || done.PathValidation != "NOT_RUN" {
		t.Fatal("incorrect assessment", done.ErrorCode)
	}
	f.mu.Lock()
	if f.starts != 3 || f.releases != 3 || f.health != 0 {
		t.Error(f.starts, f.releases, f.health)
	}
	f.mu.Unlock()
	wire, _ := json.Marshal(done)
	if strings.Contains(string(wire), "fixture-secret") {
		t.Fatal("credential leaked")
	}
	done.Result.Analysis.Results[0].Model = "mutated"
	done.Samples[0].Prompt = "mutated"
	again := detectionCall(t, p, detection.TopicGet, detection.GetCommand{SlotID: "slot", ProbeID: start.Report.ProbeID})
	if again.Report.Result.Analysis.Results[0].Model == "mutated" || again.Report.Samples[0].Prompt == "mutated" {
		t.Fatal("shared report")
	}
	if out := detectionCall(t, p, detection.TopicGet, detection.GetCommand{SlotID: "other", ProbeID: start.Report.ProbeID}); out.Status != 404 {
		t.Fatal("cross-slot access")
	}
}
func TestModelTraceAdmissionAndCleanupBarrier(t *testing.T) {
	t.Run("denied", func(t *testing.T) {
		p, f := newModelTraceFixture(t)
		f.denied = "model_blocked"
		start := detectionCall(t, p, detection.TopicStart, detection.StartCommand{SlotID: "slot", Model: "gpt-6.1-sol", Reasoning: "low", IdempotencyKey: "denied"})
		out := waitModelTrace(t, p, start.Report.ProbeID)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.starts != 0 || f.releases != 0 || out.AdmissionReason != "model_blocked" || out.Result != nil {
			t.Fatal("denied probe executed")
		}
	})
	t.Run("cancel waits for close", func(t *testing.T) {
		p, f := newModelTraceFixture(t)
		f.pending = true
		start := detectionCall(t, p, detection.TopicStart, detection.StartCommand{SlotID: "slot", Model: "gpt-6.1-sol", Reasoning: "low", IdempotencyKey: "cancel"})
		deadline := time.Now().Add(time.Second)
		for {
			f.mu.Lock()
			started := f.starts > 0
			f.mu.Unlock()
			if started {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no start")
			}
			time.Sleep(time.Millisecond)
		}
		f.mu.Lock()
		f.closeFailures = 1
		f.mu.Unlock()
		detectionCall(t, p, detection.TopicCancel, detection.CancelCommand{SlotID: "slot", ProbeID: start.Report.ProbeID})
		p.tickModelTrace()
		f.mu.Lock()
		released := f.releases
		f.mu.Unlock()
		if released != 0 {
			t.Fatal("lease released before close")
		}
		out := waitModelTrace(t, p, start.Report.ProbeID)
		if out.ErrorCode != "canceled" {
			t.Fatal(out.ErrorCode)
		}
	})
}
