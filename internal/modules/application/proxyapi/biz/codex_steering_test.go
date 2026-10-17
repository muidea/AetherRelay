package biz

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"aetherrelay/internal/modules/blocks/codexupstream/pkg/steering"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type steeringFixture struct {
	mu                                                sync.Mutex
	denied                                            map[string]string
	versions                                          map[string]string
	snapshots                                         map[string]upevents.SteeringSnapshotResult
	acquired                                          map[string]int
	starts, releases, closes, refreshes, healthWrites int
	usageSnapshots                                    []accevents.MergeUsageSnapshotCommand
	scopedIdentity                                    bool
	closeFailures                                     int
	refreshNeeded                                     bool
	recheckReason, recheckVersion, recheckLeaseID     string
	recheckError                                      bool
	recheckWait                                       bool
	recheckCancel                                     func()
}

func newSteeringFixture(t *testing.T) (*Proxy, *steeringFixture) {
	t.Helper()
	hub := event.NewHub(64)
	bg := task.NewBackgroundRoutine(8)
	p := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, bg)}
	f := &steeringFixture{denied: map[string]string{}, versions: map[string]string{}, snapshots: map[string]upevents.SteeringSnapshotResult{}, acquired: map[string]int{}}
	a := event.NewSimpleObserver(acccommon.UnitID, hub)
	if err := a.Subscribe(accevents.TopicProbeAdmission, func(ev event.Event, r event.Result) {
		cmd := ev.Data().(accevents.ProbeAdmissionCommand)
		f.mu.Lock()
		defer f.mu.Unlock()
		if cmd.LeaseID != "" {
			f.recheckLeaseID = cmd.LeaseID
			if f.recheckWait {
				<-ev.Context().Done()
			}
			if f.recheckCancel != nil {
				f.recheckCancel()
			}
			if f.recheckError {
				r.Set(nil, cd.NewError(cd.Unexpected, "owner unavailable"))
				return
			}
			if f.recheckReason != "" {
				f.denied[cmd.AccountID] = f.recheckReason
			}
			if f.recheckVersion != "" {
				f.versions[cmd.AccountID] = f.recheckVersion
			}
		}
		version := f.versions[cmd.AccountID]
		if version == "" {
			version = "v1"
		}
		out := accevents.ProbeAdmissionResult{Allowed: f.denied[cmd.AccountID] == "", Reason: f.denied[cmd.AccountID], ConfigVersion: version, AccountLabel: "probe@example.com", RefreshNeeded: f.refreshNeeded}
		if out.Reason == "pool_stopping" {
			out.ConfigVersion = ""
		}
		if out.Allowed && cmd.Acquire {
			f.acquired[cmd.AccountID]++
			out.Credential = accevents.AcquireResult{AccountID: cmd.AccountID, AccountLabel: out.AccountLabel, AccessToken: "test-secret", LeaseID: "lease:" + cmd.AccountID}
		}
		if out.Allowed && cmd.Acquire && f.scopedIdentity {
			out.Credential.FingerprintMode = accevents.FingerprintModeScoped
			out.Credential.FingerprintSeed = "7327df33-9db6-4a25-afc5-387db8221acf"
		}
		r.Set(out, nil)
	}); err != nil {
		t.Fatal(err)
	}
	a.Subscribe(accevents.TopicRefreshToken, func(ev event.Event, r event.Result) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.refreshes++
		r.Set(accevents.RefreshTokenResult{AccessToken: "refreshed-test-token", RequestSent: true}, nil)
	})
	a.Subscribe(accevents.TopicMergeUsageSnapshot, func(ev event.Event, r event.Result) {
		f.mu.Lock()
		f.usageSnapshots = append(f.usageSnapshots, ev.Data().(accevents.MergeUsageSnapshotCommand))
		f.mu.Unlock()
		r.Set(nil, nil)
	})
	a.Subscribe(accevents.TopicRelease, func(ev event.Event, r event.Result) {
		f.mu.Lock()
		f.releases++
		f.mu.Unlock()
		r.Set(accevents.ReleaseResult{Released: true}, nil)
	})
	for _, topic := range []string{accevents.TopicRecordResult, accevents.TopicRecordTransportCapability} {
		a.Subscribe(topic, func(_ event.Event, r event.Result) { f.mu.Lock(); f.healthWrites++; f.mu.Unlock(); r.Set(nil, nil) })
	}
	u := event.NewSimpleObserver(upcommon.UnitID, hub)
	u.Subscribe(upevents.TopicSteeringStart, func(ev event.Event, r event.Result) {
		cmd := ev.Data().(upevents.SteeringStartCommand)
		f.mu.Lock()
		defer f.mu.Unlock()
		if cmd.SessionHash == cmd.ClientSessionHash || cmd.ClientSessionHash != cmd.SessionHash+":client" {
			t.Error("probe sessions not isolated")
		}
		if f.scopedIdentity && (cmd.Fingerprint.SessionID == cmd.ClientFingerprint.SessionID || cmd.Fingerprint.ThreadID == cmd.ClientFingerprint.ThreadID || cmd.Fingerprint.WindowID == cmd.ClientFingerprint.WindowID || cmd.Fingerprint.TurnID == cmd.ClientFingerprint.TurnID || cmd.Fingerprint.InstallationID != cmd.ClientFingerprint.InstallationID) {
			t.Error("scoped identity not isolated per check")
		}

		f.starts++
		e := steering.NewEvidence()
		e.Phase = "steering"
		e.ExecutionOutcome = "awaiting_commit"
		e.UpstreamRequestSent = true
		e.CreateSent = true
		e.SteerSent = true
		e.WebsocketState = "connected"
		f.snapshots[cmd.ProbeID] = upevents.SteeringSnapshotResult{Evidence: e, Endpoint: "wss://chatgpt.com/backend-api/codex/responses"}
		r.Set(upevents.SteeringStartResult{ProbeID: cmd.ProbeID}, nil)
	})
	u.Subscribe(upevents.TopicSteeringSnapshot, func(ev event.Event, r event.Result) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.Set(f.snapshots[ev.Data().(upevents.SteeringSnapshotCommand).ProbeID], nil)
	})
	u.Subscribe(upevents.TopicSteeringClose, func(ev event.Event, r event.Result) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.closes++
		if f.closeFailures > 0 {
			f.closeFailures--
			r.Set(nil, cd.NewError(cd.Timeout, "not yet drained"))
			return
		}
		id := ev.Data().(upevents.SteeringCloseCommand).ProbeID
		snap, ok := f.snapshots[id]
		delete(f.snapshots, id)
		r.Set(upevents.SteeringCloseResult{Closed: ok, Snapshot: snap}, nil)
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := p.drainSteering(ctx); err != nil {
			t.Error(err)
		}
		bg.Shutdown(ctx)
		hub.Terminate(ctx)
	})
	return p, f
}
func getSteeringTest(t *testing.T, p *Proxy, slot, id string) proxyevents.SteeringProbeResult {
	t.Helper()
	r := event.NewResult(proxyevents.TopicGetSteeringProbe, "test", p.ID())
	p.handleGetSteeringProbe(event.NewEvent(proxyevents.TopicGetSteeringProbe, "test", p.ID(), nil, proxyevents.GetSteeringProbeCommand{SlotID: slot, ProbeID: id}), r)
	v, err := r.Get()
	if err != nil {
		t.Fatal(err)
	}
	return v.(proxyevents.SteeringProbeResult)
}
func waitSteeringTest(t *testing.T, p *Proxy, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.tickSteering()
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("steering task did not reach expected state")
}

