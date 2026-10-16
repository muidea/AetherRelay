package biz

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"aetherrelay/internal/modules/blocks/codexupstream/pkg/steering"
	"github.com/google/uuid"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

const steeringTTL = 30 * time.Minute
const steeringBudget = 120 * time.Second

type codexSteeringTask struct {
	report          proxyevents.SteeringReport
	key             string
	created         time.Time
	finished        time.Time
	ctx             context.Context
	cancel          context.CancelFunc
	leaseID         string
	monitoring      bool
	upstreamStarted bool
	usageMerged     bool
	finalizing      bool
}

type steeringJobs struct {
	mu       sync.Mutex
	jobs     map[string]*codexSteeringTask
	keys     map[string]string
	queue    []string
	running  int
	stopping bool
	wg       sync.WaitGroup
	drained  chan struct{}
}

func (s *Proxy) probeAdmission(ctx context.Context, cmd accevents.ProbeAdmissionCommand) (accevents.ProbeAdmissionResult, error) {
	v, err := s.SendEvent(event.NewEventWithContext(accevents.TopicProbeAdmission, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, cmd)).Get()
	if err != nil {
		return accevents.ProbeAdmissionResult{}, err
	}
	out, ok := v.(accevents.ProbeAdmissionResult)
	if !ok {
		return out, cd.NewError(cd.Unexpected, "invalid probe admission result")
	}
	return out, nil
}

func (j *steeringJobs) prune(now time.Time) {
	for id, t := range j.jobs {
		if t.report.State == "queued" && !now.Before(t.created.Add(steeringBudget)) {
			t.report.Phase = "queue"
			t.report.ErrorCode = "timeout"
			t.report.ExecutionOutcome = "timeout"
			j.finish(t)
		}
		if !t.finished.IsZero() && !now.Before(t.finished.Add(steeringTTL)) {
			delete(j.jobs, id)
			delete(j.keys, t.key)
		}
	}
	queue := j.queue[:0]
	for _, id := range j.queue {
		if t := j.jobs[id]; t != nil && t.report.State == "queued" {
			queue = append(queue, id)
		}
	}
	j.queue = queue
}
func (j *steeringJobs) finish(t *codexSteeringTask) {
	if t.report.State == "finished" {
		return
	}
	t.finished = time.Now().UTC()
	t.report.State = "finished"
	t.report.Done = true
	t.report.FinishedAt = t.finished.Format(time.RFC3339Nano)
	t.report.ExpiresAt = t.finished.Add(steeringTTL).Format(time.RFC3339Nano)
	actualSlot := ""
	if t.report.ActualSlotID != nil {
		actualSlot = *t.report.ActualSlotID
	}
	slog.Info("Codex steering probe finished", "probe_id", t.report.ProbeID, "target_slot_id", t.report.TargetSlotID, "actual_slot_id", actualSlot, "model", t.report.Model, "probe_profile", t.report.Profile, "phase", t.report.Phase, "admission_state", t.report.AdmissionState, "admission_reason", t.report.AdmissionReason, "capability", t.report.Capability, "execution_outcome", t.report.ExecutionOutcome, "error_code", t.report.ErrorCode, "upstream_request_sent", t.report.UpstreamRequestSent, "credential_refresh_performed", t.report.CredentialRefreshPerformed)
}

