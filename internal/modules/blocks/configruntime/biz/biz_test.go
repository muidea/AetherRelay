package biz

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	basebiz "ai-proxy/internal/modules/base/biz"
	"ai-proxy/internal/modules/blocks/configruntime/internal/providerstore"
	"ai-proxy/internal/modules/blocks/configruntime/pkg/common"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	config "ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxycredential"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

func testProvider(name, key string) config.Provider {
	provider := config.Provider{Name: name, Protocol: "openai", BaseURL: "https://example.com/v1", APIKey: key, Models: []string{"model"}, Endpoints: []string{config.ProviderEndpointChatCompletions}}
	config.ConfigureProviderPolicy(&provider, 100, true)
	return provider
}

func testConfig(providers map[string]config.Provider) config.Config {
	return config.Config{
		State: config.StateConfig{MemoryLimit: "256MB", Threads: 1},
		ChatGPTWeb: config.ChatGPTWebConfig{TemporaryChat: config.TemporaryChatConfig{
			RetentionDays: 1, MaxConversations: 1, MaxMessagesPerConversation: 1, MaxMessageBytes: 1, TurnTimeoutSeconds: 1,
		}},
		AdminAuth: config.AdminAuthConfig{BasePath: "/admin", DefaultLanguage: "zh-CN", SessionTTLSeconds: config.DefaultAdminSessionTTLSeconds},
		Providers: providers,
	}
}

func testCredentialCodec(t *testing.T) *aiproxycredential.Codec {
	t.Helper()
	codec, err := aiproxycredential.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func proxyUpdateObserver(hub event.Hub, failure error, captured *config.Config) event.SimpleObserver {
	observer := event.NewSimpleObserver("aiproxy.proxy.module", hub)
	observer.Subscribe(proxyevents.TopicUpdateConfig, func(ev event.Event, result event.Result) {
		if command, ok := ev.Data().(proxyevents.UpdateConfigCommand); ok && captured != nil {
			*captured = command.Config
		}
		if failure != nil {
			result.Set(nil, cd.NewError(cd.Unexpected, failure.Error()))
			return
		}
		result.Set(struct{}{}, nil)
	})
	return observer
}

func TestValidateHotReloadAllowsBuiltinRoutingPolicy(t *testing.T) {
	current := config.Config{
		ChatGPTWeb: config.ChatGPTWebConfig{ProviderEnabled: true},
		CodexOAuth: config.CodexOAuthConfig{ProviderEnabled: true},
	}
	next := current
	next.ChatGPTWeb.ProviderEnabled = false
	next.ChatGPTWeb.Priority = 5
	next.CodexOAuth.ProviderEnabled = false
	next.CodexOAuth.Priority = 120
	if err := validateHotReload(current, next); err != nil {
		t.Fatalf("builtin routing policy should hot reload: %v", err)
	}
}

func TestActivatePreservesManagedProviders(t *testing.T) {
	hub := event.NewHub(8)
	defer hub.Terminate(context.Background())
	current := testConfig(map[string]config.Provider{"managed": testProvider("managed", "secret")})
	var activated config.Config
	_ = proxyUpdateObserver(hub, nil, &activated)
	runtime := &ConfigRuntime{Base: basebiz.New(common.UnitID, hub, nil), bootstrap: configevents.Bootstrap{Config: current}, managedProviders: true}
	next := current
	next.Providers = map[string]config.Provider{"yaml": testProvider("yaml", "wrong")}
	ev := event.NewEventWithContext(configevents.TopicActivate, "test", common.UnitID, event.NewHeader(), context.Background(), configevents.ActivateCommand{Config: next})
	result := event.NewResult(ev.ID(), ev.Source(), ev.Destination())
	runtime.handleActivate(ev, result)
	if err := result.Error(); err != nil {
		t.Fatalf("activate: %s", err.Message)
	}
	if _, ok := activated.Providers["managed"]; !ok || len(activated.Providers) != 1 {
		t.Fatalf("activated providers=%+v", activated.Providers)
	}
}

func TestReplaceProvidersRollsBackStoreWhenActivationFails(t *testing.T) {
	hub := event.NewHub(8)
	defer hub.Terminate(context.Background())
	_ = proxyUpdateObserver(hub, errors.New("rejected"), nil)
	store, err := providerstore.Open(filepath.Join(t.TempDir(), "ai-proxy.duckdb"), "256MB", 1, testCredentialCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	current := testConfig(map[string]config.Provider{"current": testProvider("current", "current-key")})
	if err := store.Replace(current.Providers); err != nil {
		t.Fatal(err)
	}
	runtime := &ConfigRuntime{Base: basebiz.New(common.UnitID, hub, nil), bootstrap: configevents.Bootstrap{Config: current}, providers: store, managedProviders: true}
	replacement := map[string]config.Provider{"replacement": testProvider("replacement", "replacement-key")}
	ev := event.NewEventWithContext(configevents.TopicReplaceProviders, "test", common.UnitID, event.NewHeader(), context.Background(), configevents.ReplaceProvidersCommand{Providers: replacement})
	result := event.NewResult(ev.ID(), ev.Source(), ev.Destination())
	runtime.handleReplaceProviders(ev, result)
	if err := result.Error(); err == nil || !strings.Contains(err.Message, "rejected") {
		t.Fatalf("replace error=%v", err)
	}
	stored, initialized, err := store.Load()
	if err != nil || !initialized {
		t.Fatalf("load rollback: initialized=%t err=%v", initialized, err)
	}
	if stored["current"].APIKey != "current-key" || len(stored) != 1 {
		t.Fatalf("stored providers after rollback=%+v", stored)
	}
}
