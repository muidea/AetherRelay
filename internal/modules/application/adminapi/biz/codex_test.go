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

func TestCodexAccountMutationsStartProxyOwnedDiscovery(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() {
		background.Shutdown(nil)
		hub.Terminate(context.Background())
	})
	accounts := event.NewSimpleObserver(accountcommon.UnitID, hub)
	accounts.Subscribe(accountevents.TopicImport, func(_ event.Event, result event.Result) {
		result.Set(accountevents.ImportResult{Added: 1}, nil)
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
	if !reflect.DeepEqual(discovered, [][]string{nil, {"refresh-account"}, {"oauth-account"}}) {
		t.Fatalf("discovery account IDs=%v", discovered)
	}
	if !reflect.DeepEqual(usageRefreshes, [][]string{nil, {"refresh-account"}, {"oauth-account"}}) {
		t.Fatalf("usage account IDs=%v", usageRefreshes)
	}
}
