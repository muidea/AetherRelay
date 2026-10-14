package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/pkg/aetherrelayclientauth"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelayusage"
)

func TestGenerateClientKeyHint(t *testing.T) {
	key, hash, hint, err := generateClientKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 46 || !strings.HasPrefix(key, "sk_") {
		t.Fatalf("unexpected key format/length")
	}
	if want := key[:5] + "****" + key[len(key)-2:]; hint != want || len(hint) != 11 {
		t.Fatalf("unexpected hint: %q", hint)
	}
	digest := sha256.Sum256([]byte(key))
	if hash != "sha256:"+hex.EncodeToString(digest[:]) {
		t.Fatal("hint changed credential digest")
	}
}

func TestClientAPIKeyHintAdminLifecycle(t *testing.T) {
	store := usage.NewMemoryStore()
	runtime := &testRuntime{}
	handler := NewHandlerWithUsage("", runtime, store)
	ctx := context.Background()
	for _, id := range []string{"legacy", config.BuiltinClientAPIKeyID} {
		if err := store.EnsureClientAPIKey(ctx, id, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	request := func(method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/admin/api/client-api-keys"+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("X-AetherRelay-Admin", "1")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s status=%d body=%s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	readSecret := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		var result struct {
			APIKey string `json:"api_key"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.APIKey) != 46 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("one-time secret response contract changed")
		}
		return result.APIKey
	}
	assertList := func(secret string) {
		t.Helper()
		w := request(http.MethodGet, "", "", http.StatusOK)
		var result struct {
			Keys []clientKeyView `json:"client_api_keys"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, key := range result.Keys {
			if key.ID == "hint-key" {
				found = true
				if key.Hint != secret[:5]+"****"+secret[len(secret)-2:] {
					t.Fatalf("hint mismatch: %q", key.Hint)
				}
			} else if key.Hint != "" {
				t.Fatalf("metadata-only key has hint: %q", key.ID)
			}
		}
		if !found || strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), "sha256:") || strings.Contains(w.Body.String(), `"api_key"`) {
			t.Fatal("list missing key or leaked credential")
		}
		keys, err := store.ListClientAPIKeys(ctx)
		if err != nil || keys["hint-key"].Hint != secret[:5]+"****"+secret[len(secret)-2:] {
			t.Fatal("hint was not persisted")
		}
	}
	secret := readSecret(request(http.MethodPost, "", `{"id":"hint-key","provider_access":{"mode":"all","provider_ids":[]}}`, http.StatusCreated))
	assertList(secret)
	request(http.MethodPatch, "/hint-key", `{"enabled":false}`, http.StatusOK)
	assertList(secret)
	request(http.MethodPut, "/hint-key/provider-access", `{"mode":"all","provider_ids":[]}`, http.StatusOK)
	assertList(secret)
	rotated := readSecret(request(http.MethodPost, "/hint-key/rotate", "", http.StatusOK))
	assertList(rotated)
	for _, tc := range []struct {
		secret string
		valid  bool
	}{{secret, false}, {rotated, true}, {rotated[:5] + "****" + rotated[len(rotated)-2:], false}} {
		_, err := clientauth.ResolveHeaders(http.Header{"Authorization": []string{"Bearer " + tc.secret}}, runtime.keyIndex)
		if (err == nil) != tc.valid {
			t.Fatalf("authentication accepted old secret/hint or rejected rotated key: valid=%v err=%v", tc.valid, err)
		}
	}
}