func TestSteeringAdmissionRejectedWithoutExecution(t *testing.T) {
	p, f := newSteeringFixture(t)
	f.denied["slot"] = "model_blocked"
	out := p.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: "slot", Model: "gpt-test", IdempotencyKey: "one"})
	e := out.Report
	if out.Status != 202 || !e.Done || e.AdmissionState != "rejected" || e.ActualSlotID != nil || e.UpstreamRequestSent || e.ExecutionOutcome != "not_started" || e.AuthState != "not_observed" {
		t.Fatal(out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starts != 0 || len(f.acquired) != 0 || f.releases != 0 || f.healthWrites != 0 {
		t.Fatal("local rejection touched upstream/lease/health", f)
	}
}

func TestSteeringFailureDiagnosticsSurviveCleanupAndQueries(t *testing.T) {
	for _, tc := range []struct{ code, capability, outcome string }{
		{"future_error", steering.Unknown, "upstream_error"},
		{"unsupported_native_inflight_message", steering.Unsupported, "unsupported_native_inflight_message"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			p, f := newSteeringFixture(t)
			cmd := proxyevents.StartSteeringProbeCommand{SlotID: "slot", Model: "gpt-test", IdempotencyKey: "diagnostic"}
			first := p.startSteeringProbe(context.Background(), cmd)
			waitSteeringTest(t, p, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.starts == 1 })
			m := steering.NewMachine()
			m.HTTPStatus = 101
			m.WriteAttempt("response.create")
			m.WriteResult("response.create", nil)
			m.Observe([]byte(`{"type":"response.created","response":{"id":"resp_parent"}}`))
			m.WriteAttempt("response.steer")
			m.WriteResult("response.steer", nil)
			m.Observe([]byte(`{"type":"error","error":{"code":"` + tc.code + `","message":"management detail"}}`))
			f.mu.Lock()
			snap := f.snapshots[first.Report.ProbeID]
			snap.Evidence = m.Evidence.Clone()
			f.snapshots[first.Report.ProbeID] = snap
			f.mu.Unlock()
			waitSteeringTest(t, p, func() bool { return getSteeringTest(t, p, "slot", first.Report.ProbeID).Report.State == "finished" })
			for _, query := range []string{first.Report.ProbeID, "latest", "idempotent"} {
				var result proxyevents.SteeringProbeResult
				if query == "idempotent" {
					result = p.startSteeringProbe(context.Background(), cmd)
				} else {
					result = getSteeringTest(t, p, "slot", query)
				}
				e := result.Report
				if e.Capability != tc.capability || e.ErrorCode != tc.outcome || e.ExecutionOutcome != tc.outcome || !e.SteerWriteCompleted || e.UpstreamError == nil || e.UpstreamError.Code != tc.code || e.UpstreamError.Message != "management detail" || len(e.Timeline) != len(m.Timeline) {
					t.Fatal("management projection lost failure evidence", e)
				}
				e.UpstreamError.Message = "reader mutation"
				e.Timeline[0].Type = "reader mutation"
			}
			if m.UpstreamError.Message != "management detail" || snap.Evidence.UpstreamError.Message != "management detail" {
				t.Fatal("snapshot mutated")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.starts != 1 || f.releases != 1 || f.healthWrites != 0 {
				t.Fatal("diagnostics changed execution or health", f)
			}
		})
	}
}

