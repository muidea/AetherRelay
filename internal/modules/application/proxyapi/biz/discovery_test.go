package biz

import (
	"context"
	"sync"
	"testing"

	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	basebiz "ai-proxy/internal/modules/base/biz"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
	config "ai-proxy/internal/pkg/aiproxyconfig"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestDueDiscoverySkipsBackedOffAccounts(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	var mu sync.Mutex
	modelRequests := map[string]int{}
	accountCandidates := []accevents.DiscoveryCandidate{
		{AccountID: "backed-off", AccessToken: "token-backoff", NeedsDiscovery: true, DiscoveryDue: false},
		{AccountID: "due", AccessToken: "token-due", NeedsDiscovery: true, DiscoveryDue: true},
	}
	accounts.Subscribe(accevents.TopicListDiscoveryCandidates, func(_ event.Event, result event.Result) {
		result.Set(accevents.ListDiscoveryCandidatesResult{Candidates: accountCandidates}, nil)
	})
	accounts.Subscribe(accevents.TopicPutModelSnapshot, func(_ event.Event, result event.Result) {
		result.Set(accevents.PutModelSnapshotResult{OK: true}, nil)
	})
	accounts.Subscribe(accevents.TopicCatalogSnapshot, func(_ event.Event, result event.Result) {
		result.Set(accevents.CatalogSnapshotResult{}, nil)
	})

	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstream.Subscribe(upevents.TopicListModels, func(ev event.Event, result event.Result) {
		cmd, ok := ev.Data().(upevents.ListModelsCommand)
		if !ok {
			t.Fatalf("unexpected list models command: %#v", ev.Data())
		}
		mu.Lock()
		modelRequests[cmd.AccessToken]++
		mu.Unlock()
		result.Set(upevents.ListModelsResult{Models: []upevents.ModelDescriptor{{
			ID: "gpt-5", Operations: []upevents.ModelOperation{upevents.ModelOperationChatCompletions},
		}}}, nil)
	})

	proxy := &Proxy{
		Base:   basebiz.New(proxycommon.UnitID, hub, background),
		config: config.Config{ChatGPTWeb: config.ChatGPTWebConfig{Enabled: true}},
	}
	proxy.runDiscoveryRound(context.Background(), true)
	mu.Lock()
	if modelRequests["token-due"] != 1 || modelRequests["token-backoff"] != 0 {
		t.Fatalf("due discovery requests=%v", modelRequests)
	}
	modelRequests = map[string]int{}
	mu.Unlock()

	proxy.runDiscoveryRound(context.Background(), false)
	mu.Lock()
	defer mu.Unlock()
	if modelRequests["token-due"] != 1 || modelRequests["token-backoff"] != 1 {
		t.Fatalf("full discovery requests=%v", modelRequests)
	}
}
