package biz

import (
	"context"
	"sync"
	"testing"
	"time"

	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
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
			ID: "gpt-5", Capabilities: []upevents.ModelCapability{upevents.ModelCapabilityTextGeneration},
		}}}, nil)
	})

	proxy := &Proxy{
		Base:   basebiz.New(proxycommon.UnitID, hub, background),
		config: config.Config{ChatGPTWeb: config.ChatGPTWebConfig{}},
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

	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background), config: config.Config{CodexOAuth: config.CodexOAuthConfig{}}}
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

func TestManualCodexDiscoveryReportsAccountScopedProgress(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() {
		background.Shutdown(nil)
		hub.Terminate(context.Background())
	})
	accounts := event.NewSimpleObserver(codexcommon.UnitID, hub)
	accounts.Subscribe(codexevents.TopicListDiscoveryCandidates, func(_ event.Event, result event.Result) {
		result.Set(codexevents.ListDiscoveryCandidatesResult{Candidates: []codexevents.DiscoveryCandidate{{AccountID: "account-1", AccessToken: "token-1"}}}, nil)
	})
	accounts.Subscribe(codexevents.TopicPutModelSnapshot, func(_ event.Event, result event.Result) {
		result.Set(codexevents.PutModelSnapshotResult{OK: true}, nil)
	})
	accounts.Subscribe(codexevents.TopicCatalogSnapshot, func(_ event.Event, result event.Result) {
		result.Set(codexevents.CatalogSnapshotResult{}, nil)
	})
	upstream := event.NewSimpleObserver(codexupcommon.UnitID, hub)
	upstream.Subscribe(codexupevents.TopicListModels, func(_ event.Event, result event.Result) {
		result.Set(codexupevents.ListModelsResult{Models: []codexupevents.ModelDescriptor{{ID: "gpt-5.2-codex"}}}, nil)
	})
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background), config: config.Config{CodexOAuth: config.CodexOAuthConfig{}}}
	started, err := proxy.StartCodexModelDiscovery(context.Background(), []string{"account-1", "account-1"})
	if err != nil || started.ProgressID == "" || started.Done {
		t.Fatalf("start=%+v err=%v", started, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		progress, found := proxy.CodexModelDiscoveryProgress(started.ProgressID)
		if !found {
			t.Fatal("manual discovery progress disappeared")
		}
		if progress.Done {
			if progress.Total != 1 || progress.Processed != 1 || progress.Succeeded != 1 || progress.Failed != 0 || progress.LastError != "" || progress.CompletedAt == "" {
				t.Fatalf("progress=%+v", progress)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("manual discovery did not complete: %+v", progress)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestManualCodexUsageRefreshUsesAccountScopedCredentials(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() {
		background.Shutdown(nil)
		hub.Terminate(context.Background())
	})
	accounts := event.NewSimpleObserver(codexcommon.UnitID, hub)
	accounts.Subscribe(codexevents.TopicListUsageCandidates, func(ev event.Event, result event.Result) {
		command, ok := ev.Data().(codexevents.ListUsageCandidatesCommand)
		if !ok || len(command.AccountIDs) != 1 || command.AccountIDs[0] != "account-1" {
			t.Fatalf("usage candidates command=%#v", ev.Data())
		}
		result.Set(codexevents.ListUsageCandidatesResult{Candidates: []codexevents.UsageCandidate{{
			AccountID: "account-1", AccessToken: "token-1", AccountIDHeader: "upstream-account", Proxy: "http://proxy.invalid:8080",
		}}}, nil)
	})
	var snapshots []codexevents.PutUsageSnapshotCommand
	accounts.Subscribe(codexevents.TopicPutUsageSnapshot, func(ev event.Event, result event.Result) {
		command, ok := ev.Data().(codexevents.PutUsageSnapshotCommand)
		if !ok {
			t.Fatalf("usage snapshot command=%#v", ev.Data())
		}
		snapshots = append(snapshots, command)
		result.Set(codexevents.PutUsageSnapshotResult{OK: true}, nil)
	})
	accounts.Subscribe(codexevents.TopicRecordUsageFailure, func(_ event.Event, result event.Result) {
		result.Set(codexevents.RecordUsageFailureResult{OK: true}, nil)
	})
	upstream := event.NewSimpleObserver(codexupcommon.UnitID, hub)
	upstream.Subscribe(codexupevents.TopicGetUsage, func(ev event.Event, result event.Result) {
		command, ok := ev.Data().(codexupevents.GetUsageCommand)
		if !ok || command.AccessToken != "token-1" || command.AccountIDHeader != "upstream-account" || command.Proxy != "http://proxy.invalid:8080" {
			t.Fatalf("usage command=%#v", ev.Data())
		}
		result.Set(codexupevents.GetUsageResult{PlanType: "pro", Windows: []codexupevents.UsageWindow{{
			ID: "standard-primary", UsedPercent: 40, UsedPercentKnown: true, WindowSeconds: 18000, Allowed: true, AllowedKnown: true,
		}}}, nil)
	})
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background), config: config.Config{CodexOAuth: config.CodexOAuthConfig{}}, codexUsageJobs: map[string]proxyevents.CodexUsageProgress{}}
	started, err := proxy.StartCodexUsageRefresh(context.Background(), []string{"account-1", "account-1"})
	if err != nil || started.ProgressID == "" || started.Done {
		t.Fatalf("start=%+v err=%v", started, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		progress, found := proxy.CodexUsageRefreshProgress(started.ProgressID)
		if !found {
			t.Fatal("usage progress disappeared")
		}
		if progress.Done {
			if progress.Total != 1 || progress.Processed != 1 || progress.Succeeded != 1 || progress.Failed != 0 || len(snapshots) != 1 {
				t.Fatalf("progress=%+v snapshots=%+v", progress, snapshots)
			}
			if snapshot := snapshots[0].Snapshot; snapshot.PlanType != "pro" || len(snapshot.Windows) != 1 || snapshot.Windows[0].UsedPercent != 40 || snapshot.ExpiresAt == "" {
				t.Fatalf("snapshot=%+v", snapshot)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("manual usage refresh did not complete: %+v", progress)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCodexUsageRefreshRetriesOnceAfterCredentialRefresh(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() {
		background.Shutdown(nil)
		hub.Terminate(context.Background())
	})
	accounts := event.NewSimpleObserver(codexcommon.UnitID, hub)
	accounts.Subscribe(codexevents.TopicListUsageCandidates, func(_ event.Event, result event.Result) {
		result.Set(codexevents.ListUsageCandidatesResult{Candidates: []codexevents.UsageCandidate{{AccountID: "account-1", AccessToken: "expired-token", AccountIDHeader: "old-account", Proxy: "http://old-proxy.invalid:8080"}}}, nil)
	})
	refreshes := 0
	accounts.Subscribe(codexevents.TopicRefreshToken, func(ev event.Event, result event.Result) {
		command, ok := ev.Data().(codexevents.RefreshTokenCommand)
		if !ok || command.AccountID != "account-1" {
			t.Fatalf("refresh command=%#v", ev.Data())
		}
		refreshes++
		result.Set(codexevents.RefreshTokenResult{AccountID: "account-1", AccessToken: "fresh-token", AccountIDHeader: "new-account", Proxy: "http://new-proxy.invalid:8080", Refreshed: true}, nil)
	})
	var snapshot codexevents.AccountUsageSnapshot
	accounts.Subscribe(codexevents.TopicPutUsageSnapshot, func(ev event.Event, result event.Result) {
		command := ev.Data().(codexevents.PutUsageSnapshotCommand)
		snapshot = command.Snapshot
		result.Set(codexevents.PutUsageSnapshotResult{OK: true}, nil)
	})
	accounts.Subscribe(codexevents.TopicRecordUsageFailure, func(_ event.Event, result event.Result) {
		result.Set(codexevents.RecordUsageFailureResult{OK: true}, nil)
	})
	upstream := event.NewSimpleObserver(codexupcommon.UnitID, hub)
	requests := 0
	upstream.Subscribe(codexupevents.TopicGetUsage, func(ev event.Event, result event.Result) {
		command := ev.Data().(codexupevents.GetUsageCommand)
		requests++
		switch requests {
		case 1:
			if command.AccessToken != "expired-token" || command.AccountIDHeader != "old-account" || command.Proxy != "http://old-proxy.invalid:8080" {
				t.Fatalf("initial usage command=%+v", command)
			}
			result.Set(codexupevents.GetUsageResult{ErrorClass: codexupevents.ErrorInvalidToken}, nil)
		case 2:
			if command.AccessToken != "fresh-token" || command.AccountIDHeader != "new-account" || command.Proxy != "http://new-proxy.invalid:8080" {
				t.Fatalf("retried usage command=%+v", command)
			}
			result.Set(codexupevents.GetUsageResult{Windows: []codexupevents.UsageWindow{{ID: "standard-primary", UsedPercent: 20, UsedPercentKnown: true}}}, nil)
		default:
			t.Fatalf("unexpected usage request count=%d", requests)
		}
	})
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background), config: config.Config{CodexOAuth: config.CodexOAuthConfig{}}, codexUsageJobs: map[string]proxyevents.CodexUsageProgress{}}
	started, err := proxy.StartCodexUsageRefresh(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		progress, found := proxy.CodexUsageRefreshProgress(started.ProgressID)
		if !found {
			t.Fatal("usage progress disappeared")
		}
		if progress.Done {
			if progress.Succeeded != 1 || progress.Failed != 0 || requests != 2 || refreshes != 1 || len(snapshot.Windows) != 1 {
				t.Fatalf("progress=%+v requests=%d refreshes=%d snapshot=%+v", progress, requests, refreshes, snapshot)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("credential refresh retry did not complete: %+v", progress)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
