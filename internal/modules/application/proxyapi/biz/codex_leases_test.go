package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/usagetarget"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

func TestReleaseRetriesCanceledRequestAndPreservesOriginalError(t *testing.T) {
	for _, mode := range []string{"transient", "invalid_ack", "permanent", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			hub := event.NewHub(16)
			defer hub.Terminate(context.Background())
			observer := event.NewSimpleObserver(acccommon.UnitID, hub)
			calls, reports, decrements := 0, 0, 0
			observer.Subscribe(accevents.TopicRelease, func(ev event.Event, r event.Result) {
				calls++
				if ev.Context().Err() != nil {
					t.Error("release inherited canceled request")
				}
				if mode == "permanent" || mode == "transient" && calls == 1 {
					r.Set(nil, cd.NewError(cd.ResourceExhausted, "original detail: owner queue full, lease=full-id"))
					return
				}
				if mode == "invalid_ack" && calls == 1 {
					r.Set(nil, nil)
					return
				}
				if mode == "duplicate" {
					r.Set(accevents.ReleaseResult{Released: false}, nil)
					return
				}
				decrements++
				r.Set(accevents.ReleaseResult{Released: true}, nil)
			})
			observer.Subscribe(accevents.TopicReleaseFailure, func(_ event.Event, r event.Result) { reports++; r.Set(accevents.ReleaseFailureResult{}, nil) })
			var log bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&log, nil)))
			defer slog.SetDefault(old)
			p := &Proxy{Base: basebiz.New("test", hub, nil)}
			ctx, cancel := context.WithCancel(codexresponses.WithExecutionIdentity(usagetarget.WithTracker(context.Background()), codexresponses.ExecutionIdentity{RequestID: "request-full", EventID: "event-full", APIKeyID: "claude-office"}))
			usagetarget.Record(ctx, "codexoauth", "account-full", "owner@example.com")
			cancel()
			p.releaseCodexAccount(ctx, "full-id")
			wantCalls, wantFailures, wantDecrements := 2, 1, 1
			if mode == "permanent" {
				wantCalls, wantFailures, wantDecrements = 3, 3, 0
			}
			if mode == "duplicate" {
				wantCalls, wantFailures, wantDecrements = 1, 0, 0
			}
			if calls != wantCalls || reports != wantFailures || decrements != wantDecrements {
				t.Fatal(calls, reports, decrements)
			}
			if mode == "transient" || mode == "permanent" {
				if !strings.Contains(log.String(), "original detail: owner queue full, lease=full-id") || !strings.Contains(log.String(), `"event_id":"event-full"`) {
					t.Fatal(log.String())
				}
			}
			if strings.Contains(log.String(), "release unresolved") != (mode == "permanent") {
				t.Fatal(log.String())
			}
		})
	}
}

func TestConcurrentDuplicateReleaseDoesNotDecrementTwice(t *testing.T) {
	hub := event.NewHub(32)
	defer hub.Terminate(context.Background())
	observer := event.NewSimpleObserver(acccommon.UnitID, hub)
	mu := sync.Mutex{}
	remaining, calls := 1, 0
	observer.Subscribe(accevents.TopicRelease, func(_ event.Event, r event.Result) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		released := remaining > 0
		if released {
			remaining--
		}
		r.Set(accevents.ReleaseResult{Released: released}, nil)
	})
	p := &Proxy{Base: basebiz.New("test", hub, nil)}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); p.releaseCodexAccount(context.Background(), "same-lease") }()
	}
	wg.Wait()
	if remaining != 0 || calls != 8 {
		t.Fatal(remaining, calls)
	}
}

func TestWebsocketStateAcknowledgmentIsChecked(t *testing.T) {
	hub := event.NewHub(8)
	defer hub.Terminate(context.Background())
	observer := event.NewSimpleObserver(acccommon.UnitID, hub)
	updated := true
	observer.Subscribe(accevents.TopicLeaseState, func(_ event.Event, r event.Result) { r.Set(accevents.LeaseStateResult{Updated: updated}, nil) })
	var log bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, nil)))
	defer slog.SetDefault(old)
	p := &Proxy{Base: basebiz.New("test", hub, nil)}
	p.observeCodexLeaseState(context.Background(), "lease", accevents.LeaseIdle)
	if log.Len() != 0 {
		t.Fatal("successful typed acknowledgment treated as failure", log.String())
	}
	updated = false
	p.observeCodexLeaseState(context.Background(), "lease", accevents.LeaseIdle)
	var record map[string]any
	if err := json.Unmarshal(log.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["msg"] != "Codex lease state observation failed" {
		t.Fatal(record)
	}
}
