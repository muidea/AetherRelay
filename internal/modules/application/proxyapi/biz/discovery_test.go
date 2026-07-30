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
	codexcommon "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/common"
	codexevents "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/events"
	codexupcommon "ai-proxy/internal/modules/blocks/codexupstream/pkg/common"
	codexupevents "ai-proxy/internal/modules/blocks/codexupstream/pkg/events"
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
		{AccountID: "backed-off", AccessToken: "token-backoff", Proxy: "http://backed-off-proxy.invalid:8080", NeedsDiscovery: true, DiscoveryDue: false},
		{AccountID: "due", AccessToken: "token-due", Proxy: "http://due-proxy.invalid:8080", NeedsDiscovery: true, DiscoveryDue: true},
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
		if cmd.AccessToken == "token-due" && cmd.Proxy != "http://due-proxy.invalid:8080" {
			t.Fatalf("due account proxy=%q", cmd.Proxy)
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

func TestCodexDiscoveryUsesAccountScopedCredentialsAndSkipsBackoff(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	accounts := event.NewSimpleObserver(codexcommon.UnitID, hub)
	candidates := []codexevents.DiscoveryCandidate{
		{AccountID: "backed-off", AccessToken: "token-backoff", NeedsDiscovery: true, DiscoveryDue: false},
		{AccountID: "due", AccessToken: "token-due", AccountIDHeader: "upstream-account", Proxy: "http://due-proxy.invalid:8080", NeedsDiscovery: true, DiscoveryDue: true},
	}
	var mu sync.Mutex
	var stored []codexevents.PutModelSnapshotCommand
	accounts.Subscribe(codexevents.TopicListDiscoveryCandidates, func(_ event.Event, result event.Result) {
		result.Set(codexevents.ListDiscoveryCandidatesResult{Candidates: candidates}, nil)
	})
	accounts.Subscribe(codexevents.TopicPutModelSnapshot, func(ev event.Event, result event.Result) {
		command, ok := ev.Data().(codexevents.PutModelSnapshotCommand)
		if !ok {
			t.Fatalf("unexpected snapshot command: %#v", ev.Data())
		}
		mu.Lock()
		stored = append(stored, command)
		mu.Unlock()
		result.Set(codexevents.PutModelSnapshotResult{Version: 1, OK: true}, nil)
	})
	accounts.Subscribe(codexevents.TopicCatalogSnapshot, func(_ event.Event, result event.Result) {
		result.Set(codexevents.CatalogSnapshotResult{}, nil)
	})

	upstream := event.NewSimpleObserver(codexupcommon.UnitID, hub)
	requests := 0
	upstream.Subscribe(codexupevents.TopicListModels, func(ev event.Event, result event.Result) {
		command, ok := ev.Data().(codexupevents.ListModelsCommand)
		if !ok {
			t.Fatalf("unexpected list models command: %#v", ev.Data())
		}
		if command.AccessToken == "token-due" && (command.AccountIDHeader != "upstream-account" || command.Proxy != "http://due-proxy.invalid:8080") {
			t.Errorf("credential transport=%+v", command)
		}
		mu.Lock()
		requests++
		mu.Unlock()
		result.Set(codexupevents.ListModelsResult{Models: []codexupevents.ModelDescriptor{{ID: "gpt-5.2-codex", OwnedBy: "openai"}}}, nil)
	})

	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background), config: config.Config{CodexOAuth: config.CodexOAuthConfig{Enabled: true}}}
	proxy.runCodexDiscoveryRound(context.Background(), true)
	mu.Lock()
	if requests != 1 || len(stored) != 1 || stored[0].AccountID != "due" || len(stored[0].Snapshot.Models) != 1 {
		mu.Unlock()
		t.Fatalf("due discovery requests=%d snapshots=%+v", requests, stored)
	}
	requests = 0
	stored = nil
	mu.Unlock()
	proxy.runCodexDiscoveryRound(context.Background(), false)
	mu.Lock()
	defer mu.Unlock()
	if requests != 2 || len(stored) != 2 {
		t.Fatalf("full discovery requests=%d snapshots=%+v", requests, stored)
	}
}