func TestSteeringTerminalEvidenceSurvivesDelayedController(t *testing.T) {
	for _, reason := range []string{"timeout", "canceled"} {
		for _, outcome := range []string{"waiting_for_required_input", "completed", "successor_failed", "awaiting_commit"} {
			t.Run(reason+"/"+outcome, func(t *testing.T) {
				p, f := newSteeringFixture(t)
				first := p.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: "slot", Model: "gpt-test", IdempotencyKey: "one"})
				waitSteeringTest(t, p, func() bool {
					p.steering.mu.Lock()
					defer p.steering.mu.Unlock()
					return p.steering.jobs[first.Report.ProbeID].monitoring
				})
				f.mu.Lock()
				snap := f.snapshots[first.Report.ProbeID]
				snap.Evidence.Done = outcome != "awaiting_commit"
				snap.Evidence.ExecutionOutcome = outcome
				snap.Evidence.Capability = steering.Supported
				if outcome == "waiting_for_required_input" {
					snap.Evidence.Capability = steering.Pending
				}
				if outcome == "successor_failed" {
					snap.Evidence.ErrorCode = outcome
				}
				f.snapshots[first.Report.ProbeID] = snap
				f.mu.Unlock()
				p.steering.mu.Lock()
				job := p.steering.jobs[first.Report.ProbeID]
				job.cancel()
				if reason == "timeout" {
					job.ctx, job.cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				}
				p.steering.mu.Unlock()
				waitSteeringTest(t, p, func() bool { return getSteeringTest(t, p, "slot", first.Report.ProbeID).Report.Done })
				final := getSteeringTest(t, p, "slot", first.Report.ProbeID).Report
				want := outcome
				if outcome == "awaiting_commit" {
					want = reason
				}
				if final.ExecutionOutcome != want || final.Capability != snap.Evidence.Capability {
					t.Fatal(final)
				}
			})
		}
	}
}