func (s *Proxy) handleStartSteeringProbe(ev event.Event, r event.Result) {
	if r == nil {
		return
	}
	cmd, ok := ev.Data().(proxyevents.StartSteeringProbeCommand)
	if !ok {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid steering command"))
		return
	}
	r.Set(s.startSteeringProbe(ev.Context(), cmd), nil)
}
func (s *Proxy) startSteeringProbe(ctx context.Context, cmd proxyevents.StartSteeringProbeCommand) proxyevents.SteeringProbeResult {
	cmd.SlotID = strings.TrimSpace(cmd.SlotID)
	fail := func(status int, code string) proxyevents.SteeringProbeResult {
		return proxyevents.SteeringProbeResult{Status: status, ErrorCode: code}
	}
	if cmd.SlotID == "" || len(cmd.SlotID) > 256 || cmd.Model == "" || len(cmd.Model) > 256 || cmd.IdempotencyKey == "" || len(cmd.IdempotencyKey) > 128 || strings.ContainsAny(cmd.IdempotencyKey, "\r\n\x00") {
		return fail(400, "invalid_probe_request")
	}
	j := &s.steering
	j.mu.Lock()
	if j.jobs == nil {
		j.jobs = map[string]*codexSteeringTask{}
		j.keys = map[string]string{}
	}
	j.prune(time.Now())
	if j.stopping {
		j.mu.Unlock()
		return fail(503, "probe_stopping")
	}
	if id := j.keys[cmd.IdempotencyKey]; id != "" {
		t := j.jobs[id]
		out := t.report
		j.mu.Unlock()
		if out.TargetSlotID != cmd.SlotID || out.Model != cmd.Model || out.Profile != steering.Profile {
			return fail(409, "idempotency_conflict")
		}
		return proxyevents.SteeringProbeResult{Status: 202, Report: out}
	}
	active := 0
	for _, t := range j.jobs {
		if t.report.State != "finished" {
			active++
		}
		if t.report.TargetSlotID == cmd.SlotID && t.report.State != "finished" {
			j.mu.Unlock()
			return fail(409, "slot_probe_active")
		}
	}
	if len(j.jobs) >= 200 || active >= 34 {
		j.mu.Unlock()
		return fail(429, "probe_capacity")
	}
	now := time.Now().UTC()
	t := &codexSteeringTask{key: cmd.IdempotencyKey, created: now, report: proxyevents.SteeringReport{
		Evidence: steering.NewEvidence(), ProbeID: uuid.NewString(), TargetSlotID: cmd.SlotID, Model: cmd.Model, Profile: steering.Profile,
		Parameters:     proxyevents.SteeringParameters{Instructions: steering.Instructions, InitialInput: steering.InitialInput, SteerInput: steering.SteerInput, Store: false, Reasoning: "upstream_default"},
		AdmissionState: "pending", State: "preflight", CreatedAt: now.Format(time.RFC3339Nano), Transport: "websocket",
	}}
	j.jobs[t.report.ProbeID] = t
	j.keys[t.key] = t.report.ProbeID
	j.mu.Unlock()
	slog.Info("Codex steering probe created", "probe_id", t.report.ProbeID, "target_slot_id", cmd.SlotID, "model", cmd.Model, "probe_profile", steering.Profile)
	// Reservation above makes duplicate submissions safe while preflight runs.
	preflightCtx, cancel := context.WithDeadline(ctx, now.Add(steeringBudget))
	admission, err := s.probeAdmission(preflightCtx, accevents.ProbeAdmissionCommand{AccountID: cmd.SlotID, Model: cmd.Model})
	cancel()
	j.mu.Lock()
	if err != nil || !admission.Allowed || j.stopping {
		t.report.AccountLabel = admission.AccountLabel
		t.report.ConfigVersion = admission.ConfigVersion
		t.report.AdmissionState = "rejected"
		t.report.AdmissionReason = admission.Reason
		if err != nil {
			t.report.AdmissionReason = "admission_unavailable"
		}
		if j.stopping {
			t.report.AdmissionReason = "probe_stopping"
		}
		j.finish(t)
	} else {
		t.report.State = "queued"
		t.report.AccountLabel = admission.AccountLabel
		t.report.ConfigVersion = admission.ConfigVersion
		// Recheck queue capacity after concurrent preflights. A reserved report
		// becomes a neutral local terminal result if the queue filled meanwhile.
		if j.running >= 2 && len(j.queue) >= 32 {
			t.report.ErrorCode = "probe_capacity"
			t.report.ExecutionOutcome = "not_started"
			j.finish(t)
		} else {
			j.queue = append(j.queue, t.report.ProbeID)
		}
	}
	out := t.report
	j.mu.Unlock()
	s.pumpSteering()
	return proxyevents.SteeringProbeResult{Status: 202, Report: out}
}

func (s *Proxy) pumpSteering() {
	j := &s.steering
	var ready []*codexSteeringTask
	j.mu.Lock()
	j.prune(time.Now())
	for !j.stopping && j.running < 2 && len(j.queue) > 0 {
		id := j.queue[0]
		j.queue = j.queue[1:]
		t := j.jobs[id]
		if time.Since(t.created) >= steeringBudget {
			t.report.Phase = "queue"
			t.report.ExecutionOutcome = "timeout"
			t.report.ErrorCode = "timeout"
			j.finish(t)
			continue
		}
		t.report.State = "running"
		t.report.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
		j.running++
		j.wg.Add(1)
		ready = append(ready, t)
	}
	j.mu.Unlock()
	for _, t := range ready {
		admitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := s.AsyncTaskContext(admitCtx, func() { s.runSteeringTask(t) })
		cancel()
		if err != nil {
			j.mu.Lock()
			t.report.ErrorCode = "worker_unavailable"
			t.report.ExecutionOutcome = "not_started"
			j.finish(t)
			j.running--
			j.mu.Unlock()
			j.wg.Done()
			s.pumpSteering()
		}
	}
}

