package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"aetherrelay/internal/pkg/modeltrace"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

const modeltraceBudget = 180 * time.Second
const modeltraceTTL = 30 * time.Minute

type modeltraceTask struct {
	report                 proxyevents.ModelTraceReport
	key                    string
	created, finished      time.Time
	ctx                    context.Context
	cancel                 context.CancelFunc
	leaseID, sampleID      string
	index                  int
	monitoring, finalizing bool
}
type modeltraceJobs struct {
	mu       sync.Mutex
	tickMu   sync.Mutex
	jobs     map[string]*modeltraceTask
	keys     map[string]string
	stopping bool
	wg       sync.WaitGroup
	drained  chan struct{}
}

func (s *Proxy) probeAdmission(ctx context.Context, cmd accevents.ProbeAdmissionCommand) (accevents.ProbeAdmissionResult, error) {
	v, err := s.SendEvent(event.NewEventWithContext(accevents.TopicProbeAdmission, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, cmd)).Get()
	out, ok := v.(accevents.ProbeAdmissionResult)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, cd.NewError(cd.Unexpected, "invalid probe admission result")
	}
	return out, nil
}
func (j *modeltraceJobs) prune() {
	now := time.Now()
	for id, t := range j.jobs {
		if !t.finished.IsZero() && now.After(t.finished.Add(modeltraceTTL)) {
			delete(j.jobs, id)
			delete(j.keys, t.key)
		}
	}
}
func (s *Proxy) handleStartModelTraceProbe(ev event.Event, r event.Result) {
	if r == nil {
		return
	}
	cmd, ok := ev.Data().(proxyevents.StartModelTraceProbeCommand)
	if !ok {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid sample batch"))
		return
	}
	r.Set(s.startModelTraceProbe(ev.Context(), cmd), nil)
}
func validProbeReasoning(v string) bool {
	switch v {
	case "upstream_default", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	}
	return false
}
func (s *Proxy) startModelTraceProbe(ctx context.Context, cmd proxyevents.StartModelTraceProbeCommand) proxyevents.ModelTraceProbeResult {
	fail := func(status int, code string) proxyevents.ModelTraceProbeResult {
		return proxyevents.ModelTraceProbeResult{Status: status, ErrorCode: code}
	}
	cmd.SlotID = strings.TrimSpace(cmd.SlotID)
	if cmd.Reasoning == "" {
		cmd.Reasoning = "upstream_default"
	}
	if cmd.SlotID == "" || len(cmd.SlotID) > 256 || cmd.Model == "" || len(cmd.Model) > 256 || strings.TrimSpace(cmd.Model) != cmd.Model || !validProbeReasoning(cmd.Reasoning) || cmd.ProbeID == "" || len(cmd.ProbeID) > 128 || cmd.IdempotencyKey == "" || len(cmd.IdempotencyKey) > 128 || strings.ContainsAny(cmd.IdempotencyKey, "\r\n\x00") || len(cmd.Challenges) != 3 {
		return fail(400, "invalid_probe_request")
	}
	for _, c := range cmd.Challenges {
		if c.ExpectedCount < 292 || c.ExpectedCount > 332 || len(c.Prompt) > 4096 || c.Prompt == "" || len(c.ID) > 128 || c.ID == "" || c.PromptVersion != modeltrace.PagesPromptVersion {
			return fail(400, "invalid_probe_profile")
		}
	}
	j := &s.modeltrace
	j.mu.Lock()
	if j.jobs == nil {
		j.jobs = map[string]*modeltraceTask{}
		j.keys = map[string]string{}
	}
	j.prune()
	if j.stopping {
		j.mu.Unlock()
		return fail(503, "probe_stopping")
	}
	if id := j.keys[cmd.IdempotencyKey]; id != "" {
		out := j.jobs[id].report.Clone()
		j.mu.Unlock()
		if out.TargetSlotID != cmd.SlotID || out.Model != cmd.Model || out.Reasoning != cmd.Reasoning {
			return fail(409, "idempotency_conflict")
		}
		return proxyevents.ModelTraceProbeResult{Status: 202, Report: out}
	}
	if j.jobs[cmd.ProbeID] != nil {
		j.mu.Unlock()
		return fail(409, "probe_id_conflict")
	}
	for _, t := range j.jobs {
		if !t.report.Done {
			j.mu.Unlock()
			return fail(429, "probe_capacity")
		}
	}
	if len(j.jobs) >= 100 {
		j.mu.Unlock()
		return fail(429, "probe_capacity")
	}
	now := time.Now().UTC()
	t := &modeltraceTask{key: cmd.IdempotencyKey, created: now}
	t.ctx, t.cancel = context.WithDeadline(context.Background(), now.Add(modeltraceBudget))
	t.report = proxyevents.ModelTraceReport{SchemaVersion: "aetherrelay-modeltrace-sampling-v1", ProbeID: cmd.ProbeID, TargetSlotID: cmd.SlotID, Model: cmd.Model, Reasoning: cmd.Reasoning, Profile: modeltrace.PagesPromptVersion, Parser: string(modeltrace.CoreCompatible), Transport: "http_sse", State: "preflight", CreatedAt: now.Format(time.RFC3339Nano)}
	for _, c := range cmd.Challenges {
		t.report.Samples = append(t.report.Samples, proxyevents.ModelTraceSample{ID: c.ID, Prompt: c.Prompt, ExpectedCount: c.ExpectedCount, State: "not_started"})
	}
	j.jobs[cmd.ProbeID] = t
	j.keys[t.key] = cmd.ProbeID
	j.wg.Add(1)
	j.mu.Unlock()
	slog.Info("Codex modeltrace probe created", "probe_id", cmd.ProbeID, "target_slot_id", cmd.SlotID, "model", cmd.Model)
	// Preflight uses the caller context. An admitted job thereafter has an owner deadline.
	preflightCtx, preflightCancel := context.WithDeadline(ctx, now.Add(modeltraceBudget))
	admission, err := s.probeAdmission(preflightCtx, accevents.ProbeAdmissionCommand{AccountID: cmd.SlotID, Model: cmd.Model})
	preflightCancel()
	j.mu.Lock()
	t.report.ConfigVersion = admission.ConfigVersion
	if err != nil || !admission.Allowed {
		t.report.AdmissionReason = admission.Reason
		if err != nil {
			t.report.AdmissionReason = "admission_unavailable"
		}
		t.report.ErrorCode = "admission_rejected"
		t.finalizing = true
		t.monitoring = true
	}
	j.mu.Unlock()
	if err == nil && admission.Allowed {
		s.scheduleModelTrace(t)
	}
	j.mu.Lock()
	out := t.report.Clone()
	j.mu.Unlock()
	return proxyevents.ModelTraceProbeResult{Status: 202, Report: out}
}
func (s *Proxy) scheduleModelTrace(t *modeltraceTask) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.AsyncTaskContext(ctx, func() { s.initializeModelTraceSample(t) }); err != nil {
		j := &s.modeltrace
		j.mu.Lock()
		t.report.ErrorCode = "worker_unavailable"
		t.finalizing = true
		t.monitoring = true
		j.mu.Unlock()
	}
}
func (s *Proxy) initializeModelTraceSample(t *modeltraceTask) {
	j := &s.modeltrace
	j.mu.Lock()
	out := t.report.Clone()
	j.mu.Unlock()
	defer func() { j.mu.Lock(); t.report = out; t.monitoring = true; j.mu.Unlock() }()
	fail := func(code string) { out.ErrorCode = code; t.finalizing = true }
	if t.ctx.Err() != nil {
		fail("canceled")
		return
	}
	admission, err := s.probeAdmission(t.ctx, accevents.ProbeAdmissionCommand{AccountID: out.TargetSlotID, Model: out.Model, Acquire: true, Request: accevents.LeaseRequest{RequestID: out.ProbeID, Protocol: "modeltrace_probe"}})
	if err != nil || !admission.Allowed {
		out.AdmissionReason = admission.Reason
		fail("admission_rejected")
		return
	}
	account := admission.Credential
	t.leaseID = account.LeaseID
	out.ActualSlotID = account.AccountID
	if account.AccountID != out.TargetSlotID || admission.ConfigVersion != out.ConfigVersion {
		out.Historical = true
		fail("account_changed")
		return
	}
	if admission.RefreshNeeded {
		v, e := s.SendEvent(accevents.BindProbeRefreshLane(event.NewEventWithContext(accevents.TopicRefreshToken, s.ID(), acccommon.UnitID, event.NewHeader(), t.ctx, accevents.RefreshTokenCommand{AccountID: account.AccountID, ProbeLeaseID: account.LeaseID, ProbeConfigVersion: out.ConfigVersion}))).Get()
		refreshed, ok := v.(accevents.RefreshTokenResult)
		out.CredentialRefreshPerformed = out.CredentialRefreshPerformed || refreshed.RequestSent
		if e != nil || !ok || refreshed.AccessToken == "" {
			fail("credential_refresh_failed")
			return
		}
		account.AccessToken = refreshed.AccessToken
		account.AccountIDHeader = refreshed.AccountIDHeader
		account.Proxy = refreshed.Proxy
	}
	current, err := s.probeAdmission(t.ctx, accevents.ProbeAdmissionCommand{AccountID: out.TargetSlotID, Model: out.Model, LeaseID: t.leaseID})
	if err != nil || !current.Allowed {
		out.AdmissionReason = current.Reason
		fail("admission_rejected")
		return
	}
	if current.ConfigVersion != out.ConfigVersion {
		out.Historical = true
		fail("account_changed")
		return
	}
	sample := &out.Samples[t.index]
	body := struct {
		// Sampling bypasses the HTTP normalization entry point (CP-REQ-003).
		Instructions string `json:"instructions"`
		Model        string `json:"model"`
		Input        string `json:"input"`
		Store        bool   `json:"store"`
		Stream       bool   `json:"stream"`
		Reasoning    *struct {
			Effort string `json:"effort"`
		} `json:"reasoning,omitempty"`
	}{Model: out.Model, Input: sample.Prompt, Stream: true}
	if out.Reasoning != "upstream_default" {
		body.Reasoning = &struct {
			Effort string `json:"effort"`
		}{out.Reasoning}
	}
	wire, _ := json.Marshal(body)
	sample.SourceRequestSHA256 = probeDigest(wire)
	sample.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	sample.State = "running"
	session := "modeltrace:" + out.ProbeID + ":" + sample.ID
	t.sampleID = out.ProbeID + ":" + sample.ID
	request := upevents.CompleteCommand{Body: wire, AccessToken: account.AccessToken, AccountIDHeader: account.AccountIDHeader, Proxy: account.Proxy, SessionHash: session, ClientIdentity: resolvedCodexClientIdentity(account, "", ""), Fingerprint: codexFingerprintForTurn(resolveCodexFingerprint(account.FingerprintSeed, account.FingerprintMode, session, "", codexresponses.TurnMetadata{}))}
	v, e := s.SendEvent(upevents.BindCommandLane(event.NewEventWithContext(upevents.TopicSampleStart, s.ID(), upcommon.UnitID, event.NewHeader(), t.ctx, upevents.SampleStartCommand{ProbeID: t.sampleID, Deadline: t.created.Add(modeltraceBudget), Request: request}))).Get()
	started, ok := v.(upevents.SampleStartResult)
	if e != nil || !ok || started.ProbeID != t.sampleID {
		fail("probe_unavailable")
		return
	}
	out.State = "running"
}
func probeDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func (s *Proxy) tickModelTrace()     { s.tickModelTraceContext(context.Background()) }
func (s *Proxy) tickModelTraceContext(cleanupCtx context.Context) {
	j := &s.modeltrace
	j.tickMu.Lock()
	defer j.tickMu.Unlock()
	j.mu.Lock()
	j.prune()
	var ready []*modeltraceTask
	for _, t := range j.jobs {
		if t.monitoring && !t.report.Done {
			t.monitoring = false
			ready = append(ready, t)
		}
	}
	j.mu.Unlock()
	for _, t := range ready {
		s.pollModelTrace(t, cleanupCtx)
	}
}
func (s *Proxy) pollModelTrace(t *modeltraceTask, cleanupCtx context.Context) {
	j := &s.modeltrace
	j.mu.Lock()
	out := t.report.Clone()
	j.mu.Unlock()
	advance := false
	defer func() {
		j.mu.Lock()
		t.report = out
		if !out.Done && out.State != "starting" {
			t.monitoring = true
		}
		j.mu.Unlock()
		if advance {
			s.scheduleModelTrace(t)
		}
	}()
	if t.ctx.Err() != nil && out.ErrorCode == "" {
		out.ErrorCode = "timeout"
		if t.ctx.Err() == context.Canceled {
			out.ErrorCode = "canceled"
		}
		t.finalizing = true
	}
	if !t.finalizing {
		current, err := s.probeAdmission(t.ctx, accevents.ProbeAdmissionCommand{AccountID: out.TargetSlotID, Model: out.Model, LeaseID: t.leaseID})
		if err != nil || !current.Allowed {
			out.ErrorCode = "admission_rejected"
			out.AdmissionReason = current.Reason
			t.finalizing = true
		} else if current.ConfigVersion != out.ConfigVersion {
			out.ErrorCode = "account_changed"
			out.Historical = true
			t.finalizing = true
		}
	}
	if !t.finalizing {
		v, err := s.SendEvent(upevents.BindCommandLane(event.NewEventWithContext(upevents.TopicSampleSnapshot, s.ID(), upcommon.UnitID, event.NewHeader(), t.ctx, upevents.SampleSnapshotCommand{ProbeID: t.sampleID}))).Get()
		snapshot, ok := v.(upevents.SampleSnapshotResult)
		if err != nil || !ok {
			out.ErrorCode = "probe_unavailable"
			t.finalizing = true
		} else if !snapshot.Done {
			return
		} else {
			applyModelTraceSample(&out.Samples[t.index], snapshot)
			if out.Samples[t.index].ErrorCode != "" {
				out.ErrorCode = out.Samples[t.index].ErrorCode
			}
			t.finalizing = true
		}
	}
	// Every sample closes the worker before checked release. Failed cleanup is retried.
	out.State = "finalizing"
	if t.sampleID != "" {
		ctx, cancel := context.WithTimeout(cleanupCtx, time.Second)
		v, err := s.SendEvent(upevents.BindCommandLane(event.NewEventWithContext(upevents.TopicSampleClose, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.SampleCloseCommand{ProbeID: t.sampleID}))).Get()
		cancel()
		closed, ok := v.(upevents.SampleCloseResult)
		if err != nil || !ok || !closed.Closed {
			return
		}
		if closed.Snapshot.Done {
			sample := &out.Samples[t.index]
			applyModelTraceSample(sample, closed.Snapshot)
			started, _ := time.Parse(time.RFC3339Nano, sample.StartedAt)
			ctx, cancel := context.WithTimeout(cleanupCtx, time.Second)
			s.mergeCodexUsageHeaders(ctx, out.TargetSlotID, closed.Snapshot.Headers, started)
			cancel()
		}
		t.sampleID = ""
	}
	if t.leaseID != "" {
		ctx, cancel := context.WithTimeout(cleanupCtx, time.Second)
		err := s.releaseCodexAccountChecked(ctx, t.leaseID)
		cancel()
		if err != nil {
			return
		}
		t.leaseID = ""
	}
	if out.ErrorCode == "" && t.index < 2 {
		t.index++
		t.finalizing = false
		out.State = "starting"
		// Publish before admission of the next managed worker, with no concurrent controller.
		advance = true
		return
	}
	if out.ErrorCode != "" {
		for i := range out.Samples {
			if out.Samples[i].State == "running" {
				out.Samples[i].State = "failed"
				out.Samples[i].ErrorCode = out.ErrorCode
			}
		}
	}
	t.cancel()
	j.mu.Lock()
	t.finished = time.Now().UTC()
	j.mu.Unlock()
	out.State = "finished"
	out.Done = true
	out.FinishedAt = t.finished.Format(time.RFC3339Nano)
	out.ExpiresAt = t.finished.Add(modeltraceTTL).Format(time.RFC3339Nano)
	slog.Info("Codex modeltrace probe finished", "probe_id", out.ProbeID, "target_slot_id", out.TargetSlotID, "model", out.Model, "error_code", out.ErrorCode)
	j.wg.Done()
}
func applyModelTraceSample(sample *proxyevents.ModelTraceSample, snapshot upevents.SampleSnapshotResult) {
	sample.HTTPStatus = snapshot.HTTPStatus
	sample.EffectiveRequestSHA256 = snapshot.EffectiveRequestSHA256
	sample.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if snapshot.ErrorClass != "" {
		sample.State = "failed"
		sample.ErrorCode = string(snapshot.ErrorClass)
		return
	}
	var response struct {
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(snapshot.Body, &response) != nil || response.Status != "completed" {
		sample.State = "failed"
		sample.ErrorCode = "invalid_completion"
		return
	}
	var text strings.Builder
	for _, item := range response.Output {
		if item.Type != "message" || item.Role != "assistant" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" {
				text.WriteString(part.Text)
			}
		}
	}
	if text.Len() > 16384 {
		sample.State = "failed"
		sample.ErrorCode = "output_limit"
		return
	}
	sample.TextSource = "derived_completed_response"
	sample.OutputText = text.String()
	sample.ContentSHA256 = probeDigest([]byte(sample.OutputText))
	sample.ResponseModelClaim = response.Model
	var usage struct {
		Input  *int64 `json:"input_tokens"`
		Output *int64 `json:"output_tokens"`
		Total  *int64 `json:"total_tokens"`
	}
	if json.Unmarshal(response.Usage, &usage) == nil && usage.Input != nil && usage.Output != nil && usage.Total != nil && *usage.Input >= 0 && *usage.Output >= 0 && *usage.Total >= 0 {
		sample.Usage = &proxyevents.ModelTraceUsage{InputTokens: *usage.Input, OutputTokens: *usage.Output, TotalTokens: *usage.Total}
		sample.UsageComplete = true
	}
	sample.State = "completed"
}
func (s *Proxy) handleGetModelTraceProbe(ev event.Event, r event.Result) {
	if r == nil {
		return
	}
	cmd, ok := ev.Data().(proxyevents.GetModelTraceProbeCommand)
	if !ok {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid probe query"))
		return
	}
	j := &s.modeltrace
	j.mu.Lock()
	j.prune()
	t := j.jobs[cmd.ProbeID]
	if t == nil || t.report.TargetSlotID != strings.TrimSpace(cmd.SlotID) {
		j.mu.Unlock()
		r.Set(proxyevents.ModelTraceProbeResult{Status: 404, ErrorCode: "probe_not_found"}, nil)
		return
	}
	out := t.report.Clone()
	j.mu.Unlock()
	if current, err := s.probeAdmission(ev.Context(), accevents.ProbeAdmissionCommand{AccountID: out.TargetSlotID, Model: out.Model}); err == nil && current.Reason != "pool_stopping" {
		out.Historical = out.Historical || current.ConfigVersion != out.ConfigVersion
	}
	r.Set(proxyevents.ModelTraceProbeResult{Status: 200, Report: out}, nil)
}
func (s *Proxy) handleCancelModelTraceProbe(ev event.Event, r event.Result) {
	if r == nil {
		return
	}
	cmd, ok := ev.Data().(proxyevents.CancelModelTraceProbeCommand)
	if !ok {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid probe cancel"))
		return
	}
	j := &s.modeltrace
	j.mu.Lock()
	t := j.jobs[cmd.ProbeID]
	if t == nil || t.report.TargetSlotID != strings.TrimSpace(cmd.SlotID) {
		j.mu.Unlock()
		r.Set(proxyevents.ModelTraceProbeResult{Status: 404, ErrorCode: "probe_not_found"}, nil)
		return
	}
	if !t.report.Done {
		t.cancel()
	}
	out := t.report.Clone()
	j.mu.Unlock()
	r.Set(proxyevents.ModelTraceProbeResult{Status: 202, Report: out}, nil)
}
func (s *Proxy) BeginShutdown(context.Context) {
	j := &s.modeltrace
	j.mu.Lock()
	defer j.mu.Unlock()
	j.stopping = true
	for _, t := range j.jobs {
		t.cancel()
	}
}
func (s *Proxy) drainModelTrace(ctx context.Context) *cd.Error {
	s.BeginShutdown(ctx)
	j := &s.modeltrace
	j.mu.Lock()
	if j.drained == nil {
		j.drained = make(chan struct{})
		go func() { j.wg.Wait(); close(j.drained) }()
	}
	done := j.drained
	j.mu.Unlock()
	timer := time.NewTicker(20 * time.Millisecond)
	defer timer.Stop()
	for {
		s.tickModelTraceContext(ctx)
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return cd.NewError(cd.Timeout, "modeltrace samples have not drained")
		case <-timer.C:
		}
	}
}
func (s *Proxy) Quiesce(ctx context.Context) *cd.Error { return s.drainModelTrace(ctx) }

func (s *Proxy) handleDrainModelTraceProbe(ev event.Event, r event.Result) {
	if r == nil {
		return
	}
	cmd, ok := ev.Data().(proxyevents.DrainModelTraceProbeCommand)
	if !ok {
		r.Set(nil, cd.NewError(cd.IllegalParam, "invalid drain query"))
		return
	}
	s.tickModelTraceContext(ev.Context())
	s.handleGetModelTraceProbe(event.NewEventWithContext(proxyevents.TopicGetModelTraceProbe, s.ID(), s.ID(), event.NewHeader(), ev.Context(), proxyevents.GetModelTraceProbeCommand{SlotID: cmd.SlotID, ProbeID: cmd.ProbeID}), r)
}