func TestSteeringIdempotencyCommitCleanupAndHistory(t *testing.T) {
	p, f := newSteeringFixture(t)
	cmd := proxyevents.StartSteeringProbeCommand{SlotID: "slot", Model: "gpt-test", IdempotencyKey: "one"}
	first := p.startSteeringProbe(context.Background(), cmd)
	if first.Status != 202 {
		t.Fatal(first)
	}
	retry := p.startSteeringProbe(context.Background(), cmd)
	if retry.Report.ProbeID != first.Report.ProbeID {
		t.Fatal("duplicate generated new task")
	}
	different := cmd
	different.Model = "other"
	if got := p.startSteeringProbe(context.Background(), different); got.Status != 409 {
		t.Fatal(got)
	}
	different = cmd
	different.IdempotencyKey = "two"
	if got := p.startSteeringProbe(context.Background(), different); got.Status != 409 {
		t.Fatal(got)
	}
	waitSteeringTest(t, p, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.starts == 1 })
	f.mu.Lock()
	snap := f.snapshots[first.Report.ProbeID]
	snap.Evidence.Capability = steering.Supported
	snap.Evidence.Done = true
	snap.Evidence.ExecutionOutcome = "successor_failed"
	snap.Evidence.ErrorCode = "successor_failed"
	f.snapshots[first.Report.ProbeID] = snap
	f.closeFailures = 1
	f.mu.Unlock()
	p.tickSteering()
	if got := p.startSteeringProbe(context.Background(), different); got.Status != 409 {
		t.Fatal("released slot before engine drained", got)
	}
	waitSteeringTest(t, p, func() bool { return getSteeringTest(t, p, "slot", first.Report.ProbeID).Report.State == "finished" })
	final := getSteeringTest(t, p, "slot", "latest").Report
	if final.Capability != steering.Supported || final.ExecutionOutcome != "successor_failed" || final.FinishedAt == "" || final.ExpiresAt == "" {
		t.Fatal(final)
	}
	if p.startSteeringProbe(context.Background(), cmd).Report.ProbeID != first.Report.ProbeID {
		t.Fatal("completed key replayed")
	}
	if getSteeringTest(t, p, "other-slot", first.Report.ProbeID).Status != 404 {
		t.Fatal("cross-slot lookup leaked")
	}
	f.mu.Lock()
	f.versions["slot"] = "v2"
	if f.releases != 1 || f.starts != 1 || f.healthWrites != 0 {
		t.Fatal(f)
	}
	f.mu.Unlock()
	if !getSteeringTest(t, p, "slot", first.Report.ProbeID).Report.Historical {
		t.Fatal("report presented as current after config change")
	}
	p.steering.mu.Lock()
	p.steering.jobs[first.Report.ProbeID].finished = time.Now().Add(-31 * time.Minute)
	p.steering.mu.Unlock()
	if getSteeringTest(t, p, "slot", first.Report.ProbeID).Status != 404 {
		t.Fatal("expired report retained")
	}
}

func TestSteeringCapacityQueueBudgetAndShutdown(t *testing.T) {
	p, f := newSteeringFixture(t)
	var reports []proxyevents.SteeringReport
	for i := 0; i < 34; i++ {
		got := p.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: fmt.Sprint(i), Model: "gpt-test", IdempotencyKey: fmt.Sprint(i)})
		if got.Status != 202 {
			t.Fatal(i, got)
		}
		reports = append(reports, got.Report)
	}
	if got := p.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: "overflow", Model: "gpt-test", IdempotencyKey: "overflow"}); got.Status != 429 {
		t.Fatal(got)
	}
	waitSteeringTest(t, p, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.starts == 2 })
	p.steering.mu.Lock()
	queued := p.steering.jobs[reports[2].ProbeID]
	queued.created = time.Now().Add(-121 * time.Second)
	p.steering.mu.Unlock()
	p.tickSteering()
	expired := getSteeringTest(t, p, "2", queued.report.ProbeID).Report
	if !expired.Done || expired.ExecutionOutcome != "timeout" || expired.UpstreamRequestSent || expired.ActualSlotID != nil {
		t.Fatal(expired)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.drainSteering(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if f.starts != 2 || f.releases != 2 || f.healthWrites != 0 {
		t.Fatal(f)
	}
	f.mu.Unlock()
	if got := p.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: "new", Model: "gpt-test", IdempotencyKey: "new"}); got.Status != 503 {
		t.Fatal(got)
	}
}