// Initialization returns the managed worker after starting the upstream engine.
// A short timer turn monitors it; controllers never occupy workers while
// waiting for a separate engine on the same BackgroundRoutine.
func (s *Proxy) runSteeringTask(t *codexSteeringTask) {
	j := &s.steering
	ctx, cancel := context.WithDeadline(context.Background(), t.created.Add(steeringBudget))
	j.mu.Lock()
	t.ctx = ctx
	t.cancel = cancel
	stopping := j.stopping
	out := t.report
	j.mu.Unlock()
	if stopping {
		cancel()
	}
	initialized := false
	defer func() {
		if !initialized {
			s.finishSteeringTask(t, out, context.Background())
			return
		}
		j.mu.Lock()
		t.report = out
		t.monitoring = true
		j.mu.Unlock()
	}()
	admission, err := s.probeAdmission(ctx, accevents.ProbeAdmissionCommand{AccountID: out.TargetSlotID, Model: out.Model, Acquire: true})
	out.AccountLabel = admission.AccountLabel
	out.ConfigVersion = admission.ConfigVersion
	if err != nil || !admission.Allowed {
		out.AdmissionState = "rejected"
		out.AdmissionReason = admission.Reason
		if err != nil {
			out.AdmissionReason = "admission_unavailable"
		}
		return
	}
	account := admission.Credential
	t.leaseID = account.LeaseID
	out.FingerprintMode = account.FingerprintMode
	out.ProxyConfigured = strings.TrimSpace(account.Proxy) != ""
	out.ClientVersion = account.ClientIdentity.Version
	out.AdmissionState = "accepted"
	out.ActualSlotID = &account.AccountID
	if admission.RefreshNeeded {
		out.Phase = "credential_refresh"
		out.CredentialRefreshPerformed = false
		j.mu.Lock()
		t.report = out
		j.mu.Unlock()
		v, refreshErr := s.SendEvent(accevents.BindProbeRefreshLane(event.NewEventWithContext(accevents.TopicRefreshToken, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.RefreshTokenCommand{AccountID: account.AccountID, ProbeLeaseID: account.LeaseID, ProbeConfigVersion: out.ConfigVersion}))).Get()
		refreshed, ok := v.(accevents.RefreshTokenResult)
		out.CredentialRefreshPerformed = refreshed.RequestSent
		out.UpstreamRequestSent = refreshed.RequestSent
		if refreshErr != nil || !ok || refreshed.AccessToken == "" {
			out.CredentialRefreshResult = "failed"
			out.ExecutionOutcome = "credential_refresh_failed"
			out.ErrorCode = "credential_refresh_failed"
			return
		}
		out.CredentialRefreshResult = "succeeded"
		account.AccessToken = refreshed.AccessToken
		account.AccountIDHeader = refreshed.AccountIDHeader
		account.Proxy = refreshed.Proxy
	}
	out.Phase = "admission"
	current, err := s.probeAdmission(ctx, accevents.ProbeAdmissionCommand{AccountID: out.TargetSlotID, Model: out.Model, LeaseID: account.LeaseID})
	if ctx.Err() != nil {
		out.ExecutionOutcome = "timeout"
		if ctx.Err() == context.Canceled {
			out.ExecutionOutcome = "canceled"
		}
		out.ErrorCode = out.ExecutionOutcome
		return
	}
	if err != nil {
		out.AdmissionState = "rejected"
		out.AdmissionReason = "admission_unavailable"
		out.ExecutionOutcome = "admission_unavailable"
		out.ErrorCode = "admission_unavailable"
		return
	}
	// A stopping owner returns no account epoch; this is a local refusal,
	// not evidence that the account configuration changed.
	if current.ConfigVersion != out.ConfigVersion && current.Reason != "pool_stopping" {
		out.Historical = true
		out.ExecutionOutcome = "account_changed"
		out.ErrorCode = "account_changed"
		return
	}
	if !current.Allowed {
		out.AdmissionState = "rejected"
		out.AdmissionReason = current.Reason
		out.ExecutionOutcome = "not_started"
		return
	}
	session := "steering-probe:" + out.ProbeID
	cmd := upevents.SteeringStartCommand{ProbeID: out.ProbeID, Model: out.Model, Deadline: t.created.Add(steeringBudget), AccessToken: account.AccessToken, AccountIDHeader: account.AccountIDHeader, Proxy: account.Proxy, SessionHash: session,
		Fingerprint: codexFingerprintForTurn(resolveCodexFingerprint(account.FingerprintSeed, account.FingerprintMode, session, "", codexresponses.TurnMetadata{})), ClientIdentity: resolvedCodexClientIdentity(account, "", "")}
	t.upstreamStarted = true
	v, sendErr := s.SendEvent(upevents.BindCommandLane(event.NewEventWithContext(upevents.TopicSteeringStart, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, cmd))).Get()
	started, ok := v.(upevents.SteeringStartResult)
	if sendErr != nil || !ok || started.ProbeID != out.ProbeID {
		out.ExecutionOutcome = "probe_unavailable"
		out.ErrorCode = "probe_unavailable"
		return
	}
	out.Phase = "websocket"
	out.ExecutionOutcome = "running"
	initialized = true
}

