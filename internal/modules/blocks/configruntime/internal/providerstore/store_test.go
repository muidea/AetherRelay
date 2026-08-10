package providerstore

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelaycredential"
)

func codec(t *testing.T, fill byte) *aetherrelaycredential.Codec {
	t.Helper()
	value := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
	result, err := aetherrelaycredential.New(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestStoreEncryptsProviderCatalogAndRestoresIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	initialized, err := Initialized(path, "256MB", 1)
	if err != nil || initialized {
		t.Fatalf("new store initialized=%t err=%v", initialized, err)
	}
	store, err := Open(path, "256MB", 1, codec(t, 3))
	if err != nil {
		t.Fatal(err)
	}
	provider := config.Provider{Name: "openai", Protocol: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "provider-secret-value", Models: []string{"gpt-5"}, Endpoints: []string{config.ProviderEndpointResponses}}
	config.ConfigureProviderPolicy(&provider, 0, false)
	if err := store.Replace(map[string]config.Provider{"openai": provider}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	initialized, err = Initialized(path, "256MB", 1)
	if err != nil || !initialized {
		t.Fatalf("persisted store initialized=%t err=%v", initialized, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("provider-secret-value")) {
		t.Fatal("DuckDB file contains plaintext Provider credential")
	}

	store, err = Open(path, "256MB", 1, codec(t, 3))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	providers, initialized, err := store.Load()
	if err != nil || !initialized || providers["openai"].APIKey != "provider-secret-value" || config.EffectiveProviderPriority(providers["openai"]) != 0 {
		t.Fatalf("providers=%+v initialized=%t err=%v", providers, initialized, err)
	}
}

func TestStoreRejectsWrongCredentialKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aetherrelay.duckdb")
	store, err := Open(path, "256MB", 1, codec(t, 4))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Replace(map[string]config.Provider{}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	store, err = Open(path, "256MB", 1, codec(t, 5))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, err := store.Load(); err == nil {
		t.Fatal("wrong credential key was accepted")
	}
}