func TestSteeringExecutionRechecksAdmissionAndConfig(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			p, f := newSteeringFixture(t)
			got := p.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: "slot", Model: "gpt-test", IdempotencyKey: "key"})
			waitSteeringTest(t, p, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.starts == 1 })
			f.mu.Lock()
			if committed {
				snap := f.snapshots[got.Report.ProbeID]
				snap.Evidence.Capability = steering.Supported
				f.snapshots[got.Report.ProbeID] = snap
			}
			f.versions["slot"] = "new-authorization"
			f.mu.Unlock()
			waitSteeringTest(t, p, func() bool { return getSteeringTest(t, p, "slot", got.Report.ProbeID).Report.State == "finished" })
			out := getSteeringTest(t, p, "slot", got.Report.ProbeID).Report
			want := steering.Unknown
			if committed {
				want = steering.Supported
			}
			if out.Capability != want || out.ExecutionOutcome != "account_changed" || !out.Historical {
				t.Fatal(out)
			}
		})
	}
}

func TestSteeringConcurrentIdempotencyAndStorageBound(t *testing.T) {
	p, f := newSteeringFixture(t)
	cmd := proxyevents.StartSteeringProbeCommand{SlotID: "slot", Model: "gpt-test", IdempotencyKey: "same-key"}
	results := make(chan proxyevents.SteeringProbeResult, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- p.startSteeringProbe(context.Background(), cmd) }()
	}
	wg.Wait()
	close(results)
	id := ""
	for got := range results {
		if got.Status != 202 {
			t.Fatal(got)
		}
		if id != "" && got.Report.ProbeID != id {
			t.Fatal("concurrent duplicate created a task")
		}
		id = got.Report.ProbeID
	}
	waitSteeringTest(t, p, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.starts == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.drainSteering(ctx); err != nil {
		t.Fatal(err)
	}

	p2, f2 := newSteeringFixture(t)
	for i := 0; i < 200; i++ {
		slot := fmt.Sprint(i)
		f2.denied[slot] = "account_disabled"
		got := p2.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: slot, Model: "gpt-test", IdempotencyKey: slot})
		if got.Status != 202 || !got.Report.Done {
			t.Fatal(i, got)
		}
	}
	if got := p2.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: "overflow", Model: "gpt-test", IdempotencyKey: "overflow"}); got.Status != 429 {
		t.Fatal("unbounded report storage", got)
	}
	if got := p2.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: "0", Model: "gpt-test", IdempotencyKey: "0"}); got.Status != 202 {
		t.Fatal("capacity broke retained idempotency", got)
	}
}

func TestSteeringQueuedAdmissionRefusalHasNoLeaseOrUpstream(t *testing.T) {
	p, f := newSteeringFixture(t)
	var jobs []proxyevents.SteeringReport
	for i := 0; i < 3; i++ {
		slot := fmt.Sprint(i)
		jobs = append(jobs, p.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: slot, Model: "gpt-test", IdempotencyKey: slot}).Report)
	}
	waitSteeringTest(t, p, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.starts == 2 })
	f.mu.Lock()
	f.denied["2"] = "usage_limit"
	first := f.snapshots[jobs[0].ProbeID]
	first.Evidence.Done = true
	first.Evidence.ExecutionOutcome = "completed"
	f.snapshots[jobs[0].ProbeID] = first
	f.mu.Unlock()
	waitSteeringTest(t, p, func() bool { return getSteeringTest(t, p, "2", jobs[2].ProbeID).Report.State == "finished" })
	out := getSteeringTest(t, p, "2", jobs[2].ProbeID).Report
	if out.AdmissionState != "rejected" || out.UpstreamRequestSent || out.ActualSlotID != nil || out.ExecutionOutcome != "not_started" {
		t.Fatal(out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starts != 2 || f.acquired["2"] != 0 {
		t.Fatal("queue skipped admission", f)
	}
}

func TestSteeringCanonicalSlotTaskIdentity(t *testing.T) {
	for _, initialSlot := range []string{"slot", " \tslot\n "} {
		t.Run(initialSlot, func(t *testing.T) {
			p, f := newSteeringFixture(t)
			first := p.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: initialSlot, Model: "gpt-test", IdempotencyKey: "same"})
			if first.Status != 202 || first.Report.TargetSlotID != "slot" {
				t.Fatal(first)
			}
			for _, slot := range []string{"slot", " slot ", "\tslot\n"} {
				retry := p.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: slot, Model: "gpt-test", IdempotencyKey: "same"})
				if retry.Status != 202 || retry.Report.ProbeID != first.Report.ProbeID {
					t.Fatal("alias broke idempotency", retry)
				}
				duplicate := p.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: slot, Model: "gpt-test", IdempotencyKey: "different"})
				if duplicate.Status != 409 || duplicate.ErrorCode != "slot_probe_active" {
					t.Fatal("alias bypassed single-slot task limit", duplicate)
				}
				for _, id := range []string{first.Report.ProbeID, "latest"} {
					if got := getSteeringTest(t, p, slot, id); got.Status != 200 || got.Report.ProbeID != first.Report.ProbeID {
						t.Fatal("alias lost query identity", got)
					}
				}
			}
			if got := getSteeringTest(t, p, "another-slot", first.Report.ProbeID); got.Status != 404 {
				t.Fatal("cross-slot query exposed report", got)
			}
			if got := p.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: " \t\n ", Model: "gpt-test", IdempotencyKey: "blank"}); got.Status != 400 {
				t.Fatal("blank canonical slot accepted", got)
			}
			waitSteeringTest(t, p, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.starts == 1 })
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.acquired["slot"] != 1 || len(f.acquired) != 1 || f.recheckLeaseID != "lease:slot" {
				t.Fatal("canonical account/lease was not reused", f.acquired, f.recheckLeaseID)
			}
		})
	}
}