func (s *Proxy) tickSteering() { s.tickSteeringContext(context.Background()) }
func (s *Proxy) tickSteeringContext(cleanupCtx context.Context) {
	s.pumpSteering()
	j := &s.steering
	var ready []*codexSteeringTask
	j.mu.Lock()
	for _, t := range j.jobs {
		if t.monitoring && t.report.State == "running" {
			t.monitoring = false
			ready = append(ready, t)
		}
	}
	j.mu.Unlock()
	for _, t := range ready {
		s.pollSteeringTask(t, cleanupCtx)
	}
}

func (s *Proxy) pollSteeringTask(t *codexSteeringTask, cleanupCtx context.Context) {
	j := &s.steering
	j.mu.Lock()
	out := t.report
	j.mu.Unlock()
	if t.finalizing {
		s.finishSteeringTask(t, out, cleanupCtx)
		return
	}
	ctx := t.ctx
	if ctx.Err() != nil {
		out.ErrorCode = "timeout"
		if ctx.Err() == context.Canceled {
			out.ErrorCode = "canceled"
		}
		out.ExecutionOutcome = out.ErrorCode
		s.finishSteeringTask(t, out, cleanupCtx)
		return
	}
	v, err := s.SendEvent(upevents.BindCommandLane(event.NewEventWithContext(upevents.TopicSteeringSnapshot, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.SteeringSnapshotCommand{ProbeID: out.ProbeID}))).Get()
	snapshot, ok := v.(upevents.SteeringSnapshotResult)
	if err != nil || !ok {
		out.ErrorCode = "probe_unavailable"
		out.ExecutionOutcome = out.ErrorCode
		s.finishSteeringTask(t, out, cleanupCtx)
		return
	}
	applySteeringSnapshot(&out, snapshot)
	current, admissionErr := s.probeAdmission(ctx, accevents.ProbeAdmissionCommand{AccountID: out.TargetSlotID, Model: out.Model})
	if admissionErr != nil {
		out.ExecutionOutcome = "probe_unavailable"
		out.ErrorCode = "probe_unavailable"
		s.finishSteeringTask(t, out, cleanupCtx)
		return
	}
	if current.ConfigVersion != out.ConfigVersion {
		out.Historical = true
		out.ExecutionOutcome = "account_changed"
		out.ErrorCode = "account_changed"
		s.finishSteeringTask(t, out, cleanupCtx)
		return
	}
	if !t.usageMerged && len(snapshot.Headers) > 0 {
		s.mergeCodexUsageHeaders(ctx, out.TargetSlotID, snapshot.Headers)
		t.usageMerged = true
	}
	if out.Done {
		s.finishSteeringTask(t, out, cleanupCtx)
		return
	}
	j.mu.Lock()
	t.report = out
	t.monitoring = true
	j.mu.Unlock()
}

