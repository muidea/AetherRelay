package biz

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestCodexStreamTimeoutAndFeedback(t *testing.T) {
	// CP-STREAM-003/013, CP-FAIL-019: real EventHub calls exercise context
	// propagation, output-before-retry, lease release and account feedback.
	for _, tc := range []struct {
		name    string
		want    codexresponses.ErrorKind
		neutral bool
	}{
		{"progress", "", false},
		{"cleanup_delay", codexresponses.KindNetwork, false},
		{"first", codexresponses.KindFirstEventTimeout, false},
		{"headers", codexresponses.KindFirstEventTimeout, false},
		{"parent_deadline", codexresponses.KindClientCanceled, true},
		{"idle", codexresponses.KindIdleTimeout, false},
		{"lifetime", codexresponses.KindStreamLifetime, true},
		{"cancel", codexresponses.KindClientCanceled, true},
		{"write", codexresponses.KindClientWrite, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := event.NewHub(24)
			background := task.NewBackgroundRoutine(8)
			t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
			accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
			var acquired, released, canceled, pulls atomic.Int32
			recorded := make(chan accevents.RecordResultCommand, 4)
			accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, result event.Result) {
				if acquired.Add(1) > 1 {
					result.Set(accevents.AcquireResult{UnavailableReason: "accounts_cooling", RetryAfterSeconds: 30}, cd.NewError(cd.Unexpected, "unavailable"))
					return
				}
				result.Set(accevents.AcquireResult{AccountID: "test-account", AccessToken: "test", LeaseID: "lease"}, nil)
			})
			accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, result event.Result) { released.Add(1); result.Set(accevents.ReleaseResult{}, nil) })
			accounts.Subscribe(accevents.TopicRecordResult, func(ev event.Event, result event.Result) {
				recorded <- ev.Data().(accevents.RecordResultCommand)
				result.Set(accevents.RecordResultResult{}, nil)
			})
			up := event.NewSimpleObserver(upcommon.UnitID, hub)
			up.Subscribe(upevents.TopicStart, func(ev event.Event, result event.Result) {
				if tc.name == "headers" {
					<-ev.Context().Done()
					result.Set(upevents.StartResult{ErrorClass: upevents.ErrorNetwork}, nil)
					return
				}
				result.Set(upevents.StartResult{StreamID: "stream"}, nil)
			})
			up.Subscribe(upevents.TopicPull, func(ev event.Event, result event.Result) {
				n := pulls.Add(1)
				if tc.name == "cleanup_delay" && n > 1 {
					result.Set(upevents.PullResult{Done: true, ErrorClass: upevents.ErrorNetwork}, nil)
					return
				}
				if tc.name == "first" || (tc.name == "idle" && n > 1) {
					<-ev.Context().Done()
					result.Set(nil, cd.NewError(cd.Unexpected, "pull canceled"))
					return
				}
				select {
				case <-time.After(15 * time.Millisecond):
				case <-ev.Context().Done():
					result.Set(nil, cd.NewError(cd.Unexpected, "pull canceled"))
					return
				}
				if n == 15 {
					result.Set(upevents.PullResult{Data: []byte("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{}]}}\n\n"), Done: true}, nil)
					return
				}
				result.Set(upevents.PullResult{Data: []byte("data: {\"type\":\"response.custom_tool_call_input.delta\",\"delta\":\"x\"}\n\n")}, nil)
			})
			up.Subscribe(upevents.TopicCancel, func(_ event.Event, result event.Result) {
				if tc.name == "cleanup_delay" {
					time.Sleep(200 * time.Millisecond)
				}
				canceled.Add(1)
				result.Set(upevents.CancelResult{}, nil)
			})
			cfg := config.Config{RequestTimeout: 20 * time.Millisecond, StreamFirstEventTimeout: 200 * time.Millisecond, StreamIdleTimeout: 150 * time.Millisecond}
			if tc.name == "lifetime" || tc.name == "cleanup_delay" {
				cfg.CodexOAuth.StreamMaxDuration = 100 * time.Millisecond
			}
			proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background), config: cfg}
			timeout := 3 * time.Second
			if tc.name == "parent_deadline" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			var firstEvent time.Duration
			err := proxy.StreamCodexResponses(ctx, codexresponses.Request{Model: "gpt-test", Body: []byte(`{"model":"gpt-test"}`)}, func(info codexresponses.StreamStart) error {
				firstEvent = info.FirstEventDuration
				return nil
			}, func([]byte) error {
				if tc.name == "cancel" {
					cancel()
				}
				if tc.name == "write" {
					return errors.New("client disconnected")
				}
				return nil
			})
			failure, _ := codexresponses.AsFailure(err)
			if (tc.want == "" && err != nil) || (tc.want != "" && (failure == nil || failure.Kind != tc.want)) {
				t.Fatalf("failure=%+v want=%s", failure, tc.want)
			}
			if tc.name == "progress" && firstEvent <= 0 {
				t.Fatalf("first event duration=%s", firstEvent)
			}
			if tc.name == "first" && failure.RetryAfterSeconds != firstEventTimeoutRetryAfter {
				t.Fatalf("retry after=%d", failure.RetryAfterSeconds)
			}
			wantCancels := int32(1)
			if tc.name == "headers" {
				wantCancels = 0
			}
			if released.Load() != 1 || canceled.Load() != wantCancels {
				t.Fatalf("release=%d cancel=%d", released.Load(), canceled.Load())
			}
			wantAcquires := int32(1)
			if tc.name == "first" || tc.name == "headers" {
				wantAcquires = 2
			}
			if acquired.Load() != wantAcquires {
				t.Fatalf("unexpected replay: acquired=%d", acquired.Load())
			}
			if tc.neutral {
				select {
				case r := <-recorded:
					t.Fatalf("local failure affected account: %+v", r)
				default:
				}
			} else {
				select {
				case r := <-recorded:
					wantClass := accevents.ErrorTimeout
					if tc.name == "cleanup_delay" {
						wantClass = accevents.ErrorNetwork
					}
					if r.Success != (tc.want == "") || (!r.Success && r.ErrorClass != wantClass) {
						t.Fatalf("feedback=%+v", r)
					}
					if tc.name == "first" && r.RetryAfterSeconds != firstEventTimeoutRetryAfter {
						t.Fatalf("first-event feedback=%+v", r)
					}
				default:
					t.Fatal("missing account feedback")
				}
			}
		})
	}
}