func TestSteeringRecheckRefusalPreventsWSAndReleasesLease(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		for _, reason := range []string{"usage_limit", "rate_limit", "probe_lease_unavailable", "pool_stopping"} {
			t.Run(fmt.Sprintf("refresh=%v/%s", refresh, reason), func(t *testing.T) {
				p, f := newSteeringFixture(t)
				f.refreshNeeded, f.recheckReason = refresh, reason
				first := p.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: "slot", Model: "gpt-test", IdempotencyKey: "one"})
				waitSteeringTest(t, p, func() bool { return getSteeringTest(t, p, "slot", first.Report.ProbeID).Report.Done })
				out := getSteeringTest(t, p, "slot", first.Report.ProbeID).Report
				if out.AdmissionState != "rejected" || out.AdmissionReason != reason || out.Phase != "admission" || out.ExecutionOutcome != "not_started" || out.Capability != steering.Unknown || out.Historical {
					t.Fatal(out)
				}
				if out.CredentialRefreshPerformed != refresh || out.UpstreamRequestSent != refresh || (refresh && out.CredentialRefreshResult != "succeeded") || out.CreateSent || out.SteerSent || out.WebsocketState != "not_observed" || out.ActualSlotID == nil || *out.ActualSlotID != "slot" {
					t.Fatal("refusal lost refresh evidence or fabricated WS evidence", out)
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				wantRefresh := 0
				if refresh {
					wantRefresh = 1
				}
				if f.starts != 0 || f.closes != 0 || f.releases != 1 || f.acquired["slot"] != 1 || f.healthWrites != 0 || f.refreshes != wantRefresh || f.recheckLeaseID != "lease:slot" {
					t.Fatal("refusal failed isolation or lease cleanup", f)
				}
				p.steering.mu.Lock()
				defer p.steering.mu.Unlock()
				if p.steering.running != 0 {
					t.Fatal("refusal leaked worker reservation")
				}
			})
		}
	}
}

