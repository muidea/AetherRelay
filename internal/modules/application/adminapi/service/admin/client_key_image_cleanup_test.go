package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	adminbiz "aetherrelay/internal/modules/application/adminapi/biz"
	admincommon "aetherrelay/internal/modules/application/adminapi/pkg/common"
	taskcommon "aetherrelay/internal/modules/application/chatgptimagetask/pkg/common"
	taskevents "aetherrelay/internal/modules/application/chatgptimagetask/pkg/events"
	basebiz "aetherrelay/internal/modules/base/biz"
	imagecommon "aetherrelay/internal/modules/blocks/chatgptimagestore/pkg/common"
	imageevents "aetherrelay/internal/modules/blocks/chatgptimagestore/pkg/events"
	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelayusage"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

// Use the production EventHub adapter: a stub returning untyped nil would
// conceal a nil *cd.Error converted to a non-nil error interface.
func TestDeleteClientKeyThroughImageCleanupAdapter(t *testing.T) {
	for _, stage := range []string{"success", "tasks", "assets"} {
		t.Run(stage, func(t *testing.T) {
			hub := event.NewHub(8)
			defer hub.Terminate(context.Background())
			failureStage := stage
			var taskCalls, assetCalls int
			tasks := event.NewSimpleObserver(taskcommon.UnitID, hub)
			tasks.Subscribe(taskevents.TopicDeleteOwner, func(ev event.Event, result event.Result) {
				taskCalls++
				if ev.Data().(taskevents.DeleteOwnerCommand).OwnerID != "test-office" {
					t.Error("incorrect task owner")
				}
				if failureStage == "tasks" {
					result.Set(nil, cd.NewError(cd.Unexpected, "task cleanup failed"))
					return
				}
				result.Set(taskevents.DeleteOwnerResult{}, nil)
			})
			images := event.NewSimpleObserver(imagecommon.UnitID, hub)
			images.Subscribe(imageevents.TopicDeleteScope, func(ev event.Event, result event.Result) {
				assetCalls++
				if ev.Data().(imageevents.DeleteScopeCommand).APIKeyID != "test-office" {
					t.Error("incorrect image scope")
				}
				if failureStage == "assets" {
					result.Set(nil, cd.NewError(cd.Unexpected, "image cleanup failed"))
					return
				}
				result.Set(imageevents.DeleteScopeResult{}, nil)
			})
			store := usage.NewMemoryStore()
			defer store.Close()
			if err := store.CreateClientAPIKey(context.Background(), usage.ClientAPIKeyRecord{ID: "test-office", Hash: "sha256:test", Enabled: true, CreatedAt: time.Now().UTC(), ProviderAccess: clientaccess.All()}); err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{InteractionDir: t.TempDir(), RequestTimeout: time.Second}
			scope := filepath.Join(cfg.InteractionDir, "test-office")
			if err := os.MkdirAll(scope, 0o700); err != nil {
				t.Fatal(err)
			}
			handler := NewHandlerWithUsage("", &testRuntime{cfg: cfg}, store)
			handler.chatGPT = &adminbiz.Admin{Base: basebiz.New(admincommon.UnitID, hub, nil)}
			remove := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodDelete, "/admin/api/client-api-keys/test-office", nil)
				req.RemoteAddr = "127.0.0.1:1234"
				req.Header.Set("X-AetherRelay-Admin", "1")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				return response
			}
			response := remove()
			if stage != "success" {
				want := http.StatusServiceUnavailable
				if stage == "assets" {
					want = http.StatusInternalServerError
				}
				if response.Code != want {
					t.Fatalf("failed cleanup = %d %s; want %d", response.Code, response.Body.String(), want)
				}
				keys, err := store.ListClientAPIKeys(context.Background())
				if err != nil || keys["test-office"].Enabled || keys["test-office"].DeletingAt == nil {
					t.Fatalf("failed deletion state=%+v err=%v", keys["test-office"], err)
				}
				if _, err := os.Stat(scope); err != nil {
					t.Fatalf("archives removed before image cleanup: %v", err)
				}
				if stage == "tasks" && assetCalls != 0 {
					t.Fatal("asset cleanup proceeded after task cleanup failed")
				}
				failureStage = "success"
				response = remove()
			}
			if response.Code != http.StatusNoContent {
				t.Fatalf("successful delete = %d %s", response.Code, response.Body.String())
			}
			keys, err := store.ListClientAPIKeys(context.Background())
			if _, exists := keys["test-office"]; err != nil || exists {
				t.Fatalf("key remains after cleanup: %+v err=%v", keys, err)
			}
			if _, err := os.Stat(scope); !os.IsNotExist(err) {
				t.Fatalf("archive scope remains: %v", err)
			}
			if taskCalls == 0 || assetCalls == 0 {
				t.Fatalf("missing cleanup: tasks=%d assets=%d", taskCalls, assetCalls)
			}
		})
	}
}