func TestCodexStreamCommentsDoNotExtendDeadline(t *testing.T) {
	// CP-STREAM-013: both pre-output and post-output comment traffic is inert.
	for _, output := range []bool{false, true} {
		t.Run(fmt.Sprint(output), func(t *testing.T) {
			ctx, g := newCodexStreamGuard(context.Background(), 60*time.Millisecond, 60*time.Millisecond, 0)
			defer g.close()
			want := codexresponses.KindFirstEventTimeout
			if output {
				g.observe([]byte(`data: {"type":"response.output_text.delta","delta":"a"}`))
				want = codexresponses.KindIdleTimeout
			}
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			limit := time.NewTimer(time.Second)
			defer limit.Stop()
			for {
				select {
				case <-ticker.C:
					g.observe([]byte(": keepalive\n\n"))
				case <-ctx.Done():
					f, _ := codexresponses.AsFailure(context.Cause(ctx))
					if f == nil || f.Kind != want {
						t.Fatalf("cause=%v", context.Cause(ctx))
					}
					return
				case <-limit.C:
					t.Fatal("comments extended deadline")
				}
			}
		})
	}
}

func TestCodexStreamGuardKeepsFirstEventTimestamp(t *testing.T) {
	_, guard := newCodexStreamGuard(context.Background(), 0, 0, 0)
	defer guard.close()
	if !guard.observe([]byte(`data: {"type":"response.output_text.delta","delta":"a"}`)) {
		t.Fatal("first data line was not identified")
	}
	first := guard.firstEventDuration()
	time.Sleep(10 * time.Millisecond)
	if guard.observe([]byte(`data: {"type":"response.output_text.delta","delta":"b"}`)) {
		t.Fatal("second data line was identified as first")
	}
	if got := guard.firstEventDuration(); got != first {
		t.Fatalf("first event duration changed from %s to %s", first, got)
	}
	guard.mu.Lock()
	last := guard.lastEvent.Sub(guard.started)
	guard.mu.Unlock()
	if last <= first {
		t.Fatalf("last event duration=%s first=%s", last, first)
	}
}

func TestCodexAcquirePreservesAdmissionHintAndReleasesCanceledLease(t *testing.T) {
	// CP-FAIL-019: denial data survives the EventHub error; cancellation after
	// successful acquisition must not discard a live lease.
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(4)
	defer func() { background.Shutdown(nil); hub.Terminate(context.Background()) }()
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	var released atomic.Int32
	accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, r event.Result) { released.Add(1); r.Set(accevents.ReleaseResult{}, nil) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accounts.Subscribe(accevents.TopicAcquire, func(ev event.Event, r event.Result) {
		if ev.Data().(accevents.AcquireCommand).Model == "denied" {
			r.Set(accevents.AcquireResult{UnavailableReason: "accounts_cooling", RetryAfterSeconds: 23}, cd.NewError(cd.Unexpected, "denied"))
			return
		}
		cancel()
		r.Set(accevents.AcquireResult{AccountID: "test", AccessToken: "test", LeaseID: "test"}, nil)
	})
	p := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	_, err := p.acquireCodexAccount(context.Background(), "denied", nil)
	f, _ := codexresponses.AsFailure(err)
	if f == nil || f.Kind != codexresponses.KindProviderUnavailable || f.RetryAfterSeconds != 23 || f.UnavailableReason != "accounts_cooling" {
		t.Fatalf("denial=%+v", f)
	}
	_, err = p.acquireCodexAccount(ctx, "canceled", nil)
	f, _ = codexresponses.AsFailure(err)
	if f == nil || f.Kind != codexresponses.KindClientCanceled || released.Load() != 1 {
		t.Fatalf("failure=%+v releases=%d", f, released.Load())
	}
}