func TestSteeringRecheckFailureClassification(t *testing.T) {
	for _, reason := range []string{"account_changed", "admission_unavailable", "canceled"} {
		t.Run(reason, func(t *testing.T) {
			p, f := newSteeringFixture(t)
			switch reason {
			case "account_changed":
				f.recheckVersion, f.recheckReason = "v2", "account_disabled"
			case "admission_unavailable":
				f.recheckError = true
			case "canceled":
				f.recheckCancel = p.stopSteering
			}
			first := p.startSteeringProbe(context.Background(), proxyevents.StartSteeringProbeCommand{SlotID: "slot", Model: "gpt-test", IdempotencyKey: "one"})
			waitSteeringTest(t, p, func() bool { return getSteeringTest(t, p, "slot", first.Report.ProbeID).Report.Done })
			out := getSteeringTest(t, p, "slot", first.Report.ProbeID).Report
			want := reason
			if out.ExecutionOutcome != want || out.ErrorCode != want || out.UpstreamRequestSent || out.Historical != (reason == "account_changed") {
				t.Fatal(out)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.starts != 0 || f.releases != 1 || f.healthWrites != 0 {
				t.Fatal(f)
			}
		})
	}
}

func TestSteeringRecheckDeadlineIsNotAccountChange(t *testing.T) {
	p, f := newSteeringFixture(t)
	f.recheckWait = true
	job := &codexSteeringTask{
		created: time.Now().Add(-steeringBudget + 250*time.Millisecond),
		report: proxyevents.SteeringReport{
			Evidence: steering.NewEvidence(), ProbeID: "deadline-test", TargetSlotID: "slot", Model: "gpt-test", State: "running",
		},
	}
	p.steering.jobs = map[string]*codexSteeringTask{job.report.ProbeID: job}
	p.steering.running = 1
	p.steering.wg.Add(1)
	p.runSteeringTask(job)
	out := getSteeringTest(t, p, "slot", job.report.ProbeID).Report
	if !out.Done || out.ExecutionOutcome != "timeout" || out.ErrorCode != "timeout" || out.Historical || out.UpstreamRequestSent {
		t.Fatal(out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starts != 0 || f.releases != 1 || f.healthWrites != 0 || f.recheckLeaseID != "lease:slot" {
		t.Fatal("deadline did not stop and release admitted task", f)
	}
}

// CP-PROBE-001/003: final result and fallback diagnostics survive cleanup, all query forms, and
// arbitrary reader mutations without affecting scheduling or health.
func TestSteeringFallbackReportProjectionAndCleanup(t *testing.T) {
	p, f := newSteeringFixture(t)
	f.scopedIdentity = true
	cmd := proxyevents.StartSteeringProbeCommand{SlotID: "slot", Model: "gpt-test", IdempotencyKey: "fallback"}
	first := p.startSteeringProbe(context.Background(), cmd)
	waitSteeringTest(t, p, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.starts == 1 })
	n := steering.NewEvidence()
	n.Capability = steering.Unsupported
	n.ExecutionOutcome = "unsupported_native_inflight_message"
	n.ErrorCode = n.ExecutionOutcome
	n.Done = true
	n.UpstreamError = &steering.UpstreamError{Code: n.ErrorCode}
	c := steering.NewClientEvidence()
	c.Capability = steering.Supported
	c.ExecutionOutcome = "completed"
	c.Done = true
	c.InputQueued = true
	c.ContinuationSent = true
	c.ContinuationWriteCompleted = true
	c.Timeline = []steering.TraceEvent{{Type: "input.queued"}}
	tokens := int64(4)
	c.ParentUsage = &steering.Usage{InputTokens: &tokens}
	f.mu.Lock()
	snap := f.snapshots[first.Report.ProbeID]
	snap.Evidence = c.Clone()
	snap.Fallback = steering.NewFallback(n, "native_protocol_unsupported")
	snap.Headers = []upevents.Header{{Name: "x-codex-primary-used-percent", Value: "7"}}
	snap.ClientHeaders = []upevents.Header{{Name: "x-codex-primary-used-percent", Value: "8"}}
	f.snapshots[first.Report.ProbeID] = snap
	f.mu.Unlock()
	waitSteeringTest(t, p, func() bool { return getSteeringTest(t, p, "slot", first.Report.ProbeID).Report.State == "finished" })
	for _, id := range []string{first.Report.ProbeID, "latest", "idempotent"} {
		var report proxyevents.SteeringReport
		if id == "idempotent" {
			report = p.startSteeringProbe(context.Background(), cmd).Report
		} else {
			report = getSteeringTest(t, p, "slot", id).Report
		}
		if report.Capability != steering.Supported || report.CapabilityTarget != steering.ClientTarget || !report.UpstreamRequestSent || report.Fallback == nil || report.Fallback.UpstreamError.Code != n.ErrorCode || !report.ContinuationWriteCompleted || *report.ParentUsage.InputTokens != 4 || report.Timeline[0].Type != "input.queued" {
			t.Fatal(report)
		}
		report.Fallback.UpstreamError.Code = "mutated"
		report.Timeline[0].Type = "mutated"
		*report.ParentUsage.InputTokens = 99
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starts != 1 || f.releases != 1 || f.closes != 1 || f.healthWrites != 0 || len(f.usageSnapshots) != 2 {
		t.Fatal(f)
	}
	for i, percent := range []float64{7, 8} {
		if f.usageSnapshots[i].AccountID != "slot" || f.usageSnapshots[i].Snapshot.Windows[0].UsedPercent != percent {
			t.Fatal("handshake quota observation lost", f.usageSnapshots)
		}
	}
}
