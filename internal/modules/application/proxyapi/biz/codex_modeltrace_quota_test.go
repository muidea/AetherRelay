package biz

import (
	"bytes"
	"context"
	"encoding/base64"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	basebiz "aetherrelay/internal/modules/base/biz"
	accountbiz "aetherrelay/internal/modules/blocks/codexaccountpool/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	acc "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	up "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	configcommon "aetherrelay/internal/modules/blocks/configruntime/pkg/common"
	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelaycredential"
	"aetherrelay/internal/pkg/modeltrace"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

// Exercise the real account owner and durable Store through their public
// contracts. Only upstream execution is simulated; two emitted merge events
// alone do not prove that the newer window replaced the older one.
func TestModelTraceQuotaObservationsReachStore(t *testing.T) {
	t.Setenv(aetherrelaycredential.EnvironmentKey, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	hub := event.NewHub(32)
	bg := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer bg.Shutdown(context.Background())
	dir := t.TempDir()
	cfg := event.NewSimpleObserver(configcommon.UnitID, hub)
	if err := cfg.Subscribe(configevents.TopicBootstrap, func(_ event.Event, r event.Result) {
		r.Set(configevents.BootstrapResult{Bootstrap: configevents.Bootstrap{Config: config.Config{
			State: config.StateConfig{Dir: dir, Database: filepath.Join(dir, "state.duckdb"), MemoryLimit: "256MB", Threads: 1},
		}}}, nil)
	}); err != nil {
		t.Fatal(err)
	}
	account, err := accountbiz.New(context.Background(), hub, bg)
	if err != nil {
		t.Fatal(err)
	}
	defer account.Teardown(context.Background())
	p := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, bg)}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := p.Quiesce(ctx); err != nil {
			t.Error(err)
		}
	}()
	call := func(topic string, data any) any {
		t.Helper()
		v, err := hub.Send(event.NewEventWithContext(topic, p.ID(), acccommon.UnitID, event.NewHeader(), context.Background(), data)).Get()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	imported := call(acc.TopicImport, acc.ImportCommand{Accounts: []acc.CredentialInput{{AccessToken: "fixture-access", RefreshToken: "fixture-refresh"}}}).(acc.ImportResult)
	id := imported.AccountIDs[0]
	call(acc.TopicPutModelSnapshot, acc.PutModelSnapshotCommand{AccountID: id, Snapshot: acc.AccountModelSnapshot{
		Models: []acc.AccountModelEntry{{ID: "gpt-test"}}, ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	}})
	base := time.Now().UTC().Add(-30 * time.Second).Truncate(time.Second)
	var mu sync.Mutex
	starts := 0
	snapshots := map[string]up.SampleSnapshotResult{}
	u := event.NewSimpleObserver(upcommon.UnitID, hub)
	subscribe := func(topic string, fn event.ObserverFunc) {
		t.Helper()
		if err := u.Subscribe(topic, fn); err != nil {
			t.Fatal(err)
		}
	}
	subscribe(up.TopicSampleStart, func(ev event.Event, r event.Result) {
		cmd := ev.Data().(up.SampleStartCommand)
		mu.Lock()
		defer mu.Unlock()
		starts++
		started := base.Add(time.Duration(starts) * 5 * time.Second)
		snapshots[cmd.ProbeID] = up.SampleSnapshotResult{
			Done: true, RequestStartedAt: started, ResponseReceivedAt: started.Add(2 * time.Second),
			Body: []byte(`{"status":"completed"}`), HTTPStatus: 200,
			Headers: []up.Header{
				{Name: "x-codex-primary-used-percent", Value: strconv.Itoa(98 + starts)},
				{Name: "x-codex-primary-reset-after-seconds", Value: "600"},
			},
		}
		r.Set(up.SampleStartResult{ProbeID: cmd.ProbeID}, nil)
	})
	subscribe(up.TopicSampleSnapshot, func(ev event.Event, r event.Result) {
		mu.Lock()
		defer mu.Unlock()
		r.Set(snapshots[ev.Data().(up.SampleSnapshotCommand).ProbeID].Clone(), nil)
	})
	subscribe(up.TopicSampleClose, func(ev event.Event, r event.Result) {
		mu.Lock()
		defer mu.Unlock()
		id := ev.Data().(up.SampleCloseCommand).ProbeID
		r.Set(up.SampleCloseResult{Closed: true, Snapshot: snapshots[id].Clone()}, nil)
		delete(snapshots, id)
	})
	challenges, e := modeltrace.GenerateChallenges(nil)
	if e != nil {
		t.Fatal(e)
	}
	started := p.startModelTraceProbe(context.Background(), proxyevents.StartModelTraceProbeCommand{
		SlotID: id, Model: "gpt-test", ProbeID: "quota-test", IdempotencyKey: "quota-test", Challenges: challenges,
	})
	if started.Status != 202 {
		t.Fatal(started)
	}
	saw99 := false
	deadline := time.Now().Add(3 * time.Second)
	for {
		p.tickModelTrace()
		view := call(acc.TopicList, acc.ListCommand{}).(acc.ListResult).Items[0]
		if view.UsageSnapshot != nil && len(view.UsageSnapshot.Windows) > 0 {
			w := view.UsageSnapshot.Windows[0]
			saw99 = saw99 || w.UsedPercent == 99
			if w.UsedPercent == 100 {
				if !saw99 || !w.UsedPercentKnown {
					t.Fatal("did not preserve the 99% then 100% observations", w)
				}
				if w.ObservedAt != base.Add(10*time.Second).Format(time.RFC3339Nano) || w.ResetAt != base.Add(612*time.Second).Format(time.RFC3339) {
					t.Fatal("window must use attempt start for ordering and header receipt for reset", w)
				}
				admission, err := p.probeAdmission(context.Background(), acc.ProbeAdmissionCommand{AccountID: id, Model: "gpt-test"})
				if err != nil || admission.Allowed || admission.Reason != "usage_limit" {
					t.Fatal("latest exhausted quota did not block admission", admission, err)
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("newer 100% quota was not merged into Store")
		}
		time.Sleep(time.Millisecond)
	}
}