// Keep the slot reserved until the engine really exited and the lease release
// was acknowledged. Failed cleanup remains retryable, including during drain.
func (s *Proxy) finishSteeringTask(t *codexSteeringTask, out proxyevents.SteeringReport, cleanupCtx context.Context) {
	j := &s.steering
	retry := func() {
		out.Done = false
		j.mu.Lock()
		t.report = out
		t.finalizing = true
		t.monitoring = true
		j.mu.Unlock()
	}
	if t.upstreamStarted {
		cleanup, cancel := context.WithTimeout(cleanupCtx, 5*time.Second)
		v, err := s.SendEvent(upevents.BindCommandLane(event.NewEventWithContext(upevents.TopicSteeringClose, s.ID(), upcommon.UnitID, event.NewHeader(), cleanup, upevents.SteeringCloseCommand{ProbeID: out.ProbeID}))).Get()
		cancel()
		closed, ok := v.(upevents.SteeringCloseResult)
		if err != nil || !ok {
			retry()
			return
		}
		if closed.Closed {
			reason := out.ErrorCode
			applySteeringSnapshot(&out, closed.Snapshot)
			// The controller can observe its deadline or shutdown after the
			// engine already terminated. Keep that recorded terminal result,
			// especially pending, instead of replacing it with a later cancel.
			terminalObserved := out.Done && out.ExecutionOutcome != "timeout" && out.ExecutionOutcome != "canceled"
			if reason != "" && !(terminalObserved && (reason == "timeout" || reason == "canceled")) {
				out.ErrorCode = reason
				out.ExecutionOutcome = reason
			}
		}
		t.upstreamStarted = false
	}
	if t.leaseID != "" {
		cleanup, cancel := context.WithTimeout(cleanupCtx, 5*time.Second)
		v, err := s.SendEvent(event.NewEventWithContext(accevents.TopicRelease, s.ID(), acccommon.UnitID, event.NewHeader(), cleanup, accevents.ReleaseCommand{LeaseID: t.leaseID})).Get()
		cancel()
		if _, ok := v.(accevents.ReleaseResult); err != nil || !ok {
			retry()
			return
		}
		t.leaseID = ""
	}
	if t.cancel != nil {
		t.cancel()
	}
	j.mu.Lock()
	t.cancel = nil
	t.report = out
	j.finish(t)
	j.running--
	j.mu.Unlock()
	j.wg.Done()
	s.pumpSteering()
}

func applySteeringSnapshot(out *proxyevents.SteeringReport, s upevents.SteeringSnapshotResult) {
	refreshSent := out.CredentialRefreshPerformed
	out.Evidence = s.Evidence
	out.UpstreamRequestSent = out.UpstreamRequestSent || refreshSent
	out.Endpoint = s.Endpoint
}

func (s *Proxy) handleGetSteeringProbe(ev event.Event, r event.Result) {
	if r == nil {
		return
	}
	cmd, ok := ev.Data().(proxyevents.GetSteeringProbeCommand)
	if !ok {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid steering query"))
		return
	}
	cmd.SlotID = strings.TrimSpace(cmd.SlotID)
	j := &s.steering
	j.mu.Lock()
	j.prune(time.Now())
	t := j.jobs[cmd.ProbeID]
	if cmd.ProbeID == "latest" {
		for _, candidate := range j.jobs {
			if candidate.report.TargetSlotID == cmd.SlotID && (t == nil || candidate.created.After(t.created)) {
				t = candidate
			}
		}
	}
	if t == nil || t.report.TargetSlotID != cmd.SlotID {
		j.mu.Unlock()
		r.Set(proxyevents.SteeringProbeResult{Status: 404, ErrorCode: "probe_not_found"}, nil)
		return
	}
	out := t.report
	j.mu.Unlock()
	if current, err := s.probeAdmission(ev.Context(), accevents.ProbeAdmissionCommand{AccountID: out.TargetSlotID, Model: out.Model}); err == nil && current.Reason != "pool_stopping" {
		out.Historical = out.ConfigVersion != "" && current.ConfigVersion != out.ConfigVersion
	}
	r.Set(proxyevents.SteeringProbeResult{Status: 200, Report: out}, nil)
}

func (s *Proxy) stopSteering() {
	j := &s.steering
	j.mu.Lock()
	defer j.mu.Unlock()
	j.stopping = true
	for _, t := range j.jobs {
		if t.cancel != nil {
			t.cancel()
		}
		if !t.report.Done && (t.report.State == "queued" || t.report.State == "preflight") {
			t.report.ExecutionOutcome = "canceled"
			t.report.ErrorCode = "canceled"
			j.finish(t)
		}
	}
	j.queue = nil
}
func (s *Proxy) drainSteering(ctx context.Context) *cd.Error {
	s.stopSteering()
	j := &s.steering
	j.mu.Lock()
	if j.drained == nil {
		j.drained = make(chan struct{})
		go func() { j.wg.Wait(); close(j.drained) }()
	}
	done := j.drained
	j.mu.Unlock()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		s.tickSteeringContext(ctx)
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return cd.NewError(cd.Timeout, "steering probes have not drained")
		case <-tick.C:
		}
	}

}

func (s *Proxy) BeginShutdown(context.Context)         { s.stopSteering() }
func (s *Proxy) Quiesce(ctx context.Context) *cd.Error { return s.drainSteering(ctx) }
