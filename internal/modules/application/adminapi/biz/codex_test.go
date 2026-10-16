package biz

import (
	"context"
	"reflect"
	"testing"

	admincommon "aetherrelay/internal/modules/application/adminapi/pkg/common"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	basebiz "aetherrelay/internal/modules/base/biz"
	accountcommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accountevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestCodexBlacklistUpdateRefreshesPublishedDirectory(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(4)
	t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
	accounts := event.NewSimpleObserver(accountcommon.UnitID, hub)
	updated := false
	accounts.Subscribe(accountevents.TopicUpdate, func(e event.Event, r event.Result) {
		updated = true
		cmd := e.Data().(accountevents.UpdateCommand)
		r.Set(accountevents.UpdateResult{Item: accountevents.AccountView{ID: cmd.ID, BlockedModels: cmd.BlockedModels}}, nil)
	})
	proxy := event.NewSimpleObserver(proxycommon.UnitID, hub)
	refreshes := 0
	proxy.Subscribe(proxyevents.TopicEffectiveCatalog, func(e event.Event, r event.Result) {
		if !updated || !e.Data().(proxyevents.EffectiveCatalogCommand).Refresh {
			t.Error("catalog refreshed before policy save")
		}
		refreshes++
		r.Set(proxyevents.EffectiveCatalogResult{}, nil)
	})
	admin := &Admin{Base: basebiz.New(admincommon.UnitID, hub, background)}
	result, err := admin.UpdateCodexAccount(context.Background(), accountevents.UpdateCommand{ID: "slot", BlockedModels: accountevents.ModelBlacklist{"gpt-sol"}})
	if err != nil || refreshes != 1 || result.CatalogRefreshError != "" {
		t.Fatalf("result=%+v refreshes=%d err=%v", result, refreshes, err)
	}
	limit := 3
	if _, err = admin.UpdateCodexAccount(context.Background(), accountevents.UpdateCommand{ID: "slot", MaxConcurrency: &limit}); err != nil || refreshes != 1 {
		t.Fatal("unrelated settings refreshed model directory", err)
	}
}

func TestCodexAccountMutationsStartProxyOwnedDiscovery(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() {
		background.Shutdown(nil)
		hub.Terminate(context.Background())
	})
	accounts := event.NewSimpleObserver(accountcommon.UnitID, hub)
	accounts.Subscribe(accountevents.TopicImport, func(_ event.Event, result event.Result) {
		result.Set(accountevents.ImportResult{Added: 1, AccountIDs: []string{"import-account"}}, nil)
	})
	accounts.Subscribe(accountevents.TopicRefreshByID, func(_ event.Event, result event.Result) {
		result.Set(accountevents.RefreshByIDResult{Refreshed: 1}, nil)
	})
	accounts.Subscribe(accountevents.TopicOAuthFinish, func(_ event.Event, result event.Result) {
		result.Set(accountevents.OAuthFinishResult{Added: true, Item: accountevents.AccountView{ID: "oauth-account"}}, nil)
	})
	proxy := event.NewSimpleObserver(proxycommon.UnitID, hub)
	var discovered [][]string
	var usageRefreshes [][]string
	proxy.Subscribe(proxyevents.TopicStartCodexDiscovery, func(ev event.Event, result event.Result) {
		command := ev.Data().(proxyevents.StartCodexDiscoveryCommand)
		discovered = append(discovered, append([]string(nil), command.AccountIDs...))
		result.Set(proxyevents.StartCodexDiscoveryResult{Progress: proxyevents.CodexDiscoveryProgress{ProgressID: "progress"}}, nil)
	})
	proxy.Subscribe(proxyevents.TopicStartCodexUsageRefresh, func(ev event.Event, result event.Result) {
		command := ev.Data().(proxyevents.StartCodexUsageRefreshCommand)
		usageRefreshes = append(usageRefreshes, append([]string(nil), command.AccountIDs...))
		result.Set(proxyevents.StartCodexUsageRefreshResult{Progress: proxyevents.CodexUsageProgress{ProgressID: "usage-progress"}}, nil)
	})
	admin := &Admin{Base: basebiz.New(admincommon.UnitID, hub, background)}
	imported, err := admin.ImportCodexAccounts(context.Background(), []accountevents.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}})
	if err != nil || imported.Added != 1 || imported.ModelDiscovery == nil || imported.UsageRefresh == nil {
		t.Fatalf("imported=%+v err=%v", imported, err)
	}
	refreshed, err := admin.RefreshCodexAccounts(context.Background(), []string{"refresh-account"})
	if err != nil || refreshed.Refreshed != 1 || refreshed.ModelDiscovery == nil || refreshed.UsageRefresh == nil {
		t.Fatalf("refreshed=%+v err=%v", refreshed, err)
	}
	finished, err := admin.FinishCodexOAuth(context.Background(), "session", "callback")
	if err != nil || !finished.Added || finished.ModelDiscovery == nil || finished.UsageRefresh == nil {
		t.Fatalf("finished=%+v err=%v", finished, err)
	}
	if !reflect.DeepEqual(discovered, [][]string{{"import-account"}, {"refresh-account"}, {"oauth-account"}}) {
		t.Fatalf("discovery account IDs=%v", discovered)
	}
	if !reflect.DeepEqual(usageRefreshes, [][]string{{"import-account"}, {"refresh-account"}, {"oauth-account"}}) {
		t.Fatalf("usage account IDs=%v", usageRefreshes)
	}
}

func TestUnchangedCodexImportDoesNotStartFullPoolRefresh(t *testing.T) {
	hub := event.NewHub(8)
	background := task.NewBackgroundRoutine(2)
	t.Cleanup(func() {
		background.Shutdown(nil)
		hub.Terminate(context.Background())
	})
	accounts := event.NewSimpleObserver(accountcommon.UnitID, hub)
	accounts.Subscribe(accountevents.TopicImport, func(_ event.Event, result event.Result) {
		result.Set(accountevents.ImportResult{Skipped: 1}, nil)
	})
	proxy := event.NewSimpleObserver(proxycommon.UnitID, hub)
	refreshes := 0
	proxy.Subscribe(proxyevents.TopicStartCodexDiscovery, func(_ event.Event, result event.Result) {
		refreshes++
		result.Set(proxyevents.StartCodexDiscoveryResult{}, nil)
	})
	proxy.Subscribe(proxyevents.TopicStartCodexUsageRefresh, func(_ event.Event, result event.Result) {
		refreshes++
		result.Set(proxyevents.StartCodexUsageRefreshResult{}, nil)
	})
	admin := &Admin{Base: basebiz.New(admincommon.UnitID, hub, background)}
	imported, err := admin.ImportCodexAccounts(context.Background(), []accountevents.CredentialInput{{AccessToken: "access", RefreshToken: "refresh"}})
	if err != nil || imported.Skipped != 1 || imported.ModelDiscovery != nil || imported.UsageRefresh != nil {
		t.Fatalf("imported=%+v err=%v", imported, err)
	}
	if refreshes != 0 {
		t.Fatalf("unchanged import started %d full-pool refreshes", refreshes)
	}
}
