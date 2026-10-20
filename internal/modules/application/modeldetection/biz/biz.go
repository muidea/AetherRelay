// Package biz owns detection orchestration and assessment, never credentials.
package biz

import (
	"context"
	"strings"
	"sync"
	"time"

	"aetherrelay/internal/modules/application/modeldetection/pkg/common"
	"aetherrelay/internal/modules/application/modeldetection/pkg/events"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	proxy "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	basebiz "aetherrelay/internal/modules/base/biz"
	"aetherrelay/internal/pkg/modeltrace"
	"aetherrelay/internal/pkg/modeltrace/assets"
	"github.com/google/uuid"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type detectionJob struct {
	starting bool
	report   events.Report
	key      string
	created  time.Time
}
type Detection struct {
	basebiz.Base
	mu            sync.Mutex
	engine        *modeltrace.Engine
	jobs          map[string]*detectionJob
	keys          map[string]string
	stopping      bool
	starts        sync.WaitGroup
	startsDrained chan struct{}
}

func New(hub event.Hub, bg task.BackgroundRoutine) (*Detection, *cd.Error) {
	engine, err := assets.Load()
	if err != nil {
		return nil, cd.NewError(cd.Unexpected, "modeltrace reference assets invalid")
	}
	b := &Detection{Base: basebiz.New(common.UnitID, hub, bg), engine: engine, jobs: map[string]*detectionJob{}, keys: map[string]string{}}
	b.SubscribeFunc(events.TopicStart, b.start)
	b.SubscribeFunc(events.TopicGet, b.get)
	b.SubscribeFunc(events.TopicCancel, b.cancel)
	return b, nil
}
func (b *Detection) Run(ctx context.Context) *cd.Error {
	b.Timer(ctx, time.Second, 0, b.tick)
	return nil
}
func (b *Detection) prune() {
	now := time.Now()
	for id, j := range b.jobs {
		if j.report.Done {
			at, _ := time.Parse(time.RFC3339Nano, j.report.ExpiresAt)
			if !at.IsZero() && !now.Before(at) {
				delete(b.jobs, id)
				delete(b.keys, j.key)
			}
		}
	}
}
func (b *Detection) start(ev event.Event, r event.Result) {
	if r == nil {
		return
	}
	cmd, ok := ev.Data().(events.StartCommand)
	if !ok {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid detection command"))
		return
	}
	cmd.SlotID = strings.TrimSpace(cmd.SlotID)
	if cmd.Reasoning == "" {
		cmd.Reasoning = "upstream_default"
	}
	if cmd.SlotID == "" || len(cmd.SlotID) > 256 || cmd.Model == "" || len(cmd.Model) > 256 || strings.TrimSpace(cmd.Model) != cmd.Model || len(cmd.IdempotencyKey) == 0 || len(cmd.IdempotencyKey) > 128 || strings.ContainsAny(cmd.IdempotencyKey, "\r\n\x00") {
		r.Set(events.Result{Status: 400, ErrorCode: "invalid_probe_request"}, nil)
		return
	}
	b.mu.Lock()
	b.prune()
	if b.stopping {
		b.mu.Unlock()
		r.Set(events.Result{Status: 503, ErrorCode: "probe_stopping"}, nil)
		return
	}
	if id := b.keys[cmd.IdempotencyKey]; id != "" {
		out := b.jobs[id].report.Clone()
		b.mu.Unlock()
		if out.TargetSlotID != cmd.SlotID || out.Model != cmd.Model || out.Reasoning != cmd.Reasoning {
			r.Set(events.Result{Status: 409, ErrorCode: "idempotency_conflict"}, nil)
		} else {
			r.Set(events.Result{Status: 202, Report: out}, nil)
		}
		return
	}
	if len(b.jobs) >= 100 {
		b.mu.Unlock()
		r.Set(events.Result{Status: 429, ErrorCode: "probe_capacity"}, nil)
		return
	}
	// Reservation precedes the synchronous execution request; duplicate submissions
	// observe the same ID even if the first acknowledgement is delayed.
	id := uuid.NewString()
	j := &detectionJob{starting: true, key: cmd.IdempotencyKey, created: time.Now(), report: events.Report{ModelTraceReport: proxy.ModelTraceReport{ProbeID: id, TargetSlotID: cmd.SlotID, Model: cmd.Model, Reasoning: cmd.Reasoning, State: "preflight"}, Bank: b.engine.Snapshot(), PathValidation: "NOT_RUN", QualityValidation: "NOT_RUN"}}
	b.jobs[id] = j
	b.keys[j.key] = id
	b.starts.Add(1)
	b.mu.Unlock()
	defer b.starts.Done()
	challenges, err := modeltrace.GenerateChallenges(nil)
	if err != nil {
		b.discard(id, j.key)
		r.Set(events.Result{Status: 503, ErrorCode: "challenge_generation_failed"}, nil)
		return
	}
	value, sendErr := b.SendEvent(event.NewEventWithContext(proxy.TopicStartModelTraceProbe, b.ID(), proxycommon.UnitID, event.NewHeader(), ev.Context(), proxy.StartModelTraceProbeCommand{SlotID: cmd.SlotID, Model: cmd.Model, Reasoning: cmd.Reasoning, IdempotencyKey: cmd.IdempotencyKey, ProbeID: id, Challenges: challenges})).Get()
	receipt, ok := value.(proxy.ModelTraceProbeResult)
	if sendErr != nil || !ok { // Keep the caller-generated ID for recovery; do not replay unknown work.
		b.mu.Lock()
		j.starting = false
		j.report.ErrorCode = "execution_status_unknown"
		out := j.report.Clone()
		b.mu.Unlock()
		r.Set(events.Result{Status: 202, Report: out}, nil)
		return
	}
	if receipt.Status >= 400 {
		b.discard(id, j.key)
		r.Set(events.Result{Status: receipt.Status, ErrorCode: receipt.ErrorCode}, nil)
		return
	}
	b.update(id, receipt.Report)
	b.mu.Lock()
	out := j.report.Clone()
	b.mu.Unlock()
	r.Set(events.Result{Status: 202, Report: out}, nil)
}
func (b *Detection) discard(id, key string) {
	b.mu.Lock()
	delete(b.jobs, id)
	delete(b.keys, key)
	b.mu.Unlock()
}
func (b *Detection) update(id string, receipt proxy.ModelTraceReport) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j := b.jobs[id]
	if j == nil {
		return
	}
	if j.report.Done {
		j.report.Historical = j.report.Historical || receipt.Historical
		return
	}
	j.starting = false
	j.report.ModelTraceReport = receipt.Clone()
	j.report.SchemaVersion = "aetherrelay-modeltrace-report-v1"
	if !receipt.Done {
		return
	}
	var outputs []modeltrace.Output
	for _, sample := range receipt.Samples {
		if sample.State == "completed" {
			outputs = append(outputs, modeltrace.Output{Text: sample.OutputText, ExpectedCount: sample.ExpectedCount})
		} else {
			outputs = append(outputs, modeltrace.Output{Text: "", ExpectedCount: sample.ExpectedCount})
		}
	}
	j.report.ParsedCounts = make([]int, len(outputs))
	for i, output := range outputs {
		j.report.ParsedCounts[i] = len(modeltrace.ParseNumbers(output.Text))
	}
	if len(outputs) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		result, err := b.engine.Score(ctx, modeltrace.Request{Outputs: outputs, Parser: modeltrace.CoreCompatible, RequestedModel: receipt.Model})
		cancel()
		if err == nil {
			j.report.Result = result
		} else if j.report.ErrorCode == "" {
			j.report.ErrorCode = "no_usable_outputs"
		}
	}
}
func (b *Detection) refresh(ctx context.Context, id, slot string) {
	b.mu.Lock()
	j := b.jobs[id]
	starting := j == nil || j.starting
	b.mu.Unlock()
	if starting {
		return
	}

	value, err := b.SendEvent(event.NewEventWithContext(proxy.TopicGetModelTraceProbe, b.ID(), proxycommon.UnitID, event.NewHeader(), ctx, proxy.GetModelTraceProbeCommand{SlotID: slot, ProbeID: id})).Get()
	receipt, ok := value.(proxy.ModelTraceProbeResult)
	if err == nil && ok && receipt.Status == 200 {
		b.update(id, receipt.Report)
	} else if err == nil && ok && receipt.Status == 404 {
		b.mu.Lock()
		if j := b.jobs[id]; j != nil && !j.report.Done && !j.starting {
			now := time.Now().UTC()
			j.report.Done = true
			j.report.State = "finished"
			j.report.ErrorCode = "execution_not_found"
			j.report.FinishedAt = now.Format(time.RFC3339Nano)
			j.report.ExpiresAt = now.Add(30 * time.Minute).Format(time.RFC3339Nano)
		}
		b.mu.Unlock()
	}
}
func (b *Detection) get(ev event.Event, r event.Result) {
	if r == nil {
		return
	}
	cmd, ok := ev.Data().(events.GetCommand)
	if !ok {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid detection query"))
		return
	}
	cmd.SlotID = strings.TrimSpace(cmd.SlotID)
	b.mu.Lock()
	b.prune()
	j := b.jobs[cmd.ProbeID]
	if cmd.ProbeID == "latest" {
		for _, candidate := range b.jobs {
			if candidate.report.TargetSlotID == cmd.SlotID && (j == nil || candidate.created.After(j.created)) {
				j = candidate
			}
		}
	}
	if j == nil || j.report.TargetSlotID != cmd.SlotID {
		b.mu.Unlock()
		r.Set(events.Result{Status: 404, ErrorCode: "probe_not_found"}, nil)
		return
	}
	id := j.report.ProbeID
	b.mu.Unlock()
	b.refresh(ev.Context(), id, cmd.SlotID)
	b.mu.Lock()
	out := j.report.Clone()
	b.mu.Unlock()
	r.Set(events.Result{Status: 200, Report: out}, nil)
}
func (b *Detection) cancel(ev event.Event, r event.Result) {
	if r == nil {
		return
	}
	cmd, ok := ev.Data().(events.CancelCommand)
	if !ok {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid detection cancel"))
		return
	}
	b.mu.Lock()
	j := b.jobs[cmd.ProbeID]
	if j == nil || j.report.TargetSlotID != strings.TrimSpace(cmd.SlotID) {
		b.mu.Unlock()
		r.Set(events.Result{Status: 404, ErrorCode: "probe_not_found"}, nil)
		return
	}
	b.mu.Unlock()
	v, err := b.SendEvent(event.NewEventWithContext(proxy.TopicCancelModelTraceProbe, b.ID(), proxycommon.UnitID, event.NewHeader(), ev.Context(), proxy.CancelModelTraceProbeCommand{SlotID: cmd.SlotID, ProbeID: cmd.ProbeID})).Get()
	receipt, ok := v.(proxy.ModelTraceProbeResult)
	if err != nil || !ok {
		r.Set(events.Result{Status: 503, ErrorCode: "probe_unavailable"}, nil)
		return
	}
	if receipt.Status >= 400 {
		r.Set(events.Result{Status: receipt.Status, ErrorCode: receipt.ErrorCode}, nil)
		return
	}
	b.update(cmd.ProbeID, receipt.Report)
	b.mu.Lock()
	out := j.report.Clone()
	b.mu.Unlock()
	r.Set(events.Result{Status: 202, Report: out}, nil)
}
func (b *Detection) tick() {
	b.mu.Lock()
	b.prune()
	var active []events.GetCommand
	for id, j := range b.jobs {
		if !j.report.Done {
			active = append(active, events.GetCommand{SlotID: j.report.TargetSlotID, ProbeID: id})
		}
	}
	b.mu.Unlock()
	for _, job := range active {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		b.refresh(ctx, job.ProbeID, job.SlotID)
		cancel()
	}
}
func (b *Detection) BeginShutdown(context.Context) { b.mu.Lock(); b.stopping = true; b.mu.Unlock() }
func (b *Detection) Quiesce(ctx context.Context) *cd.Error {
	b.BeginShutdown(ctx)
	b.mu.Lock()
	if b.startsDrained == nil {
		b.startsDrained = make(chan struct{})
		go func() { b.starts.Wait(); close(b.startsDrained) }()
	}
	startsDone := b.startsDrained
	b.mu.Unlock()
	select {
	case <-startsDone:
	case <-ctx.Done():
		return cd.NewError(cd.Timeout, "detection submissions have not drained")
	}

	b.mu.Lock()
	var active []events.GetCommand
	for id, j := range b.jobs {
		if !j.report.Done {
			active = append(active, events.GetCommand{SlotID: j.report.TargetSlotID, ProbeID: id})
		}
	}
	b.mu.Unlock()
	for _, job := range active {
		v, err := b.SendEvent(event.NewEventWithContext(proxy.TopicCancelModelTraceProbe, b.ID(), proxycommon.UnitID, event.NewHeader(), ctx, proxy.CancelModelTraceProbeCommand{SlotID: job.SlotID, ProbeID: job.ProbeID})).Get()
		receipt, ok := v.(proxy.ModelTraceProbeResult)
		if err != nil || !ok {
			return cd.NewError(cd.Timeout, "detection cancellation unacknowledged")
		}
		if receipt.Status != 404 {
			b.update(job.ProbeID, receipt.Report)
		}
	}
	for _, job := range active {
		for {
			v, err := b.SendEvent(event.NewEventWithContext(proxy.TopicDrainModelTraceProbe, b.ID(), proxycommon.UnitID, event.NewHeader(), ctx, proxy.DrainModelTraceProbeCommand{SlotID: job.SlotID, ProbeID: job.ProbeID})).Get()
			receipt, ok := v.(proxy.ModelTraceProbeResult)
			if err != nil || !ok {
				return cd.NewError(cd.Timeout, "detection execution not drained")
			}
			if receipt.Status == 404 {
				break
			}
			b.update(job.ProbeID, receipt.Report)
			if receipt.Report.Done {
				break
			}
			select {
			case <-ctx.Done():
				return cd.NewError(cd.Timeout, "detection execution not drained")
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	return nil
}

func (b *Detection) Teardown(ctx context.Context) {
	if err := b.Quiesce(ctx); err != nil {
		panic(err)
	}
	b.UnsubscribeFunc(events.TopicStart)
	b.UnsubscribeFunc(events.TopicGet)
	b.UnsubscribeFunc(events.TopicCancel)
}
