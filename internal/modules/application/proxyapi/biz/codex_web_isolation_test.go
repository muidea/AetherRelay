package biz

import (
	"context"
	"fmt"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type blockedWebFeature struct {
	FeatureExecutor
	entered chan struct{}
	release chan struct{}
}

func (s *blockedWebFeature) ExecuteFeatureSearch(ctx context.Context, _ proxyevents.ExecuteFeatureSearchCommand) (proxyevents.ExecuteFeatureSearchResult, error) {
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return proxyevents.ExecuteFeatureSearchResult{}, fmt.Errorf("Web search finished")
}

// A Web search occupies the proxy owner's feature lane while consuming SSE.
// Normal Codex requests must still acquire, execute, release, and settle using
// their separate account/upstream lanes before that Web operation finishes.
func TestCodexCompletesWhileWebSearchHoldsProxyFeatureLane(t *testing.T) {
	hub, background := event.NewHub(16), task.NewBackgroundRoutine(8)
	t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
	feature := &blockedWebFeature{entered: make(chan struct{}), release: make(chan struct{})}
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background), featureExecutor: feature}
	proxy.SubscribeFunc(proxyevents.TopicExecuteFeatureSearch, proxy.handleExecuteFeatureSearch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	webDone := make(chan struct{})
	go func() {
		defer close(webDone)
		_, _ = hub.Send(event.NewEventWithContext(proxyevents.TopicExecuteFeatureSearch, "test", proxy.ID(), nil, ctx, proxyevents.ExecuteFeatureSearchCommand{})).Get()
	}()
	defer func() {
		close(feature.release)
		select {
		case <-webDone:
		case <-ctx.Done():
			t.Error("Web feature did not exit")
		}
	}()
	select {
	case <-feature.entered:
	case <-ctx.Done():
		t.Fatal("Web feature did not start")
	}
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	acquired, released, settled := 0, 0, 0
	accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, r event.Result) {
		acquired++
		r.Set(accevents.AcquireResult{AccountID: "account", AccessToken: "codex-token", LeaseID: "lease"}, nil)
	})
	accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, r event.Result) {
		released++
		r.Set(accevents.ReleaseResult{Released: true}, nil)
	})
	accounts.Subscribe(accevents.TopicRecordResult, func(ev event.Event, r event.Result) {
		cmd := ev.Data().(accevents.RecordResultCommand)
		if !cmd.Success {
			t.Error("Codex success penalized while Web feature blocked")
		}
		settled++
		r.Set(accevents.RecordResultResult{}, nil)
	})
	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicComplete, func(ev event.Event, r event.Result) {
		if ev.Data().(upevents.CompleteCommand).AccessToken != "codex-token" {
			t.Error("Codex upstream received wrong credential")
		}
		r.Set(upevents.CompleteResult{Body: []byte(`{"id":"codex-ok"}`)}, nil)
	})
	out, err := proxy.CompleteCodexResponses(ctx, codexresponses.Request{Model: "gpt-6-sol", Body: []byte(`{"model":"gpt-6-sol","input":[]}`)})
	if err != nil || string(out.Body) != `{"id":"codex-ok"}` || acquired != 1 || released != 1 || settled != 1 {
		t.Fatalf("Codex blocked or changed: err=%v, acquired=%d released=%d settled=%d", err, acquired, released, settled)
	}
	select {
	case <-webDone:
		t.Fatal("Web feature exited before Codex completion")
	default:
	}
}
